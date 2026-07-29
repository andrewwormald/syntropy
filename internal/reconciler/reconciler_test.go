package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"
	"github.com/luno/workflow/adapters/memrolescheduler"

	"github.com/andrewwormald/syntropy/internal/eventstream"
	"github.com/andrewwormald/syntropy/internal/refactorsweep"
	"github.com/andrewwormald/syntropy/internal/store"
)

const testWorkflowName = "refactor-sweep-reconciler-test"

func seedRecord(t *testing.T, rs workflow.RecordStore, runID string, runState workflow.RunState, status refactorsweep.AgentStatus, state refactorsweep.AgentState, createdAt time.Time) {
	t.Helper()
	objJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal AgentState: %v", err)
	}
	rec := &workflow.Record{
		WorkflowName: testWorkflowName,
		ForeignID:    "fid-" + runID,
		RunID:        runID,
		RunState:     runState,
		Status:       int(status),
		Object:       objJSON,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	if err := rs.Store(t.Context(), rec); err != nil {
		t.Fatalf("seed record %s: %v", runID, err)
	}
}

func TestIsStuck(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute

	tests := []struct {
		name         string
		status       refactorsweep.AgentStatus
		lastProgress time.Time
		want         bool
	}{
		{
			name:         "stale in-flight Run is flagged",
			status:       refactorsweep.StatusWorking,
			lastProgress: now.Add(-time.Hour),
			want:         true,
		},
		{
			name:         "fresh in-flight Run is not flagged",
			status:       refactorsweep.StatusWorking,
			lastProgress: now.Add(-time.Minute),
			want:         false,
		},
		{
			name:         "stale Discovering Run is flagged",
			status:       refactorsweep.StatusDiscovering,
			lastProgress: now.Add(-time.Hour),
			want:         true,
		},
		{
			name:         "stale non-in-flight status is never flagged",
			status:       refactorsweep.StatusAwaitingMerge,
			lastProgress: now.Add(-time.Hour),
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsStuck(tt.status, tt.lastProgress, now, threshold)
			if got != tt.want {
				t.Errorf("IsStuck() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastProgress(t *testing.T) {
	fallback := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	started := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 7, 17, 11, 5, 0, 0, time.UTC)

	tests := []struct {
		name  string
		state refactorsweep.AgentState
		want  time.Time
	}{
		{
			name: "history present uses last turn's end time",
			state: refactorsweep.AgentState{
				History: []refactorsweep.Turn{
					{StartedAt: started.Add(-time.Hour), EndedAt: started.Add(-time.Hour + time.Minute)},
					{StartedAt: started, EndedAt: ended},
				},
			},
			want: ended,
		},
		{
			name:  "history empty uses fallback",
			state: refactorsweep.AgentState{},
			want:  fallback,
		},
		{
			name: "turn still in-flight uses started time",
			state: refactorsweep.AgentState{
				History: []refactorsweep.Turn{
					{StartedAt: started, EndedAt: time.Time{}},
				},
			},
			want: started,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LastProgress(tt.state, fallback)
			if !got.Equal(tt.want) {
				t.Errorf("LastProgress() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Regression (ADR-0090): found live — a Run resumed from Failed days
// after its last real Turn was immediately re-flagged as stuck by the
// reconciler, because History still pointed at that days-old timestamp
// and the resume itself (a store write with no new Turn) wasn't
// recognised as progress. recordUpdatedAt must win when it's more
// recent than History's last turn, not just serve as a fallback for an
// entirely empty History.
func TestLastProgress_RecordUpdatedAtMoreRecentThanStaleHistory_Wins(t *testing.T) {
	staleHistoryTime := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC) // the Run's last real Turn, days ago
	justResumed := time.Date(2026, 7, 28, 12, 57, 0, 0, time.UTC)     // directResume's store write, moments ago

	state := refactorsweep.AgentState{
		History: []refactorsweep.Turn{
			{StartedAt: staleHistoryTime, EndedAt: staleHistoryTime},
		},
	}
	got := LastProgress(state, justResumed)
	if !got.Equal(justResumed) {
		t.Errorf("LastProgress() = %v, want the just-resumed record UpdatedAt %v (must not use stale History)", got, justResumed)
	}
}

func TestScan(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute
	stale := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)

	rs := memrecordstore.New()

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000001", workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000002", workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: fresh, EndedAt: fresh}}}, fresh)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000003", workflow.RunStateRunning, refactorsweep.StatusAwaitingMerge,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000004", workflow.RunStatePaused, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	seedRecord(t, rs, "00000000-0000-0000-0000-000000000005", workflow.RunStateCompleted, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	got, err := Scan(t.Context(), rs, testWorkflowName, now, threshold)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	want := []string{"00000000-0000-0000-0000-000000000001"}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("Scan() = %v, want %v", got, want)
	}
}

// Regression (ADR-0090): a Run resumed from Failed (e.g. via directResume)
// has stale History but a fresh record UpdatedAt from the resume's own
// store write. Scan must not flag it as stuck immediately — only once
// genuinely no progress (including no further resume) happens for the
// full threshold.
func TestScan_JustResumedRun_NotImmediatelyFlaggedStuck(t *testing.T) {
	now := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute
	staleHistoryTime := now.Add(-5 * 24 * time.Hour) // last real Turn, 5 days ago
	justResumed := now.Add(-1 * time.Minute)         // directResume's write, a minute ago

	rs := memrecordstore.New()
	state := refactorsweep.AgentState{
		History: []refactorsweep.Turn{{StartedAt: staleHistoryTime, EndedAt: staleHistoryTime}},
	}
	objJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal AgentState: %v", err)
	}
	if err := rs.Store(t.Context(), &workflow.Record{
		WorkflowName: testWorkflowName,
		ForeignID:    "fid-resumed",
		RunID:        "00000000-0000-0000-0000-0000000000aa",
		RunState:     workflow.RunStateRunning,
		Status:       int(refactorsweep.StatusDiscovering),
		Object:       objJSON,
		CreatedAt:    staleHistoryTime,
		UpdatedAt:    justResumed,
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	got, err := Scan(t.Context(), rs, testWorkflowName, now, threshold)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Scan() flagged a just-resumed Run as stuck despite fresh UpdatedAt: %v", got)
	}
}

// retriggerObj and retriggerStatus are a minimal workflow Type/Status pair
// used only to exercise Retrigger against a real workflow.Workflow, so the
// tests below drive the vendored library's own step consumer rather than
// re-implementing its idempotency guard.
type retriggerObj struct{}

type retriggerStatus int

const (
	retriggerStatusA retriggerStatus = 1
	retriggerStatusB retriggerStatus = 2
)

func (s retriggerStatus) String() string {
	if s == retriggerStatusB {
		return "B"
	}
	return "A"
}

func TestRetrigger_EventShape(t *testing.T) {
	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())

	record := workflow.Record{
		WorkflowName: "retrigger-shape-test",
		ForeignID:    "fid-1",
		RunID:        "00000000-0000-0000-0000-000000000001",
		RunState:     workflow.RunStateRunning,
		Status:       int(retriggerStatusA),
		Meta:         workflow.Meta{Version: 3},
	}

	rec, err := streamer.NewReceiver(t.Context(), workflow.Topic(record.WorkflowName, record.Status), "shape-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	if err := Retrigger(t.Context(), streamer, record); err != nil {
		t.Fatalf("Retrigger() error = %v", err)
	}

	e, ack, err := rec.Recv(t.Context())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	defer ack()

	wantTopic := workflow.Topic(record.WorkflowName, record.Status)
	wantHeaders := map[workflow.Header]string{
		workflow.HeaderForeignID:     record.ForeignID,
		workflow.HeaderWorkflowName:  record.WorkflowName,
		workflow.HeaderTopic:         wantTopic,
		workflow.HeaderRunID:         record.RunID,
		workflow.HeaderRunState:      "2",
		workflow.HeaderRecordVersion: "3",
	}

	// Event.ForeignID carries the RunID (what the step consumer's
	// lookupFn keys on), not the business ForeignID — see Retrigger's
	// comment on this.
	if e.ForeignID != record.RunID {
		t.Errorf("ForeignID = %q, want %q (the RunID)", e.ForeignID, record.RunID)
	}
	if e.Type != record.Status {
		t.Errorf("Type = %d, want %d", e.Type, record.Status)
	}
	for k, want := range wantHeaders {
		if got := e.Headers[k]; got != want {
			t.Errorf("header %q = %q, want %q", k, got, want)
		}
	}
}

// TestRetrigger_SkipsStaleOrDuplicate drives a real workflow.Workflow (using
// the project's sqlite-backed EventStreamer) so the assertion exercises the
// vendored library's own stepConsumer idempotency guard rather than a
// reimplementation of it: an event whose HeaderRecordVersion no longer
// matches the record's current version is skipped, not double-processed.
func TestRetrigger_SkipsStaleOrDuplicate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	var processed atomic.Int32
	builder := workflow.NewBuilder[retriggerObj, retriggerStatus]("retrigger-skip-test")
	builder.AddStep(retriggerStatusA, func(ctx context.Context, r *workflow.Run[retriggerObj, retriggerStatus]) (retriggerStatus, error) {
		processed.Add(1)
		return retriggerStatusB, nil
	}, retriggerStatusB)

	wf := builder.Build(streamer, recordStore, memrolescheduler.New(), workflow.WithoutOutbox())
	wf.Run(ctx)
	t.Cleanup(wf.Stop)

	const foreignID = "fid-1"
	obj, err := workflow.Marshal(&retriggerObj{})
	if err != nil {
		t.Fatalf("marshal retriggerObj: %v", err)
	}
	seeded := &workflow.Record{
		WorkflowName: wf.Name(),
		ForeignID:    foreignID,
		RunID:        "00000000-0000-0000-0000-000000000001",
		RunState:     workflow.RunStateRunning,
		Status:       int(retriggerStatusA),
		Object:       obj,
	}
	if err := recordStore.Store(ctx, seeded); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	// Snapshot exactly what reconciler.Scan would have seen: the stuck
	// record before anything re-triggers it.
	stale, err := recordStore.Latest(ctx, wf.Name(), foreignID)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}

	if err := Retrigger(ctx, streamer, *stale); err != nil {
		t.Fatalf("Retrigger() error = %v", err)
	}

	waitFor(t, func() bool { return processed.Load() == 1 })

	current, err := recordStore.Latest(ctx, wf.Name(), foreignID)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if current.Status != int(retriggerStatusB) {
		t.Fatalf("record status = %d, want %d (StatusB) after first retrigger processed", current.Status, retriggerStatusB)
	}

	// Retrigger again using the now-stale snapshot (Meta.Version from
	// before the first retrigger advanced it). The real consumer must
	// skip this rather than reprocessing the record.
	if err := Retrigger(ctx, streamer, *stale); err != nil {
		t.Fatalf("Retrigger() (stale) error = %v", err)
	}

	// Give the consumer a chance to (wrongly) reprocess before asserting
	// it didn't.
	time.Sleep(100 * time.Millisecond)
	if got := processed.Load(); got != 1 {
		t.Errorf("processed = %d, want 1 (duplicate/stale retrigger must be skipped)", got)
	}
}

// TestSweeper_Run drives the real ticker-based loop (rather than calling
// sweepOnce directly) so the assertion exercises Run's tick-and-sweep wiring,
// not just the underlying Scan/Retrigger calls it composes.
func TestSweeper_Run(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	stale := time.Now().Add(-time.Hour)
	fresh := time.Now().Add(-time.Minute)
	const threshold = 30 * time.Minute

	stuckRunID := "00000000-0000-0000-0000-000000000010"
	freshRunID := "00000000-0000-0000-0000-000000000011"

	// seedRecord always stamps WorkflowName as testWorkflowName, so the
	// Sweeper under test must target that same workflow name.
	seedRecord(t, recordStore, stuckRunID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)
	seedRecord(t, recordStore, freshRunID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: fresh, EndedAt: fresh}}}, fresh)

	topic := workflow.Topic(testWorkflowName, int(refactorsweep.StatusWorking))
	rec, err := streamer.NewReceiver(ctx, topic, "sweeper-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	sweeper := &Sweeper{
		Store:        recordStore,
		Streamer:     streamer,
		WorkflowName: testWorkflowName,
		Interval:     10 * time.Millisecond,
		Threshold:    threshold,
	}
	go sweeper.Run(ctx)

	recvCtx, recvCancel := context.WithTimeout(ctx, 2*time.Second)
	defer recvCancel()
	e, ack, err := rec.Recv(recvCtx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	ack()

	if e.ForeignID != stuckRunID {
		t.Errorf("retriggered run = %q, want %q (the stuck run)", e.ForeignID, stuckRunID)
	}

	// The fresh run must never be retriggered. Nothing here advances the
	// stuck record's status (there's no step consumer wired up), so the
	// loop keeps re-sending for it every tick; drain a few more of those
	// and confirm the fresh run's ID never appears among them.
	for i := 0; i < 5; i++ {
		recvCtx2, recvCancel2 := context.WithTimeout(ctx, 200*time.Millisecond)
		e2, ack2, err := rec.Recv(recvCtx2)
		recvCancel2()
		if err != nil {
			break
		}
		ack2()
		if e2.ForeignID == freshRunID {
			t.Fatalf("fresh run %q was retriggered, want only %q", freshRunID, stuckRunID)
		}
	}
}

// recvOrTimeout waits up to timeout for the next event on rec, returning
// (event, true) if one arrives or (zero, false) if it doesn't — used to
// assert the *absence* of a retrigger within a window.
func recvOrTimeout(t *testing.T, ctx context.Context, rec workflow.EventReceiver, timeout time.Duration) (*workflow.Event, bool) {
	t.Helper()
	recvCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	e, ack, err := rec.Recv(recvCtx)
	if err != nil {
		return nil, false
	}
	ack()
	return e, true
}

// TestSweeper_sweepOnce_CooldownSkipsRetrigger drives sweepOnce directly
// (rather than the ticker in Run) so it can call it back-to-back without
// waiting on Interval: a stuck Run that was just re-triggered must not be
// re-triggered again while still within RetriggerCooldown.
func TestSweeper_sweepOnce_CooldownSkipsRetrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	stale := time.Now().Add(-time.Hour)
	const threshold = 30 * time.Minute
	runID := "00000000-0000-0000-0000-000000000020"

	seedRecord(t, recordStore, runID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	topic := workflow.Topic(testWorkflowName, int(refactorsweep.StatusWorking))
	rec, err := streamer.NewReceiver(ctx, topic, "cooldown-skip-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	sweeper := &Sweeper{
		Store:             recordStore,
		Streamer:          streamer,
		WorkflowName:      testWorkflowName,
		Threshold:         threshold,
		RetriggerCooldown: time.Hour,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	sweeper.sweepOnce(ctx)
	if _, ok := recvOrTimeout(t, ctx, rec, 2*time.Second); !ok {
		t.Fatalf("expected first sweepOnce to retrigger the stuck run")
	}

	sweeper.sweepOnce(ctx)
	if e, ok := recvOrTimeout(t, ctx, rec, 200*time.Millisecond); ok {
		t.Fatalf("expected second sweepOnce within cooldown to skip retrigger, got event for %q", e.ForeignID)
	}
}

// TestSweeper_sweepOnce_EligibleAfterCooldownElapses mirrors the skip case
// above but with a cooldown short enough to elapse between the two
// sweepOnce calls, so the second call must retrigger again.
func TestSweeper_sweepOnce_EligibleAfterCooldownElapses(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	stale := time.Now().Add(-time.Hour)
	const threshold = 30 * time.Minute
	runID := "00000000-0000-0000-0000-000000000021"

	seedRecord(t, recordStore, runID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	topic := workflow.Topic(testWorkflowName, int(refactorsweep.StatusWorking))
	rec, err := streamer.NewReceiver(ctx, topic, "cooldown-elapse-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	const cooldown = 20 * time.Millisecond
	sweeper := &Sweeper{
		Store:             recordStore,
		Streamer:          streamer,
		WorkflowName:      testWorkflowName,
		Threshold:         threshold,
		RetriggerCooldown: cooldown,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	sweeper.sweepOnce(ctx)
	if _, ok := recvOrTimeout(t, ctx, rec, 2*time.Second); !ok {
		t.Fatalf("expected first sweepOnce to retrigger the stuck run")
	}

	time.Sleep(2 * cooldown)

	sweeper.sweepOnce(ctx)
	if _, ok := recvOrTimeout(t, ctx, rec, 2*time.Second); !ok {
		t.Fatalf("expected sweepOnce after cooldown elapsed to retrigger again")
	}
}

// TestSweeper_sweepOnce_FreshProgressResetsCooldown covers the "reset
// naturally once a new turn advances lastProgress" case: even while still
// within a long cooldown window, a RunID whose lastProgress has moved
// forward since the retrigger that started the cooldown (i.e. it made
// progress and then got stuck again) must be re-triggered immediately
// rather than waiting the cooldown out.
func TestSweeper_sweepOnce_FreshProgressResetsCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b, err := store.OpenSqlite(":memory:")
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	streamer := eventstream.New(b.DB())
	recordStore := memrecordstore.New()

	stale := time.Now().Add(-time.Hour)
	const threshold = 30 * time.Minute
	runID := "00000000-0000-0000-0000-000000000022"

	seedRecord(t, recordStore, runID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{{StartedAt: stale, EndedAt: stale}}}, stale)

	topic := workflow.Topic(testWorkflowName, int(refactorsweep.StatusWorking))
	rec, err := streamer.NewReceiver(ctx, topic, "cooldown-fresh-progress-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	sweeper := &Sweeper{
		Store:             recordStore,
		Streamer:          streamer,
		WorkflowName:      testWorkflowName,
		Threshold:         threshold,
		RetriggerCooldown: time.Hour,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	sweeper.sweepOnce(ctx)
	if _, ok := recvOrTimeout(t, ctx, rec, 2*time.Second); !ok {
		t.Fatalf("expected first sweepOnce to retrigger the stuck run")
	}

	// The Run made progress (a new turn advanced lastProgress) and then
	// got stuck again, still well within the one-hour cooldown window.
	newStale := time.Now().Add(-time.Hour)
	seedRecord(t, recordStore, runID, workflow.RunStateRunning, refactorsweep.StatusWorking,
		refactorsweep.AgentState{History: []refactorsweep.Turn{
			{StartedAt: stale, EndedAt: stale},
			{StartedAt: newStale, EndedAt: newStale},
		}}, stale)

	sweeper.sweepOnce(ctx)
	if _, ok := recvOrTimeout(t, ctx, rec, 2*time.Second); !ok {
		t.Fatalf("expected sweepOnce to retrigger despite active cooldown once lastProgress advanced")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}
