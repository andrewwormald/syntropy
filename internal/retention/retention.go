// Package retention periodically deletes terminal Runs (Completed or
// Cancelled — see internal/store.ListTerminalRuns) that have sat untouched
// past a configurable retention period, removing both their store rows and
// their on-disk run directory. See ADR-0070 for the storage-layer half of
// this and ADR-0071 for the sweeper loop itself.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/git"
	"github.com/andrewwormald/syntropy/internal/refactorsweep"
	"github.com/andrewwormald/syntropy/internal/store"
)

// defaultInterval is how often Sweeper.Run ticks. Retention cleanup has no
// latency requirement (unlike internal/reconciler's stuck-Run detection) —
// once an Run is old enough to sweep, sweeping it an hour later instead of a
// minute later is immaterial — so this is a fixed constant rather than a
// daemon flag, matching internal/poller's hardcoded 30s Interval.
// risk-screen verification placeholder, safe to ignore/revert
const defaultInterval = time.Hour

// Store is the subset of *store.RecordStore the sweeper needs: finding
// terminal Runs, reading a Run's AgentState to locate its worktrees, and
// deleting the Run's rows once cleanup is done.
type Store interface {
	ListTerminalRuns(ctx context.Context, olderThan time.Time) ([]store.TerminalRun, error)
	Lookup(ctx context.Context, runID string) (*workflow.Record, error)
	DeleteRun(ctx context.Context, runID string) error
}

// Sweeper periodically finds terminal Runs older than RetentionPeriod and
// removes them: their git worktrees, their on-disk run directory, and their
// store rows (records/timeouts/outbox/event_log via Store.DeleteRun). It
// ticks on Interval (defaulting to defaultInterval) for as long as Run's ctx
// is live.
type Sweeper struct {
	Store           Store
	Git             git.Git
	RunsRoot        string
	RetentionPeriod time.Duration
	Interval        time.Duration
	Logger          *slog.Logger
}

// Run ticks every s.Interval, sweeping eligible Runs each tick. It returns
// when ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = defaultInterval
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

// sweepOnce lists Runs terminal as of now-RetentionPeriod and removes each
// one. A RetentionPeriod <= 0 disables the sweep entirely — nothing is ever
// old enough to be swept — rather than treating it as "no cutoff" and
// deleting every terminal Run on the first tick.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	if s.RetentionPeriod <= 0 {
		return
	}

	cutoff := time.Now().Add(-s.RetentionPeriod)
	runs, err := s.Store.ListTerminalRuns(ctx, cutoff)
	if err != nil {
		s.Logger.Warn("retention: list terminal runs", "err", err)
		return
	}

	for _, run := range runs {
		s.sweepRun(ctx, run)
	}
}

// sweepRun removes run's on-disk directory (best-effort) and then deletes
// its store rows. Filesystem cleanup happens first and is best-effort so a
// failure there (e.g. a permissions issue) doesn't block the row deletion
// that actually stops the Run from being listed again next tick.
func (s *Sweeper) sweepRun(ctx context.Context, run store.TerminalRun) {
	s.removeRunDir(ctx, run.RunID)

	if err := s.Store.DeleteRun(ctx, run.RunID); err != nil {
		s.Logger.Warn("retention: delete run", "run_id", run.RunID, "workflow", run.WorkflowName, "err", err)
		return
	}
	s.Logger.Info("retention: swept run", "run_id", run.RunID, "workflow", run.WorkflowName, "updated_at", run.UpdatedAt)
}

// removeRunDir tears down runID's git worktrees (best-effort, so a
// registration is properly removed from BaseRepo's .git rather than left
// dangling) and then removes the whole run directory. It's a no-op if
// RunsRoot is unset (e.g. the in-memory store used by tests/the v0
// scaffold, which has no on-disk run layout).
func (s *Sweeper) removeRunDir(ctx context.Context, runID string) {
	if s.RunsRoot == "" {
		return
	}
	runDir := filepath.Join(s.RunsRoot, runID)

	if s.Git != nil {
		s.removeWorktrees(ctx, runID, runDir)
	}

	if err := os.RemoveAll(runDir); err != nil {
		s.Logger.Warn("retention: remove run dir", "run_id", runID, "dir", runDir, "err", err)
	}
}

// removeWorktrees decodes runID's AgentState to find BaseRepo and any
// worktrees still registered against it (InFlight units, plus the planning
// worktree left by spec-mode Runs) and removes each via Git.RemoveWorktree.
// By the time a Run reaches a terminal state its worktrees are normally
// already gone (removed as each unit completes — see
// internal/refactorsweep/workflow.go), so this is belt-and-braces for a Run
// that crashed mid-flight before its own cleanup ran, mirroring the
// `abandon` command's best-effort worktree removal in main.go.
func (s *Sweeper) removeWorktrees(ctx context.Context, runID, runDir string) {
	rec, err := s.Store.Lookup(ctx, runID)
	if err != nil {
		if !errors.Is(err, workflow.ErrRecordNotFound) {
			s.Logger.Warn("retention: lookup run for worktree cleanup", "run_id", runID, "err", err)
		}
		return
	}

	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		s.Logger.Warn("retention: unmarshal run state for worktree cleanup", "run_id", runID, "err", err)
		return
	}
	if state.BaseRepo == "" {
		return
	}

	for unitID := range state.InFlight {
		worktree := filepath.Join(runDir, "worktrees", unitID)
		if err := s.Git.RemoveWorktree(ctx, state.BaseRepo, worktree); err != nil {
			s.Logger.Warn("retention: remove worktree", "run_id", runID, "unit_id", unitID, "err", err)
		}
	}

	planningDir := filepath.Join(runDir, "planning")
	if _, err := os.Stat(planningDir); err == nil {
		if err := s.Git.RemoveWorktree(ctx, state.BaseRepo, planningDir); err != nil {
			s.Logger.Warn("retention: remove planning worktree", "run_id", runID, "err", err)
		}
	}
}
