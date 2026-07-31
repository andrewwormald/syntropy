package refactorsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"
	"github.com/luno/workflow/adapters/memrolescheduler"
	"github.com/luno/workflow/adapters/memtimeoutstore"

	"github.com/andrewwormald/syntropy/internal/eventstream"
	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
	"github.com/andrewwormald/syntropy/internal/store"
)

// TestStatusGraph_PausedAllowsSelfLoop is a regression guard for
// ADR-0034. The workflow library validates every (current, next) status
// transition against the graph configured by b.AddCallback. If
// b.AddCallback(StatusPaused, d.resume, ...) doesn't include StatusPaused
// in its allowed next-statuses, every event arriving at a paused Run
// fails with "current status not defined in graph: current=Paused,
// next=Paused" — silently dropping /syntropy resume and locking the Run
// out of recovery.
//
// resume() at workflow.go:703 explicitly returns (StatusPaused, nil) for
// non-control events on a paused Run, so the graph MUST permit
// Paused → Paused.
//
// The workflow library doesn't expose its transition graph for runtime
// inspection, so this test exercises the actual code path: build the
// workflow, seed a Paused record, and call workflow.Callback with a
// non-control event. If the graph is misconfigured, Callback returns
// the "not defined in graph" error.
func TestStatusGraph_PausedAllowsSelfLoop(t *testing.T) {
	d := integrationDeps(t)
	wf := Build("refactor-sweep-graph-test", d)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// We don't run wf.Run(); Callback is synchronous via processCallback
	// → updater → validateTransition. No consumers needed.

	const (
		foreignID = "fid-paused-self-loop"
		runID     = "00000000-0000-0000-0000-000000099001"
	)

	mr := provider.MR{ProjectID: "x/y", IID: 1}
	state := &AgentState{
		Goal:         "test",
		ProviderName: "fake",
		ProjectID:    "x/y",
		RunnerName:   "fake-runner",
		Mode:         ModeSpec,
		Author:       provider.User{Handle: "test-author"},
		InFlight:     map[string]provider.MR{"u": mr},
		CurrentUnit:  "u",
		EventSource:  EventSourcePoll,
		PauseReason:  "set up by test",
	}
	objJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal AgentState: %v", err)
	}
	now := time.Now()
	rec := &workflow.Record{
		WorkflowName: wf.Name(),
		ForeignID:    foreignID,
		RunID:        runID,
		RunState:     workflow.RunStateRunning,
		Status:       int(StatusPaused),
		Object:       objJSON,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := d.RecordStore.Store(ctx, rec); err != nil {
		t.Fatalf("seed paused record: %v", err)
	}

	// Send a non-control event from a non-author. resume() will hit the
	// `if r.Status == StatusPaused { return StatusPaused, nil }` early
	// return, triggering the Paused → Paused transition.
	ev := provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: "x/y",
		MR:        mr,
		Author:    provider.User{Handle: "some-reviewer"},
		IsAuthor:  false,
		Note:      provider.Note{ID: 100, Body: "lgtm", DiscussionID: "d-1"},
		ReceivedAt: time.Now().UnixNano(),
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	cbErr := wf.Callback(ctx, foreignID, StatusPaused, bytes.NewReader(payload))
	if cbErr != nil && strings.Contains(cbErr.Error(), "not defined in graph") {
		t.Errorf(
			"Paused → Paused was rejected by the workflow graph: %v\n\n"+
				"This means b.AddCallback(StatusPaused, d.resume, ...) in workflow.go no longer "+
				"includes StatusPaused as an allowed next-status. Add it back — see ADR-0034. "+
				"Without it, events arriving at a paused Run cannot be processed and /syntropy resume "+
				"is silently dropped.",
			cbErr,
		)
	}
	if cbErr != nil {
		// Other errors are acceptable in this test (e.g. provider not
		// registered for runner-related branches we don't take here);
		// the only failure mode this test guards against is the
		// "not defined in graph" one above.
		t.Logf("Callback returned non-graph error (acceptable): %v", cbErr)
	}
}

// TestStatusGraph_AwaitingAbandonConfirmAllowsSelfLoop is the sibling
// guard: AwaitingAbandonConfirm must permit a self-loop for the same
// reason — events arriving during the 12h confirm window that aren't
// the second /syntropy abandon shouldn't crash dispatch.
func TestStatusGraph_AwaitingAbandonConfirmAllowsSelfLoop(t *testing.T) {
	d := integrationDeps(t)
	wf := Build("refactor-sweep-graph-test2", d)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const (
		foreignID = "fid-abandon-self-loop"
		runID     = "00000000-0000-0000-0000-000000099002"
	)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	state := &AgentState{
		Goal:         "test",
		ProviderName: "fake",
		ProjectID:    "x/y",
		RunnerName:   "fake-runner",
		Mode:         ModeSpec,
		Author:       provider.User{Handle: "test-author"},
		InFlight:     map[string]provider.MR{"u": mr},
		CurrentUnit:  "u",
		EventSource:  EventSourcePoll,
		AbandonRequestedAt: time.Now(),
	}
	objJSON, _ := json.Marshal(state)
	now := time.Now()
	rec := &workflow.Record{
		WorkflowName: wf.Name(),
		ForeignID:    foreignID,
		RunID:        runID,
		RunState:     workflow.RunStateRunning,
		Status:       int(StatusAwaitingAbandonConfirm),
		Object:       objJSON,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := d.RecordStore.Store(ctx, rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	// Non-author note — definitely not a confirm/cancel, so resume()
	// should return AwaitingAbandonConfirm to stay put.
	ev := provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: "x/y",
		MR:        mr,
		Author:    provider.User{Handle: "bot", Bot: true},
		IsBot:     true,
		Note:      provider.Note{ID: 200, Body: "🤖 CI ran"},
	}
	payload, _ := json.Marshal(ev)

	cbErr := wf.Callback(ctx, foreignID, StatusAwaitingAbandonConfirm, bytes.NewReader(payload))
	if cbErr != nil && strings.Contains(cbErr.Error(), "not defined in graph") {
		t.Errorf(
			"AwaitingAbandonConfirm → AwaitingAbandonConfirm was rejected: %v\n\n"+
				"b.AddCallback(StatusAwaitingAbandonConfirm, d.resume, ...) must include "+
				"StatusAwaitingAbandonConfirm itself in the allowed-next-statuses list (ADR-0034).",
			cbErr,
		)
	}
}

// integrationDeps wires the in-memory adapters needed to Build a real
// workflow.Workflow. Used by the graph-config regression tests above —
// other refactorsweep tests call step bodies directly and don't need
// the workflow plumbing.
func integrationDeps(t *testing.T) Deps {
	t.Helper()
	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	rs := memrecordstore.New(memrecordstore.WithOutbox(t.Context(), streamer, discardLogger{}))
	return Deps{
		RecordStore:   rs,
		TimeoutStore:  memtimeoutstore.New(),
		EventStreamer: streamer,
		RoleScheduler: memrolescheduler.New(),
		// Runners must be a non-nil (if empty) registry: the
		// Paused-self-loop path now reaches invokeForEvent for a
		// freeform reply on a non-Ask pause (ADR-0094 follow-on), and
		// Registry.Get on a nil *Registry panics instead of erroring.
		// An empty registry still returns a clean "unknown runner"
		// error, which is an acceptable non-graph error for this test.
		Runners: runner.NewRegistry(),
		// Providers/Git left empty — invokeForEvent returns before
		// touching either once Runners.Get fails above.
	}
}

// discardLogger satisfies the small logger interface memrecordstore.WithOutbox
// expects without pulling in a real logging dep.
type discardLogger struct{}

func (discardLogger) Debug(_ context.Context, _ string, _ map[string]string) {}
func (discardLogger) Info(_ context.Context, _ string, _ map[string]string)  {}
func (discardLogger) Warn(_ context.Context, _ string, _ map[string]string)  {}
func (discardLogger) Error(_ context.Context, _ error) {}
