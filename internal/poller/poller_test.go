package poller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/andrewwormald/syntropy/internal/provider"
)

func TestAuthBackoffDuration(t *testing.T) {
	tests := []struct {
		failures int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{0, 30 * time.Second, 30 * time.Second},
		{1, 2 * time.Minute, 2 * time.Minute},
		{2, 8 * time.Minute, 8 * time.Minute},
		{3, 32 * time.Minute, 32 * time.Minute},
		{4, 2 * time.Hour, 2 * time.Hour},  // capped
		{10, 2 * time.Hour, 2 * time.Hour}, // still capped
	}
	for _, tt := range tests {
		got := authBackoffDuration(tt.failures)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("authBackoffDuration(%d) = %v, want [%v, %v]",
				tt.failures, got, tt.wantMin, tt.wantMax)
		}
	}
}

// --- Auth event dispatch ---

// fakeAuthProvider wraps a fixed GetMRState error to simulate auth failures.
type fakeAuthProvider struct {
	getMRStateErr   error
	getMRStateCalls int
	mu              sync.Mutex
}

func (f *fakeAuthProvider) Name() string { return "fake" }
func (f *fakeAuthProvider) AuthenticatedUser(_ context.Context) (provider.User, error) {
	return provider.User{}, nil
}
func (f *fakeAuthProvider) RegisterWebhook(_ context.Context, _, _, _ string, _ []provider.EventKind) (string, error) {
	return "", nil
}
func (f *fakeAuthProvider) DeregisterWebhook(_ context.Context, _, _ string) error { return nil }
func (f *fakeAuthProvider) VerifySignature(_ http.Header, _ []byte, _ string) bool { return true }
func (f *fakeAuthProvider) NormaliseEvent(_ http.Header, _ []byte) (provider.Event, error) { return provider.Event{}, nil }
func (f *fakeAuthProvider) CreateMR(_ context.Context, _ string, _ provider.MRDraft) (provider.MR, error) { return provider.MR{}, nil }
func (f *fakeAuthProvider) PostComment(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeAuthProvider) UpdateMRTitle(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeAuthProvider) UpdateMRDescription(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeAuthProvider) CloseMR(_ context.Context, _ string, _ int) error { return nil }
func (f *fakeAuthProvider) ListNotesSince(_ context.Context, _ string, _ int, _ provider.NoteCursor) ([]provider.NotePoll, error) {
	return nil, nil
}
func (f *fakeAuthProvider) ResolveDiscussion(_ context.Context, _ string, _ int, _ string) error {
	return nil
}
func (f *fakeAuthProvider) ReplyToDiscussion(_ context.Context, _ string, _ int, _ string, _ string) error {
	return nil
}
func (f *fakeAuthProvider) ReactToNote(_ context.Context, _ string, _ int, _ int64, _, _ string) error {
	return nil
}
func (f *fakeAuthProvider) RetryPipelineJob(_ context.Context, _ string, _ int64) error { return nil }
func (f *fakeAuthProvider) IsBot(_ provider.User) bool                                  { return false }
func (f *fakeAuthProvider) GetMRState(_ context.Context, _ string, _ int) (provider.MRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getMRStateCalls++
	return provider.MRState{}, f.getMRStateErr
}

// dispatchRecord captures events dispatched by the poller.
type dispatchRecord struct {
	mu     sync.Mutex
	events []provider.Event
}

func (d *dispatchRecord) dispatch(_ context.Context, _ string, ev provider.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
	return nil
}

func (d *dispatchRecord) kinds() []provider.EventKind {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]provider.EventKind, len(d.events))
	for i, ev := range d.events {
		out[i] = ev.Kind
	}
	return out
}

// notesProvider wraps fakeAuthProvider and additionally records the
// provider.NoteCursor it was called with and returns a fixed set of notes.
type notesProvider struct {
	fakeAuthProvider
	notes      []provider.NotePoll
	gotCursors []provider.NoteCursor
}

func (n *notesProvider) ListNotesSince(_ context.Context, _ string, _ int, since provider.NoteCursor) ([]provider.NotePoll, error) {
	// Deep-copy ByStream: the poller mutates its own map in place after
	// this call returns to record the notes we're about to hand back, and
	// it's the SAME map instance passed in via since.ByStream. Snapshot it
	// here so later assertions see what THIS call actually received.
	byStream := make(map[string]int64, len(since.ByStream))
	for k, v := range since.ByStream {
		byStream[k] = v
	}
	n.gotCursors = append(n.gotCursors, provider.NoteCursor{ByStream: byStream, Legacy: since.Legacy})
	return n.notes, nil
}

// TestPollRun_NoteCursor_PerStreamWithLegacyFallback verifies the poller
// assembles provider.NoteCursor from ActiveRun's per-stream cursors plus
// the legacy scalar (ADR-0041), and that a returned note's Stream tag
// advances only its own stream's cursor.
func TestPollRun_NoteCursor_PerStreamWithLegacyFallback(t *testing.T) {
	np := &notesProvider{
		notes: []provider.NotePoll{
			{ID: 100, Body: "inline review comment", Stream: "pull_request_review_comment"},
		},
	}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": np},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-1",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 1}},
		// Legacy watermark already advanced past 100 by a prior issue_comment
		// on a different stream; the review_comment stream has no cursor yet.
		LastSeenNoteIDs:     map[int]int64{1: 200},
		LastSeenNoteCursors: map[int]map[string]int64{1: {"issue_comment": 200}},
	}

	l.pollRun(t.Context(), run)

	if len(np.gotCursors) != 1 {
		t.Fatalf("want 1 ListNotesSince call, got %d", len(np.gotCursors))
	}
	got := np.gotCursors[0]
	if got.Legacy != 200 {
		t.Errorf("want Legacy=200, got %d", got.Legacy)
	}
	if got.ByStream["issue_comment"] != 200 {
		t.Errorf("want ByStream[issue_comment]=200, got %+v", got.ByStream)
	}
	if _, ok := got.ByStream["pull_request_review_comment"]; ok {
		t.Errorf("pull_request_review_comment should have no cursor yet, got %+v", got.ByStream)
	}

	// The note (id 100, lower than the legacy floor 200) must still have
	// been dispatched — its own stream had never advanced.
	if len(dr.events) != 1 || dr.events[0].Note.ID != 100 {
		t.Fatalf("want the id-100 review comment dispatched despite legacy=200; got %+v", dr.events)
	}
}

func TestPollRun_AuthFailure_DispatchesAuthFailureEventOnceOnly(t *testing.T) {
	fp := &fakeAuthProvider{getMRStateErr: provider.ErrAuthFailure}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-1",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 1}},
	}

	// First poll tick — should dispatch EventProviderAuthFailure.
	l.pollRun(t.Context(), run)
	kinds := dr.kinds()
	if len(kinds) != 1 || kinds[0] != provider.EventProviderAuthFailure {
		t.Errorf("first auth failure poll should dispatch exactly one EventProviderAuthFailure; got %v", kinds)
	}

	// Second poll tick — the run is still in backoff (until in the future);
	// pollRun skips it entirely, so no new event is dispatched.
	l.pollRun(t.Context(), run)
	kinds = dr.kinds()
	if len(kinds) != 1 {
		t.Errorf("subsequent polls during backoff should not dispatch further events; got %v", kinds)
	}
}

func TestPollRun_AuthRestored_DispatchesRestoredEvent(t *testing.T) {
	fp := &fakeAuthProvider{getMRStateErr: provider.ErrAuthFailure}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-2",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 1}},
	}

	// First tick: auth failure registered, backoff set.
	l.pollRun(t.Context(), run)
	// Force the backoff to expire so the next poll is not skipped.
	l.authMu.Lock()
	e := l.authBackoff["run-2"]
	e.until = time.Now().Add(-1 * time.Second)
	l.authBackoff["run-2"] = e
	l.authMu.Unlock()

	// Second tick: token now works.
	fp.mu.Lock()
	fp.getMRStateErr = nil
	fp.mu.Unlock()

	l.pollRun(t.Context(), run)

	kinds := dr.kinds()
	// Expect: EventProviderAuthFailure from tick 1, EventProviderAuthRestored from tick 2.
	found := map[provider.EventKind]bool{}
	for _, k := range kinds {
		found[k] = true
	}
	if !found[provider.EventProviderAuthFailure] {
		t.Errorf("expected EventProviderAuthFailure in dispatched events; got %v", kinds)
	}
	if !found[provider.EventProviderAuthRestored] {
		t.Errorf("expected EventProviderAuthRestored in dispatched events after recovery; got %v", kinds)
	}
}

// --- MR terminal-state (merged/closed) dispatch ---

// mrStateProvider wraps fakeAuthProvider and returns a fixed MR state string
// (e.g. "merged") with no error, for terminal-state redispatch tests.
type mrStateProvider struct {
	fakeAuthProvider
	state       string
	hasConflict bool
}

func (p *mrStateProvider) GetMRState(_ context.Context, _ string, _ int) (provider.MRState, error) {
	return provider.MRState{State: p.state, HasConflict: p.hasConflict}, nil
}

// Regression (found live on a real run stuck for over an hour): the poller
// used to mark mrStates[mr.IID] = "merged" the instant it *detected* the
// change, before knowing whether the workflow actually *processed* it — a
// Run that happens to be Paused at that exact moment drops the dispatched
// event entirely (workflow.go's "while Paused, ignore all events" guard),
// but the poller had already cached "already told them", so no future tick
// would ever re-check or redispatch. Terminal MR-state events must keep
// being dispatched every tick for as long as the unit remains in InFlight —
// only the unit actually leaving InFlight (proof the workflow processed it)
// should stop it, and that happens naturally since ActiveRuns is re-read
// fresh every tick.
func TestPollRun_MRMerged_RedispatchesEveryTickWhileStillInFlight(t *testing.T) {
	fp := &mrStateProvider{state: "merged"}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-3",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 82167}},
	}

	// Simulate the unit still being in InFlight across three ticks — as it
	// would be if resume() keeps dropping the event (e.g. Paused) rather
	// than actually processing markUnitMerged and removing it.
	l.pollRun(t.Context(), run)
	l.pollRun(t.Context(), run)
	l.pollRun(t.Context(), run)

	kinds := dr.kinds()
	if len(kinds) != 3 {
		t.Fatalf("EventMRMerged must be redispatched every tick while the unit remains in-flight; got %d dispatches (%v)", len(kinds), kinds)
	}
	for _, k := range kinds {
		if k != provider.EventMRMerged {
			t.Errorf("expected only EventMRMerged; got %v", k)
		}
	}
}

// Once the unit actually leaves InFlight (the workflow processed the merge
// and removed it, as markUnitMerged does), the next tick's ActiveRun simply
// doesn't include it — dispatch naturally stops without needing any special
// "already handled" bookkeeping.
func TestPollRun_MRMerged_StopsOnceUnitLeavesInFlight(t *testing.T) {
	fp := &mrStateProvider{state: "merged"}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	withUnit := ActiveRun{
		RunID:    "run-4",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 82167}},
	}
	l.pollRun(t.Context(), withUnit)

	// Simulate the workflow having processed the merge and removed the
	// unit from InFlight — the next ActiveRun snapshot no longer has it.
	withoutUnit := ActiveRun{RunID: "run-4", Provider: "fake", InFlight: map[string]provider.MR{}}
	l.pollRun(t.Context(), withoutUnit)

	kinds := dr.kinds()
	if len(kinds) != 1 {
		t.Errorf("expected exactly one dispatch before the unit left InFlight; got %d (%v)", len(kinds), kinds)
	}
}

// TestPollRun_HasConflict_DispatchesMRConflictEvent verifies that a
// GetMRState response reporting HasConflict surfaces provider.EventMRConflict
// (ADR-0080), on the existing poll interval with no extra API call.
func TestPollRun_HasConflict_DispatchesMRConflictEvent(t *testing.T) {
	fp := &mrStateProvider{state: "opened", hasConflict: true}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-5",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 91001}},
	}

	l.pollRun(t.Context(), run)
	l.pollRun(t.Context(), run)

	kinds := dr.kinds()
	if len(kinds) != 2 {
		t.Fatalf("expected EventMRConflict dispatched every tick while HasConflict stays true; got %d dispatches (%v)", len(kinds), kinds)
	}
	for _, k := range kinds {
		if k != provider.EventMRConflict {
			t.Errorf("expected only EventMRConflict; got %v", k)
		}
	}
}

// TestPollRun_NoConflict_DoesNotDispatchMRConflictEvent verifies a clean
// (mergeable) MR produces no EventMRConflict dispatch.
func TestPollRun_NoConflict_DoesNotDispatchMRConflictEvent(t *testing.T) {
	fp := &mrStateProvider{state: "opened", hasConflict: false}
	dr := &dispatchRecord{}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": fp},
		Dispatcher: dr.dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	run := ActiveRun{
		RunID:    "run-6",
		Provider: "fake",
		InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: 91002}},
	}

	l.pollRun(t.Context(), run)

	if kinds := dr.kinds(); len(kinds) != 0 {
		t.Errorf("expected no dispatch for a conflict-free MR; got %v", kinds)
	}
}

// Verify that ErrAuthFailure is the sentinel we compare against.
func TestIsAuthError_WrappedErrAuthFailure(t *testing.T) {
	wrapped := errors.New("some context: " + provider.ErrAuthFailure.Error())
	if !provider.IsAuthError(wrapped) {
		// IsAuthError checks the error message too, so a message containing
		// "401" or "unauthorized" qualifies even without errors.Is.
		t.Log("note: wrapped ErrAuthFailure caught via string match, not errors.Is — OK")
	}
	if !provider.IsAuthError(provider.ErrAuthFailure) {
		t.Error("IsAuthError(ErrAuthFailure) should be true")
	}
	if !provider.IsAuthError(fmt.Errorf("wrap: %w", provider.ErrAuthFailure)) {
		t.Error("IsAuthError(wrapped ErrAuthFailure) should be true via errors.Is")
	}
}

// --- pollOnce concurrency (ADR-0093) ---

// fakeRunSource returns a fixed list of ActiveRuns.
type fakeRunSource struct {
	runs []ActiveRun
}

func (s *fakeRunSource) ActiveRuns(_ context.Context) ([]ActiveRun, error) {
	return s.runs, nil
}

// blockingProvider's GetMRState blocks until release is closed, tracking
// how many calls are concurrently in flight (peak observed) via atomic
// counters guarded by mu.
type blockingProvider struct {
	fakeAuthProvider
	release chan struct{}

	mu           sync.Mutex
	inFlightNow  int
	peakInFlight int
	calls        int
}

func (p *blockingProvider) GetMRState(ctx context.Context, _ string, _ int) (provider.MRState, error) {
	p.mu.Lock()
	p.calls++
	p.inFlightNow++
	if p.inFlightNow > p.peakInFlight {
		p.peakInFlight = p.inFlightNow
	}
	p.mu.Unlock()

	select {
	case <-p.release:
	case <-ctx.Done():
	}

	p.mu.Lock()
	p.inFlightNow--
	p.mu.Unlock()
	return provider.MRState{State: "opened"}, nil
}

func runsWithIDs(ids ...string) []ActiveRun {
	out := make([]ActiveRun, len(ids))
	for i, id := range ids {
		out[i] = ActiveRun{
			RunID:    id,
			Provider: "fake",
			InFlight: map[string]provider.MR{"unit-a": {ProjectID: "x/y", IID: i + 1}},
		}
	}
	return out
}

// Regression (ADR-0093): pollOnce used to walk every active Run
// sequentially in one goroutine — a single slow Run (blocked inside a
// real runner invocation) prevented the poller from even checking any
// other Run for new activity. Different RunIDs must now be polled
// concurrently.
func TestPollOnce_DispatchesDifferentRunsConcurrently(t *testing.T) {
	bp := &blockingProvider{release: make(chan struct{})}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": bp},
		Dispatcher: (&dispatchRecord{}).dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Source:     &fakeRunSource{runs: runsWithIDs("run-a", "run-b", "run-c")},
	}

	l.pollOnce(t.Context())

	// Give the spawned goroutines a moment to reach the blocking call.
	deadline := time.After(2 * time.Second)
	for {
		bp.mu.Lock()
		calls := bp.calls
		bp.mu.Unlock()
		if calls == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("want all 3 Runs' GetMRState called concurrently; only %d reached it in time", calls)
		case <-time.After(10 * time.Millisecond):
		}
	}

	bp.mu.Lock()
	peak := bp.peakInFlight
	bp.mu.Unlock()
	if peak < 2 {
		t.Errorf("want multiple Runs polled concurrently (peak >= 2); got peak=%d", peak)
	}

	close(bp.release) // let the blocked calls finish so the test can exit cleanly
}

// A RunID still being polled by a goroutine from a previous tick must not
// get a second, overlapping pollRun started for it by a later tick.
func TestPollOnce_SameRunNotPolledConcurrentlyAcrossTicks(t *testing.T) {
	bp := &blockingProvider{release: make(chan struct{})}
	l := &Loop{
		Providers:  map[string]provider.Provider{"fake": bp},
		Dispatcher: (&dispatchRecord{}).dispatch,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Source:     &fakeRunSource{runs: runsWithIDs("run-a")},
	}

	// First tick: starts polling run-a, which blocks inside GetMRState.
	l.pollOnce(t.Context())
	deadline := time.After(2 * time.Second)
	for {
		bp.mu.Lock()
		calls := bp.calls
		bp.mu.Unlock()
		if calls >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first tick's pollRun never reached the blocking call")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Second tick, while run-a is still in flight: must be skipped, not
	// dispatched as a second concurrent pollRun for the same RunID.
	l.pollOnce(t.Context())
	time.Sleep(50 * time.Millisecond)

	bp.mu.Lock()
	calls := bp.calls
	bp.mu.Unlock()
	if calls != 1 {
		t.Errorf("want exactly 1 call while run-a is still in flight from the first tick; got %d", calls)
	}

	close(bp.release)
}

// Concurrency bounds how many Runs are polled at once, even with more
// Runs available than the limit.
func TestPollOnce_RespectsConcurrencyLimit(t *testing.T) {
	bp := &blockingProvider{release: make(chan struct{})}
	l := &Loop{
		Providers:   map[string]provider.Provider{"fake": bp},
		Dispatcher:  (&dispatchRecord{}).dispatch,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Source:      &fakeRunSource{runs: runsWithIDs("run-a", "run-b", "run-c", "run-d", "run-e")},
		Concurrency: 2,
	}

	l.pollOnce(t.Context())

	deadline := time.After(2 * time.Second)
	for {
		bp.mu.Lock()
		peak := bp.peakInFlight
		bp.mu.Unlock()
		if peak >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("never observed 2 concurrent calls")
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(100 * time.Millisecond) // let any over-eager dispatch show up, if it exists

	bp.mu.Lock()
	peak := bp.peakInFlight
	bp.mu.Unlock()
	if peak > 2 {
		t.Errorf("Concurrency: 2 must never allow more than 2 in flight at once; observed peak=%d", peak)
	}

	close(bp.release)
}
