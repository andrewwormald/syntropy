package openhands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewwormald/syntropy/internal/runner"
)

func TestName(t *testing.T) {
	r := NewRunner("")
	if r.Name() != "openhands" {
		t.Errorf("Name() = %q, want %q", r.Name(), "openhands")
	}
	if r.Binary != "agent-server" {
		t.Errorf("default Binary = %q, want %q", r.Binary, "agent-server")
	}
}

// mockAgentServer builds an httptest.Server implementing just enough of the
// Agent Server surface (POST /api/conversations, POST .../run, GET
// /api/conversations/{id}, GET .../agent_final_response) for converse() to
// drive end to end. statuses is the sequence of execution_status values
// returned on successive GET /api/conversations/{id} calls (the last value
// repeats once exhausted); finalMessage is the agent's final response text.
func mockAgentServer(t *testing.T, statuses []string, finalMessage string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	pollCount := 0
	var gotWorkingDir []string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversations", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("unexpected method %s on /api/conversations", req.Method)
		}
		var body createConversationRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode create conversation body: %v", err)
		}
		mu.Lock()
		gotWorkingDir = append(gotWorkingDir, body.Workspace.WorkingDir)
		mu.Unlock()
		if body.ConfirmationPolicy.Kind != "NeverConfirm" {
			t.Errorf("ConfirmationPolicy.Kind = %q, want NeverConfirm", body.ConfirmationPolicy.Kind)
		}
		if len(body.InitialMessage.Content) == 0 || body.InitialMessage.Content[0].Text == "" {
			t.Error("InitialMessage.Content should not be empty")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createConversationResponse{ID: "conv-1"})
	})
	mux.HandleFunc("/api/conversations/conv-1/run", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("unexpected method %s on .../run", req.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/conversations/conv-1/agent_final_response", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agentFinalResponse{Response: finalMessage})
	})
	mux.HandleFunc("/api/conversations/conv-1", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		idx := pollCount
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		pollCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conversationInfo{ID: "conv-1", ExecutionStatus: statuses[idx]})
	})

	return httptest.NewServer(mux), &gotWorkingDir
}

func testRunner() *Runner {
	return &Runner{PollInterval: 5 * time.Millisecond}
}

func TestConverse_Done(t *testing.T) {
	srv, gotWorkingDir := mockAgentServer(t, []string{statusRunning, statusFinished},
		"All done.\n\n<syntropy-decision>done: fix(x): thing</syntropy-decision>")
	defer srv.Close()

	r := testRunner()
	resp, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/worktree", Goal: "do the thing"})
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if resp.Decision != runner.DecisionDone {
		t.Errorf("Decision = %v, want Done", resp.Decision)
	}
	if resp.Title != "fix(x): thing" {
		t.Errorf("Title = %q", resp.Title)
	}
	if !strings.Contains(resp.Summary, "All done") {
		t.Errorf("Summary = %q", resp.Summary)
	}
	if len(*gotWorkingDir) != 1 || (*gotWorkingDir)[0] != "/tmp/worktree" {
		t.Errorf("workspace.working_dir sent = %v, want [/tmp/worktree]", *gotWorkingDir)
	}
}

func TestConverse_ErrorStatusStillParsesDecision(t *testing.T) {
	srv, _ := mockAgentServer(t, []string{statusError},
		"Hit an unrecoverable problem.\n\n<syntropy-decision>fail: broken dependency</syntropy-decision>")
	defer srv.Close()

	r := testRunner()
	resp, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if resp.Decision != runner.DecisionFail {
		t.Errorf("Decision = %v, want Fail", resp.Decision)
	}
}

func TestConverse_WaitingForConfirmationIsRunnerError(t *testing.T) {
	srv, _ := mockAgentServer(t, []string{statusWaitingForConfirmation}, "irrelevant")
	defer srv.Close()

	r := testRunner()
	_, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil {
		t.Fatal("expected an error for waiting_for_confirmation")
	}
	if !strings.Contains(err.Error(), "waiting_for_confirmation") {
		t.Errorf("error = %v, want it to mention waiting_for_confirmation", err)
	}
}

func TestConverse_PausedIsRunnerError(t *testing.T) {
	srv, _ := mockAgentServer(t, []string{statusPaused}, "irrelevant")
	defer srv.Close()

	r := testRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.converse(ctx, srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil {
		t.Fatal("expected an error for paused")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("converse polled forever instead of treating paused as terminal")
	}
	if !strings.Contains(err.Error(), "paused") {
		t.Errorf("error = %v, want it to mention paused", err)
	}
}

func TestConverse_DeletingIsRunnerError(t *testing.T) {
	srv, _ := mockAgentServer(t, []string{statusDeleting}, "irrelevant")
	defer srv.Close()

	r := testRunner()
	_, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil {
		t.Fatal("expected an error for deleting")
	}
	if !strings.Contains(err.Error(), "deleting") {
		t.Errorf("error = %v, want it to mention deleting", err)
	}
}

func TestConverse_NoDecisionMarker(t *testing.T) {
	srv, _ := mockAgentServer(t, []string{statusFinished}, "I did some stuff but forgot the marker.")
	defer srv.Close()

	r := testRunner()
	_, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestConverse_EmptyFinalResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversations", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createConversationResponse{ID: "conv-1"})
	})
	mux.HandleFunc("/api/conversations/conv-1/run", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/conversations/conv-1/agent_final_response", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agentFinalResponse{Response: ""})
	})
	mux.HandleFunc("/api/conversations/conv-1", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conversationInfo{ID: "conv-1", ExecutionStatus: statusFinished})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRunner()
	_, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil {
		t.Fatal("expected an error when agent_final_response is empty")
	}
}

func TestConverse_CreateConversationHTTPError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversations", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRunner()
	_, err := r.converse(context.Background(), srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil || !strings.Contains(err.Error(), "create conversation") {
		t.Fatalf("err = %v, want it to mention create conversation", err)
	}
}

func TestConverse_ContextCanceledDuringPoll(t *testing.T) {
	srv, _ := mockAgentServer(t, []string{statusRunning}, "irrelevant")
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{PollInterval: 50 * time.Millisecond}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := r.converse(ctx, srv.URL, runner.Request{Worktree: "/tmp/w", Goal: "goal"})
	if err == nil {
		t.Fatal("expected an error when context is canceled mid-poll")
	}
}

func TestWaitReady_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &Runner{ReadyTimeout: time.Second}
	if err := r.waitReady(context.Background(), srv.URL); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
}

func TestWaitReady_TimesOut(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &Runner{ReadyTimeout: 50 * time.Millisecond}
	err := r.waitReady(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestSpawnAndStopServer(t *testing.T) {
	// spawnServer always builds argv as [binary, --host, ..., --port, ...,
	// extraArgs...]; ExtraArgs lets a test point that argv at a long-running
	// command that ignores those flags, purely to exercise spawn/kill
	// plumbing without a real Agent Server.
	r := &Runner{Binary: "sleep", ExtraArgs: []string{"5"}}
	cmd, err := r.spawnServer(0, "")
	if err != nil {
		t.Fatalf("spawnServer: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("spawnServer did not start the process")
	}
	stopServer(cmd)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("process was not reaped after stopServer")
	}
}

func TestStopServer_NilSafe(t *testing.T) {
	stopServer(nil)
	stopServer(&exec.Cmd{})
}

func TestFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Errorf("freePort() = %d, out of range", p)
	}
}

func TestDoJSON_NonJSONBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &Runner{}
	if err := r.doJSON(context.Background(), http.MethodPost, srv.URL+"/ok", nil, nil); err != nil {
		t.Fatalf("doJSON with nil body/out: %v", err)
	}
}
