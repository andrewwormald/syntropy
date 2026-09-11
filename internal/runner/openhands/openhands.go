// Package openhands implements runner.Runner by managing a per-Run
// openhands-agent-server subprocess and driving it over HTTP. See ADR-0112
// for the design: a subprocess (not a shared server), the same
// decision-marker protocol as internal/runner/claude (ADR-0027) reused via
// claude.ParseDecision, a package-local prompt built by BuildPrompt (see
// prompt.go — it ports claude.BuildPrompt's section structure but adds an
// OpenHands-specific never-push/never-call-provider-API instruction, since
// the Agent Server's confirmation policy may grant broader tool access than
// Claude Code's --dangerously-skip-permissions sandbox), and confirmation
// policy forced to "NeverConfirm" so tool calls never pause for human
// confirmation.
//
// The Agent Server endpoint shapes here were confirmed against a real
// locally running openhands-agent-server v1.44.1 (2026-09-07), correcting
// several mismatches ADR-0112's desk research got wrong: the installed
// console script is named "agent-server", not "openhands-agent-server";
// initial_message is a SendMessageRequest object, not a bare string;
// confirmation_policy's kind is "NeverConfirm", not "auto_approve";
// ConversationExecutionStatus's real values are idle/running/paused/
// waiting_for_confirmation/finished/error/stuck/deleting (no "stopped",
// and the confirmation-pause state is "waiting_for_confirmation" not
// "awaiting_user_input"); and the agent's final reply is read via the
// dedicated GET .../agent_final_response endpoint rather than by
// hand-parsing the events/search discriminated union (whose message
// payload lives under "llm_message", not "message").
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

// Execution status values from ConversationExecutionStatus, confirmed
// against a real running openhands-agent-server (v1.44.1): idle, running,
// paused, waiting_for_confirmation, finished, error, stuck, deleting. There
// is no "stopped" value and the confirmation-pause state is named
// "waiting_for_confirmation", not "awaiting_user_input" — both corrected
// here from ADR-0112's desk-research assumption.
const (
	statusIdle                   = "idle"
	statusRunning                = "running"
	statusPaused                 = "paused"
	statusWaitingForConfirmation = "waiting_for_confirmation"
	statusFinished               = "finished"
	statusError                  = "error"
	statusStuck                  = "stuck"
	statusDeleting               = "deleting"
)

// Runner implements runner.Runner. The zero value is usable (uses
// "agent-server" from $PATH with default timeouts). NewRunner is
// the canonical constructor.
type Runner struct {
	// Binary is the path to the openhands-agent-server executable (or a
	// wrapper script). Defaults to "agent-server" — the actual console-script
	// name the openhands-agent-server PyPI package installs; there is no
	// script literally named "openhands-agent-server".
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

	// APIKey is the LLM provider API key sent to the Agent Server as
	// agent.llm.api_key on every conversation it creates.
	APIKey string

	// BaseURL, if set, overrides the LLM provider endpoint sent to the
	// Agent Server as agent.llm.base_url.
	BaseURL string
}

// NewRunner constructs a Runner. All arguments are optional.
func NewRunner(binary string, extraArgs ...string) *Runner {
	if binary == "" {
		binary = "agent-server"
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
	prompt := BuildPrompt(req)

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

	// waiting_for_confirmation fires from the confirmation-policy mechanism,
	// which is disabled by createConversation's NeverConfirm policy. If we
	// see it anyway, it's a runner-level error, not a DecisionAsk — those
	// are two different concepts (ADR-0112 §3).
	if status == statusWaitingForConfirmation {
		return runner.Response{}, fmt.Errorf("openhands: conversation %s paused waiting_for_confirmation; treated as a runner error, not DecisionAsk (ADR-0112 §3)", convID)
	}

	// paused and deleting are both terminal-but-unexpected states we have no
	// way to recover a final response from; surface them as runner errors
	// the same way, instead of silently falling through to
	// finalResponseText.
	if status == statusPaused {
		return runner.Response{}, fmt.Errorf("openhands: conversation %s paused", convID)
	}
	if status == statusDeleting {
		return runner.Response{}, fmt.Errorf("openhands: conversation %s is deleting", convID)
	}

	text, err := r.finalResponseText(ctx, baseURL, convID)
	if err != nil {
		return runner.Response{}, fmt.Errorf("openhands: fetch final response: %w", err)
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
		binary = "agent-server"
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

// httpStatusError carries the HTTP status code of a non-2xx Agent Server
// response so callers can special-case specific codes (e.g. startConversation
// tolerating 409) without parsing the error string.
type httpStatusError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: unexpected status %d: %s", e.Method, e.URL, e.StatusCode, e.Body)
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
		return &httpStatusError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
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
	InitialMessage     initialMessage     `json:"initial_message"`
	ConfirmationPolicy confirmationPolicy `json:"confirmation_policy"`
}

type agentConfig struct {
	LLM llmConfig `json:"llm"`
}

type llmConfig struct {
	Model   string `json:"model,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

type workspaceConfig struct {
	Kind       string `json:"kind"`
	WorkingDir string `json:"working_dir"`
}

// initialMessage is a SendMessageRequest, not a bare string — confirmed
// against a real running openhands-agent-server, which rejects a plain
// string with "Input should be a valid dictionary or object".
type initialMessage struct {
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// confirmationPolicy is set to never pause for human confirmation of tool
// calls — ADR-0112 §3, same reasoning as ADR-0027 §4's unconditional
// --dangerously-skip-permissions: inside the worktree, autonomous tool use
// is the accepted risk. "NeverConfirm" is the real ConfirmationPolicyBase
// discriminator value; the server rejects "auto_approve" with "Unknown
// kind".
type confirmationPolicy struct {
	Kind string `json:"kind"`
}

type createConversationResponse struct {
	ID string `json:"id"`
}

func (r *Runner) createConversation(ctx context.Context, baseURL string, req runner.Request, initialMessageText string) (string, error) {
	payload := createConversationRequest{
		Agent: agentConfig{LLM: llmConfig{Model: req.Model, APIKey: r.APIKey, BaseURL: r.BaseURL}},
		Workspace: workspaceConfig{
			Kind:       "LocalWorkspace",
			WorkingDir: req.Worktree,
		},
		InitialMessage:     initialMessage{Content: []contentPart{{Type: "text", Text: initialMessageText}}},
		ConfirmationPolicy: confirmationPolicy{Kind: "NeverConfirm"},
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

// startConversation POSTs .../run to kick off execution. The Agent Server
// can already be running the conversation by the time this call lands — for
// example, create implicitly starts it on some server versions, or a
// caller retries after a timeout whose original /run actually succeeded —
// and answers a redundant /run with 409 Conflict. That 409 means the
// conversation is in the state we wanted (running), not that anything went
// wrong, so it's tolerated here rather than surfaced as a runner error.
func (r *Runner) startConversation(ctx context.Context, baseURL, convID string) error {
	err := r.doJSON(ctx, http.MethodPost, baseURL+"/api/conversations/"+convID+"/run", nil, nil)
	if err == nil {
		return nil
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return nil
	}
	return err
}

// conversationInfo is the response shape of GET /api/conversations/{id}.
type conversationInfo struct {
	ID              string `json:"id"`
	ExecutionStatus string `json:"execution_status"`
}

// pollUntilDone polls GET /api/conversations/{id} until execution_status
// reaches a terminal value (finished, error, waiting_for_confirmation,
// stuck, paused, deleting), or ctx is done.
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
		case statusFinished, statusError, statusWaitingForConfirmation, statusStuck, statusPaused, statusDeleting:
			return info.ExecutionStatus, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// agentFinalResponse is the response shape of GET
// /api/conversations/{id}/agent_final_response — the server's own
// purpose-built endpoint for "the agent's final response text, extracted
// from either a FinishAction message or the last agent MessageEvent". Using
// it instead of hand-parsing the discriminated event union from
// events/search avoids depending on that union's field names, which don't
// match ADR-0112's desk-research assumption (the message payload lives
// under "llm_message", not "message").
type agentFinalResponse struct {
	Response string `json:"response"`
}

// finalResponseText returns the agent's final freeform reply, per
// ADR-0112.
func (r *Runner) finalResponseText(ctx context.Context, baseURL, convID string) (string, error) {
	var out agentFinalResponse
	if err := r.doJSON(ctx, http.MethodGet, baseURL+"/api/conversations/"+convID+"/agent_final_response", nil, &out); err != nil {
		return "", err
	}
	if out.Response == "" {
		return "", errors.New("agent_final_response returned an empty response")
	}
	return out.Response, nil
}
