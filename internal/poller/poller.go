// Package poller drives event ingress by polling the provider's API
// instead of receiving webhooks. See ADR-0031 for the rationale.
//
// "Never polls" (the everflow brand) is about LLM tokens, not provider
// API calls. Polling glab/gh costs zero tokens — the latency penalty
// (seconds vs minutes) is acceptable for refactor sweeps that run over
// hours/days.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/provider"
)

// authBackoffEntry tracks consecutive auth failures for one Run. The poller
// skips a Run until until has passed, preventing hammering on an expired token.
type authBackoffEntry struct {
	failures int
	until    time.Time
}

// authBackoffDuration returns the wait time after n consecutive auth failures.
// Schedule: 30s → 2m → 8m → 32m → 2h (capped). This gives rapid first
// feedback that the token is broken while avoiding log noise for long outages.
func authBackoffDuration(failures int) time.Duration {
	d := 30 * time.Second
	for i := 0; i < failures; i++ {
		d *= 4
		if d > 2*time.Hour {
			return 2 * time.Hour
		}
	}
	return d
}

// EventDispatcher is the function shape main.go's webhook dispatcher
// also satisfies — synthesised events flow through the same path.
type EventDispatcher func(ctx context.Context, runID string, event provider.Event) error

// RunSource enumerates active Runs for the poller to walk each tick.
// Implementations typically wrap the workflow.RecordStore.
type RunSource interface {
	ActiveRuns(ctx context.Context) ([]ActiveRun, error)
}

// ActiveRun is a snapshot of a Run the poller needs to inspect.
type ActiveRun struct {
	RunID     string
	ForeignID string
	Provider  string
	ProjectID string
	Author    provider.User // for IsAuthor classification when synthesising events
	InFlight  map[string]provider.MR
	// LastSeenNoteIDs maps MR IID → highest note ID we've already
	// processed, across all comment streams merged (the pre-ADR-0041
	// scalar). Read at poll-start; the SaveSnapshot callback persists
	// updates. Used as provider.NoteCursor.Legacy — the floor for any
	// stream not yet present in LastSeenNoteCursors.
	LastSeenNoteIDs map[int]int64
	// LastSeenNoteCursors maps MR IID → per-stream high-water mark (see
	// provider.NoteCursor and AgentState.LastSeenNoteIDsByStream).
	LastSeenNoteCursors map[int]map[string]int64
	LastMRStates        map[int]string
}

// SaveSnapshot is called after each successful poll for a Run to persist
// the updated LastSeenNoteIDs, LastSeenNoteCursors, and LastMRStates on
// AgentState. Typically triggers a workflow.Callback no-op transition so
// the values flush to the durable store.
type SaveSnapshot func(ctx context.Context, runID string, noteIDs map[int]int64, noteCursors map[int]map[string]int64, mrStates map[int]string) error

// Loop runs in a goroutine. It ticks every interval, walks active Runs,
// queries the provider for changes since the last snapshot, and synthesises
// provider.Event values that it dispatches via the same path webhooks use.
//
// Returns when ctx is cancelled.
type Loop struct {
	Interval    time.Duration
	Providers   map[string]provider.Provider
	Source      RunSource
	Dispatcher  EventDispatcher
	SaveSnapshot SaveSnapshot
	Logger      *slog.Logger

	// authBackoff tracks per-Run auth-failure state. Protected by authMu.
	// Lazily initialised on first auth error.
	authMu      sync.Mutex
	authBackoff map[string]authBackoffEntry
}

func (l *Loop) Run(ctx context.Context) {
	if l.Interval <= 0 {
		l.Interval = 30 * time.Second
	}
	t := time.NewTicker(l.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.pollOnce(ctx)
		}
	}
}

func (l *Loop) pollOnce(ctx context.Context) {
	runs, err := l.Source.ActiveRuns(ctx)
	if err != nil {
		l.Logger.Warn("poller: list active runs", "err", err)
		return
	}
	for _, r := range runs {
		l.pollRun(ctx, r)
	}
}

func (l *Loop) pollRun(ctx context.Context, r ActiveRun) {
	p, ok := l.Providers[r.Provider]
	if !ok {
		return // unknown provider — skip silently
	}

	// Skip if still in auth-failure backoff window.
	l.authMu.Lock()
	entry := l.authBackoff[r.RunID]
	l.authMu.Unlock()
	if time.Now().Before(entry.until) {
		return
	}

	// Per-Run snapshot buffers; persisted at end via SaveSnapshot.
	noteIDs := copyInt64Map(r.LastSeenNoteIDs)
	noteCursors := copyNoteCursorMap(r.LastSeenNoteCursors)
	mrStates := copyStringMap(r.LastMRStates)
	updated := false
	hadAuthErr := false
	var mu sync.Mutex // for any future concurrent polls per Run; currently serial

	for _, mr := range r.InFlight {
		// 1. MR state delta?
		state, err := p.GetMRState(ctx, mr.ProjectID, mr.IID)
		if err != nil {
			if provider.IsAuthError(err) {
				hadAuthErr = true
				l.Logger.Warn("poller: GetMRState auth failure; backing off",
					"run_id", r.RunID, "mr_iid", mr.IID, "err", err)
				break // stop all MR polling for this run this tick
			}
			l.Logger.Warn("poller: GetMRState", "run_id", r.RunID, "mr_iid", mr.IID, "err", err)
			continue
		}
		prev := mrStates[mr.IID]
		ev := mrStateEvent(state, mr)
		if ev.Kind != "" {
			// Terminal state (merged/closed). Dispatch every tick this unit
			// is still in InFlight — NOT gated on state != prev. Found
			// live: caching mrStates[mr.IID] = state the instant a change
			// is *detected* (regardless of whether resume() actually
			// *processed* it — e.g. the Run was Paused at that exact
			// moment, which silently drops all events including this one)
			// permanently stops any future re-check, since the next tick's
			// state also equals the now-cached prev. The only real proof
			// the workflow applied the transition is markUnitMerged/
			// markUnitBlacklisted removing the unit from InFlight — and
			// since ActiveRuns is re-read fresh every tick, that already
			// self-terminates this loop naturally once it actually lands;
			// redispatching a few extra times before then is harmless
			// (resume() no-ops on a unit it's already completed), while
			// silently giving up after one dropped attempt is not.
			if state != prev {
				mu.Lock()
				mrStates[mr.IID] = state
				updated = true
				mu.Unlock()
			}
			ev.IsAuthor = false // MR-state events aren't from the author
			if err := l.Dispatcher(ctx, r.RunID, ev); err != nil {
				l.Logger.Warn("poller: dispatch MR state event", "err", err)
			}
			// Don't poll notes for it this tick — resume() will move the
			// unit out of InFlight once it actually processes the event.
			continue
		}
		if state != prev {
			mu.Lock()
			mrStates[mr.IID] = state
			updated = true
			mu.Unlock()
		}

		// 2. New comments since last seen? Per-stream cursor with a
		// fallback to the legacy scalar for any stream not yet tracked
		// individually (ADR-0041) — see provider.NoteCursor. ByStream
		// aliases noteCursors[mr.IID] (mutated below once notes come
		// back) — safe because the provider only reads it during this
		// call, before any mutation happens.
		since := provider.NoteCursor{ByStream: noteCursors[mr.IID], Legacy: noteIDs[mr.IID]}
		notes, err := p.ListNotesSince(ctx, mr.ProjectID, mr.IID, since)
		if err != nil {
			if provider.IsAuthError(err) {
				hadAuthErr = true
				l.Logger.Warn("poller: ListNotesSince auth failure; backing off",
					"run_id", r.RunID, "mr_iid", mr.IID, "err", err)
				break
			}
			l.Logger.Warn("poller: ListNotesSince", "run_id", r.RunID, "mr_iid", mr.IID, "err", err)
			continue
		}
		for _, n := range notes {
			mu.Lock()
			if n.ID > noteIDs[mr.IID] {
				noteIDs[mr.IID] = n.ID
				updated = true
			}
			if n.Stream != "" {
				if noteCursors[mr.IID] == nil {
					noteCursors[mr.IID] = map[string]int64{}
				}
				if n.ID > noteCursors[mr.IID][n.Stream] {
					noteCursors[mr.IID][n.Stream] = n.ID
					updated = true
				}
			}
			mu.Unlock()
			ev := provider.Event{
				Kind:      provider.EventNoteAdded,
				ProjectID: mr.ProjectID,
				MR:        mr,
				Author:    n.Author,
				IsBot:     n.Author.Bot,
				Note:      provider.Note{ID: n.ID, Body: n.Body, DiscussionID: n.DiscussionID, Stream: n.Stream},
				IsAuthor:  strings.EqualFold(n.Author.Handle, r.Author.Handle) && r.Author.Handle != "",
				ReceivedAt: time.Now().UnixNano(),
			}
			if err := l.Dispatcher(ctx, r.RunID, ev); err != nil {
				l.Logger.Warn("poller: dispatch note event", "err", err)
			}
		}
	}

	// Update auth-failure backoff state. On an auth error, extend the
	// backoff window. On a clean tick, reset the counter so a token rotation
	// restores normal polling immediately.
	//
	// Synthetic auth events are dispatched outside the mutex to avoid
	// deadlocking if Dispatcher calls back into the poller.
	var authEventToDispatch provider.EventKind
	l.authMu.Lock()
	if hadAuthErr {
		if l.authBackoff == nil {
			l.authBackoff = make(map[string]authBackoffEntry)
		}
		e := l.authBackoff[r.RunID]
		e.failures++
		e.until = time.Now().Add(authBackoffDuration(e.failures))
		l.authBackoff[r.RunID] = e
		l.Logger.Warn("poller: auth backoff set",
			"run_id", r.RunID, "failures", e.failures, "until", e.until.Format(time.RFC3339))
		if e.failures == 1 {
			// First failure — notify the state machine so it can park the Run
			// and post an explanatory comment on the in-flight MR.
			authEventToDispatch = provider.EventProviderAuthFailure
		}
	} else if entry.failures > 0 {
		// Successful tick after prior auth failures — reset.
		delete(l.authBackoff, r.RunID)
		l.Logger.Info("poller: auth backoff cleared after successful tick", "run_id", r.RunID)
		authEventToDispatch = provider.EventProviderAuthRestored
	}
	l.authMu.Unlock()

	if authEventToDispatch != "" {
		authEv := provider.Event{
			Kind:       authEventToDispatch,
			ReceivedAt: time.Now().UnixNano(),
		}
		if err := l.Dispatcher(ctx, r.RunID, authEv); err != nil {
			l.Logger.Warn("poller: dispatch auth event", "run_id", r.RunID, "kind", authEventToDispatch, "err", err)
		}
	}

	if updated && l.SaveSnapshot != nil {
		if err := l.SaveSnapshot(ctx, r.RunID, noteIDs, noteCursors, mrStates); err != nil {
			l.Logger.Warn("poller: save snapshot", "run_id", r.RunID, "err", err)
		}
	}
}

// mrStateEvent maps a GitLab MR state string to the provider.Event we'd
// have received from a webhook for the corresponding action.
func mrStateEvent(state string, mr provider.MR) provider.Event {
	switch state {
	case "merged":
		return provider.Event{Kind: provider.EventMRMerged, ProjectID: mr.ProjectID, MR: mr, ReceivedAt: time.Now().UnixNano()}
	case "closed":
		return provider.Event{Kind: provider.EventMRClosed, ProjectID: mr.ProjectID, MR: mr, ReceivedAt: time.Now().UnixNano()}
	default:
		// "opened" / "locked" / unknown → no event to dispatch
		return provider.Event{}
	}
}

// --- workflow.RecordStore-backed RunSource ---

// StoreSource implements RunSource against a workflow.RecordStore.
// Reads active Runs (RunState != finished AND AgentStatus active) and
// unmarshals AgentState to extract poll state.
type StoreSource struct {
	Store        workflow.RecordStore
	WorkflowName string
	Decode       func([]byte) (ActiveRun, bool) // domain-specific Object unmarshaller
}

func (s *StoreSource) ActiveRuns(ctx context.Context) ([]ActiveRun, error) {
	const pageSize = 200
	var (
		offset int64
		out    []ActiveRun
	)
	for {
		records, err := s.Store.List(ctx, s.WorkflowName, offset, pageSize, workflow.OrderTypeAscending)
		if err != nil {
			return nil, fmt.Errorf("list records at offset %d: %w", offset, err)
		}
		if len(records) == 0 {
			break
		}
		for _, rec := range records {
			if rec.RunState.Finished() {
				continue
			}
			ar, ok := s.Decode(rec.Object)
			if !ok {
				continue
			}
			ar.RunID = rec.RunID
			ar.ForeignID = rec.ForeignID
			out = append(out, ar)
		}
		if int64(len(records)) < pageSize {
			break
		}
		offset += int64(len(records))
	}
	return out, nil
}

// --- helpers ---

func copyInt64Map(m map[int]int64) map[int]int64 {
	out := make(map[int]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyStringMap(m map[int]string) map[int]string {
	out := make(map[int]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyNoteCursorMap(m map[int]map[string]int64) map[int]map[string]int64 {
	out := make(map[int]map[string]int64, len(m))
	for k, v := range m {
		out[k] = copyInt64StringKeyedMap(v)
	}
	return out
}

func copyInt64StringKeyedMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
