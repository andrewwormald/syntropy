// Package reconciler detects Runs stuck on a lost in-memory event: the
// sync.Cond EventStreamer (ADR-0033) has no durable queue, so an event
// dropped between a daemon restart and its delivery leaves the Run's
// AgentState in StatusWorking or StatusDiscovering forever, with nothing
// to wake it back up. See DESIGN.md § "The state machine".
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/refactorsweep"
)

// IsStuck reports whether a Run sitting in status since lastProgress has
// gone stale: only StatusWorking and StatusDiscovering are in-flight states
// that depend on an in-memory event to advance, so any other status is
// never considered stuck. now and lastProgress are passed in rather than
// read from time.Now() so callers (and tests) control elapsed time.
func IsStuck(status refactorsweep.AgentStatus, lastProgress time.Time, now time.Time, threshold time.Duration) bool {
	switch status {
	case refactorsweep.StatusWorking, refactorsweep.StatusDiscovering:
	default:
		return false
	}
	return now.Sub(lastProgress) >= threshold
}

// LastProgress returns when state last made progress: the EndedAt of the
// last Turn in state.History (or its StartedAt if still in-flight,
// EndedAt zero), but never earlier than recordUpdatedAt — the Run
// record's own store-level UpdatedAt, which advances on every write to
// the record, including one that doesn't append a Turn at all (e.g.
// directResume reviving a Failed/Cancelled Run back to Discovering).
//
// Found live: a Run resumed from Failed days after its last real Turn
// was immediately re-flagged as stuck by the reconciler, because History
// still pointed at that days-old timestamp — the resume itself wasn't
// recognised as "something happened here" at all. The reconciler then
// re-triggered it repeatedly (every RetriggerCooldown window) for the
// ~25 minutes it took a fresh Turn to actually land and update History,
// looking exactly like a Run resurrecting and dying in a loop
// ("zombie" behaviour) even though it was making real, if slow,
// progress the whole time (see ADR-0090).
//
// If History is empty, recordUpdatedAt is returned directly — a Run
// with no turns yet has nothing else to derive progress from.
func LastProgress(state refactorsweep.AgentState, recordUpdatedAt time.Time) time.Time {
	progress := recordUpdatedAt
	if len(state.History) > 0 {
		turn := state.History[len(state.History)-1]
		turnProgress := turn.StartedAt
		if !turn.EndedAt.IsZero() {
			turnProgress = turn.EndedAt
		}
		if turnProgress.After(progress) {
			progress = turnProgress
		}
	}
	return progress
}

// scanPageSize is the page size used when paginating through the
// RecordStore in Scan, mirroring rehydrateSecrets in main.go.
const scanPageSize = 200

// Scan queries rs for Runs in RunStateRunning (excluding Paused, which is a
// deliberate stop rather than a lost event — see Pause in the workflow
// library) and returns the RunIDs of those IsStuck flags as stale. now is
// passed in rather than read from time.Now() so callers control elapsed
// time in tests.
func Scan(ctx context.Context, rs workflow.RecordStore, workflowName string, now time.Time, threshold time.Duration) ([]string, error) {
	var stuck []string
	var offset int64
	for {
		records, err := rs.List(ctx, workflowName, offset, scanPageSize, workflow.OrderTypeAscending,
			workflow.FilterByRunState(workflow.RunStateRunning))
		if err != nil {
			return nil, fmt.Errorf("list records at offset %d: %w", offset, err)
		}
		if len(records) == 0 {
			break
		}
		for _, rec := range records {
			status := refactorsweep.AgentStatus(rec.Status)
			var state refactorsweep.AgentState
			if err := workflow.Unmarshal(rec.Object, &state); err != nil {
				continue
			}
			lastProgress := LastProgress(state, rec.UpdatedAt)
			if IsStuck(status, lastProgress, now, threshold) {
				stuck = append(stuck, rec.RunID)
			}
		}
		if int64(len(records)) < scanPageSize {
			break
		}
		offset += int64(len(records))
	}
	return stuck, nil
}

// Retrigger builds and sends an event for record to its current status
// topic, carrying the same headers a normal transition would produce (see
// MakeOutboxEventData in the vendored library's event.go). Because the
// event's HeaderRecordVersion is taken from record.Meta.Version, the step
// consumer's existing idempotency guard (step.go's stepConsumer, which
// skips any event whose HeaderRecordVersion doesn't match the record's
// current version) applies unchanged: retriggering a record more than
// once, or retriggering a stale snapshot after the record has since
// advanced, is a no-op rather than a double-process.
func Retrigger(ctx context.Context, streamer workflow.EventStreamer, record workflow.Record) error {
	topic := workflow.Topic(record.WorkflowName, record.Status)

	headers := map[workflow.Header]string{
		workflow.HeaderForeignID:     record.ForeignID,
		workflow.HeaderWorkflowName:  record.WorkflowName,
		workflow.HeaderTopic:         topic,
		workflow.HeaderRunID:         record.RunID,
		workflow.HeaderRunState:      strconv.FormatInt(int64(record.RunState), 10),
		workflow.HeaderRecordVersion: strconv.FormatInt(int64(record.Meta.Version), 10),
	}

	sender, err := streamer.NewSender(ctx, topic)
	if err != nil {
		return fmt.Errorf("new sender for topic %q: %w", topic, err)
	}
	defer sender.Close()

	// The step consumer looks up records by RunID (see stepConsumer in the
	// vendored library's step.go), so despite the Event.ForeignID field
	// name, the value sent here must be the RunID — mirroring how
	// purgeOutbox in outbox.go sends outboxRecord.RunId, not the business
	// ForeignID, as the event's foreign ID.
	if err := sender.Send(ctx, record.RunID, record.Status, headers); err != nil {
		return fmt.Errorf("send retrigger event: %w", err)
	}
	return nil
}

// Sweeper periodically sweeps a workflow's Runs for ones stuck on a lost
// in-memory event (see the package doc) and re-triggers them. It ticks on
// Interval (defaulting to 30s, matching internal/poller's cadence) for as
// long as Run's ctx is live.
type Sweeper struct {
	Store        workflow.RecordStore
	Streamer     workflow.EventStreamer
	WorkflowName string
	Interval     time.Duration
	Threshold    time.Duration
	Logger       *slog.Logger

	// RetriggerCooldown is how long a RunID is left alone after being
	// re-triggered, even if it's still flagged stuck by the next sweep: a
	// re-trigger can only wake up a lost event, it can't make a genuinely
	// wedged step finish any sooner, so re-sending every tick just spams
	// the topic. The zero value disables the cooldown (every stuck Run is
	// re-triggered on every sweep), preserving the pre-cooldown behaviour
	// for callers that don't set it.
	RetriggerCooldown time.Duration

	// cooldowns tracks, per RunID, the state observed at the last
	// retrigger. It's only ever read and written from sweepOnce, which
	// Run calls serially from a single goroutine.
	cooldowns map[string]cooldownEntry
}

// cooldownEntry records the state of a RunID at the time it was last
// re-triggered, so a later sweep can tell whether the cooldown still
// applies (lastProgress unchanged) or whether the Run has since made
// progress and gotten stuck again (lastProgress advanced), which resets
// eligibility immediately regardless of how long ago retriggeredAt was.
type cooldownEntry struct {
	retriggeredAt time.Time
	lastProgress  time.Time
}

// Run ticks every s.Interval, sweeping stuck Runs each tick. It returns when
// ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 30 * time.Second
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}

	t := time.NewTicker(s.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce runs a single Scan + Retrigger pass over s.WorkflowName,
// skipping any stuck RunID still within its RetriggerCooldown unless it has
// made fresh progress (a new turn) since the retrigger that started that
// cooldown.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	now := time.Now()

	stuck, err := Scan(ctx, s.Store, s.WorkflowName, now, s.Threshold)
	if err != nil {
		s.Logger.Warn("reconciler: scan", "err", err)
		return
	}

	if s.cooldowns == nil {
		s.cooldowns = make(map[string]cooldownEntry)
	}

	for _, runID := range stuck {
		record, err := s.Store.Lookup(ctx, runID)
		if err != nil {
			s.Logger.Warn("reconciler: lookup stuck run", "run_id", runID, "err", err)
			continue
		}

		var state refactorsweep.AgentState
		if err := workflow.Unmarshal(record.Object, &state); err != nil {
			s.Logger.Warn("reconciler: unmarshal stuck run state", "run_id", runID, "err", err)
			continue
		}
		lastProgress := LastProgress(state, record.CreatedAt)

		if entry, ok := s.cooldowns[runID]; ok && !lastProgress.After(entry.lastProgress) &&
			now.Sub(entry.retriggeredAt) < s.RetriggerCooldown {
			s.Logger.Info("reconciler: skipping stuck run in cooldown", "run_id", runID)
			continue
		}

		if err := Retrigger(ctx, s.Streamer, *record); err != nil {
			s.Logger.Warn("reconciler: retrigger stuck run", "run_id", runID, "err", err)
			continue
		}
		s.cooldowns[runID] = cooldownEntry{retriggeredAt: now, lastProgress: lastProgress}
		s.Logger.Info("reconciler: re-triggered stuck run", "run_id", runID)
	}
}
