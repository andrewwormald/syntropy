// Package openhands implements runner.Runner by managing a per-Run
// openhands-agent-server subprocess and driving it over HTTP. See ADR-0112
// for the design: a subprocess (not a shared server), the same prompt +
// decision-marker protocol as internal/runner/claude (ADR-0027), and
// confirmation policy forced to auto-approve so tool calls never pause for
// human confirmation.
//
// The Agent Server endpoint shapes assumed here (payload/response field
// names) are best-effort against OpenHands' published OpenAPI schema as of
// ADR-0112 — they have not been confirmed against a real running
// openhands-agent-server. Per this repo's local-test-gate practice, no
// tagged release should ship this adapter until that confirmation happens.
package openhands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/andrewwormald/syntropy/internal/runner"
	"github.com/andrewwormald/syntropy/internal/runner/claude"
)

// Execution status values from ConversationExecutionStatus (ADR-0112).
const (
	statusRunning           = "running"
	statusStopped           = "stopped"
	statusFinished          = "finished"
	statusError             = "error"
	statusPaused            = "paused"
	statusAwaitingUserInput = "awaiting_user_input"
)

// Runner implements runner.Runner. The zero value is usable (uses
// "openhands-agent-server" from $PATH with default timeouts). NewRunner is
// the canonical constructor.
type Runner struct {
	// Binary is the path to the openhands-agent-server executable (or a
	// wrapper script). Defaults to "openhands-agent-server".
	Binary string

	// ExtraArgs is appended to the subprocess argv, after the required
	// --host/--port flags.
	ExtraArgs []string

	// Env, if non-nil, replaces os.Environ() for the subprocess. nil
	// inherits the daemon's env.
	Env []string

	// PollInterval is how often Run polls conversation status. Defaults to
	// 2s if zero.
	PollInterval time.Duration

	// ReadyTimeout bounds how long Run waits for the server to report
	// ready before giving up. Defaults to 30s if zero.
	ReadyTimeout time.Duration

	// HTTPClient is used for all Agent Server calls. Defaults to a client
	// with a 30s per-request timeout if nil.
	HTTPClient *http.Client
}

// NewRunner constructs a Runner. All arguments are optional.
func NewRunner(binary string, extraArgs ...string) *Runner {
	if binary == "" {
		binary = "openhands-agent-server"
	}
	return &Runner{Binary: binary, ExtraArgs: extraArgs}
}

// Verify Runner satisfies runner.Runner at compile time.
var _ runner.Runner = (*Runner)(nil)

func (r *Runner) Name() string { return "openhands" }

func (r *Runner) Run(ctx context.Context, req runner.Request) (runner.Response, error) {
	start := time.Now()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	port, err := freePort()
	if err != nil {
		return runner.Response{StartedAt: start, EndedAt: time.Now()},
			fmt.Errorf("openhands: find free port: %w", err)
	}

	cmd, err := r.spawnServer(port, req.Worktree)
	if err != nil {
		return runner.Response{StartedAt: start, EndedAt: time.Now()},
			fmt.Errorf("openhands: spawn agent server: %w", err)
	}
	defer stopServer(cmd)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := r.waitReady(ctx, baseURL); err != nil {
		return runner.Response{StartedAt: start, EndedAt: time.Now()},
			fmt.Errorf("openhands: wait ready: %w", err)
	}

	resp, err := r.converse(ctx, baseURL, req)
	resp.StartedAt = start
	resp.EndedAt = time.Now()
	return resp, err
}

// converse creates a conversation against an already-ready Agent Server at
// baseURL, runs it, polls until it reaches a terminal execution status,
// reads the last MessageEvent, and parses it via claude.ParseDecision — the
// same marker protocol ADR-0027 defines, reused rather than reinvented
// (ADR-0112 §2). Split out from Run so the submit/poll/parse core can be
// unit-tested against a mock HTTP server without a real subprocess.
func (r *Runner) converse(ctx context.Context, baseURL string, req runner.Request) (runner.Response, error) {
	prompt := claude.BuildPrompt(req)

	convID, err := r.createConversation(ctx, baseURL, req, prompt)
	if err != nil {
		return runner.Response{}, fmt.Errorf("openhands: create conversation: %w", err)
	}

	if err := r.startConversation(ctx, baseURL, convID); err != nil {
		return runner.Response{}, fmt.Errorf("openhands: start conversation: %w", err)
	}

	status, err := r.pollUntilDone(ctx, baseURL, convID)
	if err != nil {
		return runner.Response{}, fmt.Errorf("openhands: poll conversation: %w", err)
	}

	// awaiting_user_input fires from the confirmation-policy mechanism,
	// which is disabled by createConversation's auto-approve policy. If we
	// see it anyway, it's a runner-level error, not a DecisionAsk — those
	// are two different concepts (ADR-0112 §3).
	if status == statusAwaitingUserInput {
		return runner.Response{}, fmt.Errorf("openhands: conversation %s paused awaiting_user_input; treated as a runner error, not DecisionAsk (ADR-0112 §3)", convID)
	}

	text, err := r.lastMessageText(ctx, baseURL, convID)
	if err != nil {
		return runner.Response{}, fmt.Errorf("openhands: fetch events: %w", err)
	}

	titleUpdate := claude.ParseTitleUpdate(text)
	descriptionUpdate := claude.ParseDescriptionUpdate(text)

	decision, summary, question, title, parseErr := claude.ParseDecision(text)
	if parseErr != nil {
		shown := strings.TrimSpace(text)
		const maxShown = 2000
		if len(shown) > maxShown {
			shown = shown[:maxShown] + "…"
		}
		return runner.Response{Summary: shown},
			fmt.Errorf("openhands: parse decision: %w; response was:\n%s", parseErr, shown)
	}

	return runner.Response{
		Decision:          decision,
		Summary:           summary,
		Question:          question,
		Title:             title,
		TitleUpdate:       titleUpdate,
		DescriptionUpdate: descriptionUpdate,
	}, nil
}

// --- subprocess lifecycle ---

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// spawnServer starts the Agent Server subprocess bound to 127.0.0.1:port,
// with its working directory set to workdir (ADR-0006's worktree remains the
// blast-radius boundary). It is a long-lived server process, not tied to any
// single request's context — callers must explicitly stopServer it.
func (r *Runner) spawnServer(port int, workdir string) (*exec.Cmd, error) {
	binary := r.Binary
	if binary == "" {
		binary = "openhands-agent-server"
	}
	args := append([]string{"--host", "127.0.0.1", "--port", strconv.Itoa(port)}, r.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	if r.Env != nil {
		cmd.Env = r.Env
	} else {
		cmd.Env = os.Environ()
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// stopServer tears down a subprocess started by spawnServer, tolerating a
// nil cmd or a process that already exited.
func stopServer(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// waitReady polls GET /ready until it returns 200 OK or ReadyTimeout elapses.
func (r *Runner) waitReady(ctx context.Context, baseURL string) error {
	timeout := r.ReadyTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := r.httpClient()
	for {
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ready", nil); err == nil {
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent server not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// --- Agent Server HTTP client ---

func (r *Runner) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// doJSON marshals body (if non-nil) as the request payload, sends it, and
// unmarshals a 2xx response body into out (if non-nil and non-empty).
func (r *Runner) doJSON(ctx context.Context, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.httpClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: unexpected status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response from %s %s: %w", method, url, err)
		}
	}
	return nil
}

// createConversationRequest is the payload for POST /api/conversations.
type createConversationRequest struct {
	Agent              agentConfig        `json:"agent"`
	Workspace          workspaceConfig    `json:"workspace"`
	InitialMessage     string             `json:"initial_message"`
	ConfirmationPolicy confirmationPolicy `json:"confirmation_policy"`
}

type agentConfig struct {
	LLM llmConfig `json:"llm"`
}

type llmConfig struct {
	Model string `json:"model,omitempty"`
}

type workspaceConfig struct {
	Kind       string `json:"kind"`
	WorkingDir string `json:"working_dir"`
}

// confirmationPolicy is set to never pause for human confirmation of tool
// calls — ADR-0112 §3, same reasoning as ADR-0027 §4's unconditional
// --dangerously-skip-permissions: inside the worktree, autonomous tool use
// is the accepted risk.
type confirmationPolicy struct {
	Kind string `json:"kind"`
}

type createConversationResponse struct {
	ID string `json:"id"`
}

func (r *Runner) createConversation(ctx context.Context, baseURL string, req runner.Request, initialMessage string) (string, error) {
	payload := createConversationRequest{
		Agent: agentConfig{LLM: llmConfig{Model: req.Model}},
		Workspace: workspaceConfig{
			Kind:       "LocalWorkspace",
			WorkingDir: req.Worktree,
		},
		InitialMessage:     initialMessage,
		ConfirmationPolicy: confirmationPolicy{Kind: "auto_approve"},
	}
	var out createConversationResponse
	if err := r.doJSON(ctx, http.MethodPost, baseURL+"/api/conversations", payload, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("create conversation returned empty id")
	}
	return out.ID, nil
}

func (r *Runner) startConversation(ctx context.Context, baseURL, convID string) error {
	return r.doJSON(ctx, http.MethodPost, baseURL+"/api/conversations/"+convID+"/run", nil, nil)
}

// conversationInfo is the response shape of GET /api/conversations/{id}.
type conversationInfo struct {
	ID              string `json:"id"`
	ExecutionStatus string `json:"execution_status"`
}

// pollUntilDone polls GET /api/conversations/{id} until execution_status
// reaches a terminal value (finished, error, awaiting_user_input, stopped),
// or ctx is done.
func (r *Runner) pollUntilDone(ctx context.Context, baseURL, convID string) (string, error) {
	interval := r.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	for {
		var info conversationInfo
		if err := r.doJSON(ctx, http.MethodGet, baseURL+"/api/conversations/"+convID, nil, &info); err != nil {
			return "", err
		}
		switch info.ExecutionStatus {
		case statusFinished, statusError, statusAwaitingUserInput, statusStopped:
			return info.ExecutionStatus, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// eventsSearchResponse is the response shape of GET
// /api/conversations/{id}/events/search.
type eventsSearchResponse struct {
	Items []eventEnvelope `json:"items"`
}

// eventEnvelope is one entry of the discriminated union GET
// .../events/search returns (ActionEvent, MessageEvent, ObservationEvent,
// ErrorEvent, ...). Only the MessageEvent shape is modelled here since it's
// the only kind this adapter needs to read from.
type eventEnvelope struct {
	Kind    string        `json:"kind"`
	Message *eventMessage `json:"message,omitempty"`
}

type eventMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (m *eventMessage) text() string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, c := range m.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// lastMessageText returns the text of the last MessageEvent with non-empty
// content — the agent's final freeform reply, per ADR-0112.
func (r *Runner) lastMessageText(ctx context.Context, baseURL, convID string) (string, error) {
	var out eventsSearchResponse
	if err := r.doJSON(ctx, http.MethodGet, baseURL+"/api/conversations/"+convID+"/events/search", nil, &out); err != nil {
		return "", err
	}
	for i := len(out.Items) - 1; i >= 0; i-- {
		if out.Items[i].Kind != "MessageEvent" {
			continue
		}
		if text := out.Items[i].Message.text(); text != "" {
			return text, nil
		}
	}
	return "", errors.New("no MessageEvent with text found in conversation events")
}
