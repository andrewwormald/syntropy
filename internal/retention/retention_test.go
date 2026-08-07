package retention

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/refactorsweep"
	"github.com/andrewwormald/syntropy/internal/store"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func freshBackend(t *testing.T) *store.Backend {
	t.Helper()
	b, err := store.OpenSqlite(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func storeRun(t *testing.T, rs *store.RecordStore, runID string, runState workflow.RunState, state refactorsweep.AgentState) {
	t.Helper()
	obj, err := workflow.Marshal(&state)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	rec := &workflow.Record{
		WorkflowName: "retention-test",
		ForeignID:    "fid-" + runID,
		RunID:        runID,
		RunState:     runState,
		Status:       1,
		Object:       obj,
	}
	if err := rs.Store(t.Context(), rec); err != nil {
		t.Fatalf("Store: %v", err)
	}
}

// fakeGit records RemoveWorktree calls; every other method panics if called,
// since the sweeper should never touch them.
type fakeGit struct {
	removed []string
	err     error
}

func (g *fakeGit) EnsureBranch(context.Context, string, string, string, string) error {
	panic("not used by retention")
}
func (g *fakeGit) CheckoutExistingBranch(context.Context, string, string, string) error {
	panic("not used by retention")
}
func (g *fakeGit) HardReset(context.Context, string, string) error { panic("not used by retention") }
func (g *fakeGit) DiscardUncommitted(context.Context, string) error {
	panic("not used by retention")
}
func (g *fakeGit) HasChanges(context.Context, string) (bool, error) {
	panic("not used by retention")
}
func (g *fakeGit) HasWorkBeyondBase(context.Context, string, string) (bool, error) {
	panic("not used by retention")
}
func (g *fakeGit) Commit(context.Context, string, string) error { panic("not used by retention") }
func (g *fakeGit) Push(context.Context, string, string) error   { panic("not used by retention") }
func (g *fakeGit) RemoveWorktree(_ context.Context, _, dir string) error {
	g.removed = append(g.removed, dir)
	return g.err
}
func (g *fakeGit) SyncWithBase(context.Context, string, string) error {
	panic("not used by retention")
}
func (g *fakeGit) ConflictedFiles(context.Context, string) ([]string, error) {
	panic("not used by retention")
}
func (g *fakeGit) DiffShortstat(context.Context, string, string) (string, error) {
	panic("not used by retention")
}
func (g *fakeGit) IsIsolatedWorktree(context.Context, string) (bool, error) {
	panic("not used by retention")
}

func TestSweepOnce_DeletesOldTerminalRunsOnly(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	storeRun(t, rs, "run-old-completed", workflow.RunStateCompleted, refactorsweep.AgentState{})
	storeRun(t, rs, "run-running", workflow.RunStateRunning, refactorsweep.AgentState{})
	time.Sleep(time.Millisecond)

	s := &Sweeper{Store: rs, RetentionPeriod: time.Millisecond, Logger: discardLogger}
	s.sweepOnce(t.Context())

	if _, err := rs.Lookup(t.Context(), "run-old-completed"); err != workflow.ErrRecordNotFound {
		t.Errorf("expected terminal run to be deleted, got err=%v", err)
	}
	if _, err := rs.Lookup(t.Context(), "run-running"); err != nil {
		t.Errorf("expected running run to survive, got err=%v", err)
	}
}

func TestSweepOnce_RetentionPeriodDisabledIsNoop(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	storeRun(t, rs, "run-completed", workflow.RunStateCompleted, refactorsweep.AgentState{})

	s := &Sweeper{Store: rs, RetentionPeriod: 0, Logger: discardLogger}
	s.sweepOnce(t.Context())

	if _, err := rs.Lookup(t.Context(), "run-completed"); err != nil {
		t.Errorf("expected run to survive with retention disabled, got err=%v", err)
	}
}

func TestSweepRun_RemovesRunDirAndWorktrees(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()
	runsRoot := t.TempDir()

	const runID = "run-with-worktrees"
	storeRun(t, rs, runID, workflow.RunStateCompleted, refactorsweep.AgentState{
		BaseRepo: "/base/repo",
		InFlight: map[string]provider.MR{"unit-a": {}},
	})

	runDir := filepath.Join(runsRoot, runID)
	if err := os.MkdirAll(filepath.Join(runDir, "worktrees", "unit-a"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "planning"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	time.Sleep(time.Millisecond)
	g := &fakeGit{}
	s := &Sweeper{Store: rs, Git: g, RunsRoot: runsRoot, RetentionPeriod: time.Millisecond, Logger: discardLogger}
	s.sweepOnce(t.Context())

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("expected run dir %s to be removed, stat err=%v", runDir, err)
	}
	wantRemoved := []string{
		filepath.Join(runDir, "worktrees", "unit-a"),
		filepath.Join(runDir, "planning"),
	}
	for _, want := range wantRemoved {
		found := false
		for _, got := range g.removed {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected RemoveWorktree(%s), got calls %v", want, g.removed)
		}
	}
}

func TestSweepRun_NoRunsRootSkipsFilesystemCleanup(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	storeRun(t, rs, "run-no-fs", workflow.RunStateCompleted, refactorsweep.AgentState{})
	time.Sleep(time.Millisecond)

	s := &Sweeper{Store: rs, RetentionPeriod: time.Millisecond, Logger: discardLogger}
	s.sweepOnce(t.Context())

	if _, err := rs.Lookup(t.Context(), "run-no-fs"); err != workflow.ErrRecordNotFound {
		t.Errorf("expected run to still be deleted, got err=%v", err)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	b := freshBackend(t)
	rs := b.RecordStore()

	s := &Sweeper{Store: rs, Interval: time.Millisecond, RetentionPeriod: time.Hour, Logger: discardLogger}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
