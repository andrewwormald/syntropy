package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/config"
	"github.com/andrewwormald/syntropy/internal/eventstream"
	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/refactorsweep"
	"github.com/andrewwormald/syntropy/internal/runner"
	"github.com/andrewwormald/syntropy/internal/store"
)

// captureStdout redirects os.Stdout to a pipe and returns a function that
// restores it and returns the captured output as a string.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	return func() string {
		w.Close()
		os.Stdout = old
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			t.Fatalf("io.Copy: %v", err)
		}
		return buf.String()
	}
}

// seedStore creates a temp sqlite store and inserts a single Record whose
// AgentState encodes the given state. Returns the store path.
func seedStore(t *testing.T, runID string, state refactorsweep.AgentState) string {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "store.db")

	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	obj, err := workflow.Marshal(&state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := &workflow.Record{
		WorkflowName: workflowName,
		ForeignID:    "test-foreign-id",
		RunID:        runID,
		RunState:     workflow.RunStateRunning,
		Status:       int(refactorsweep.StatusWorking),
		Object:       obj,
		UpdatedAt:    time.Now(),
	}
	if err := rs.Store(context.Background(), rec); err != nil {
		t.Fatalf("store.Store: %v", err)
	}
	return sp
}

// seedStoreMulti creates a temp sqlite store seeded with one Record per
// runID at the given status. Used by prefix-matching tests.
func seedStoreMulti(t *testing.T, runIDs []string, status refactorsweep.AgentStatus) string {
	t.Helper()
	sp := filepath.Join(t.TempDir(), "store.db")
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for i, rid := range runIDs {
		obj, err := workflow.Marshal(&refactorsweep.AgentState{Goal: "seed"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := &workflow.Record{
			WorkflowName: workflowName,
			ForeignID:    fmt.Sprintf("fid-%d", i),
			RunID:        rid,
			RunState:     workflow.RunStateRunning,
			Status:       int(status),
			Object:       obj,
			UpdatedAt:    time.Now(),
		}
		if err := rs.Store(context.Background(), rec); err != nil {
			t.Fatalf("store.Store: %v", err)
		}
	}
	return sp
}

func TestVersionString(t *testing.T) {
	orig := version
	origCommit := gitCommit
	origBuild := buildTime
	t.Cleanup(func() {
		version = orig
		gitCommit = origCommit
		buildTime = origBuild
	})

	version = "1.2.3"
	gitCommit = "abc1234"
	buildTime = "2026-07-03T12:00:00Z"

	got := versionString()
	want := "syntropy 1.2.3 (commit: abc1234, built: 2026-07-03T12:00:00Z)"
	if got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}
}

func TestDirectStatus_PrintsRunSummary(t *testing.T) {
	runID := "aaaaaaaa-0000-0000-0000-000000000001"
	state := refactorsweep.AgentState{
		Goal:         "migrate the acme/example service",
		ProviderName: "github",
		ProjectID:    "acme/example",
		TotalTokens:  42000,
		Budget:       runner.Budget{MaxTokens: 100000},
	}
	sp := seedStore(t, runID, state)

	flush := captureStdout(t)
	err := directStatus(context.Background(), sp, runID)
	out := flush()

	if err != nil {
		t.Fatalf("directStatus: %v", err)
	}
	for _, want := range []string{
		runID,
		"migrate the acme/example service",
		"github",
		"acme/example",
		"42000",
		"100000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n\nfull output:\n%s", want, out)
		}
	}
}

func TestDirectList(t *testing.T) {
	type seedRun struct {
		runID string
		state refactorsweep.AgentState
	}
	tests := []struct {
		name     string
		seeds    []seedRun
		wantOut  []string
		wantNone bool
	}{
		{
			name:     "empty store",
			seeds:    nil,
			wantNone: true,
		},
		{
			name: "single run",
			seeds: []seedRun{
				{
					runID: "aaaaaaaa-1111-0000-0000-000000000001",
					state: refactorsweep.AgentState{
						Goal: "migrate the acme service",
						Mode: "spec",
					},
				},
			},
			wantOut: []string{"aaaaaaaa-1111...", "spec", "migrate the acme service"},
		},
		{
			name: "multi run",
			seeds: []seedRun{
				{
					runID: "bbbbbbbb-0001-0000-0000-000000000001",
					state: refactorsweep.AgentState{Goal: "fix the alpha bug", Mode: "sweep"},
				},
				{
					runID: "cccccccc-0002-0000-0000-000000000002",
					state: refactorsweep.AgentState{Goal: "add beta feature", Mode: "spec"},
				},
			},
			wantOut: []string{"fix the alpha bug", "add beta feature", "bbbbbbbb-0001...", "cccccccc-0002..."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sp := filepath.Join(dir, "store.db")

			if len(tc.seeds) > 0 {
				rs, _, err := store.Open(sp)
				if err != nil {
					t.Fatalf("store.Open: %v", err)
				}
				for _, s := range tc.seeds {
					obj, err := workflow.Marshal(&s.state)
					if err != nil {
						t.Fatalf("marshal: %v", err)
					}
					rec := &workflow.Record{
						WorkflowName: workflowName,
						ForeignID:    "fid",
						RunID:        s.runID,
						RunState:     workflow.RunStateRunning,
						Status:       int(refactorsweep.StatusWorking),
						Object:       obj,
						UpdatedAt:    time.Now(),
					}
					if err := rs.Store(context.Background(), rec); err != nil {
						t.Fatalf("store.Store: %v", err)
					}
				}
			}

			flush := captureStdout(t)
			err := directList(context.Background(), sp)
			out := flush()

			if err != nil {
				t.Fatalf("directList: %v", err)
			}
			if tc.wantNone {
				if !strings.Contains(out, "no runs found") {
					t.Errorf("expected 'no runs found', got:\n%s", out)
				}
				return
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\n\nfull output:\n%s", want, out)
				}
			}
		})
	}
}

func TestFindRunTrackingMR(t *testing.T) {
	trackedMRVal := provider.MR{ProjectID: "grp/proj", IID: 42}

	seedRun := func(rs workflow.RecordStore, runID string, runState workflow.RunState, inFlight map[string]provider.MR) {
		state := refactorsweep.AgentState{Goal: "seed", InFlight: inFlight}
		obj, err := workflow.Marshal(&state)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := &workflow.Record{
			WorkflowName: workflowName,
			ForeignID:    "fid-" + runID,
			RunID:        runID,
			RunState:     runState,
			Status:       int(refactorsweep.StatusWorking),
			Object:       obj,
			UpdatedAt:    time.Now(),
		}
		if err := rs.Store(context.Background(), rec); err != nil {
			t.Fatalf("store.Store: %v", err)
		}
	}

	t.Run("found in an active run", func(t *testing.T) {
		sp := filepath.Join(t.TempDir(), "store.db")
		rs, _, err := store.Open(sp)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		seedRun(rs, "aaaaaaaa-1111-0000-0000-000000000001", workflow.RunStateRunning, map[string]provider.MR{
			"unit-1": trackedMRVal,
		})

		got, found, err := findRunTrackingMR(context.Background(), rs, trackedMRVal.ProjectID, trackedMRVal.IID)
		if err != nil {
			t.Fatalf("findRunTrackingMR: %v", err)
		}
		if !found {
			t.Fatalf("expected found=true")
		}
		if got.RunID != "aaaaaaaa-1111-0000-0000-000000000001" || got.UnitID != "unit-1" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("skips terminal runs", func(t *testing.T) {
		sp := filepath.Join(t.TempDir(), "store.db")
		rs, _, err := store.Open(sp)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		seedRun(rs, "bbbbbbbb-0001-0000-0000-000000000001", workflow.RunStateCompleted, map[string]provider.MR{
			"unit-1": trackedMRVal,
		})

		_, found, err := findRunTrackingMR(context.Background(), rs, trackedMRVal.ProjectID, trackedMRVal.IID)
		if err != nil {
			t.Fatalf("findRunTrackingMR: %v", err)
		}
		if found {
			t.Fatalf("expected found=false for a terminal run's stale InFlight entry")
		}
	})

	t.Run("not tracked anywhere", func(t *testing.T) {
		sp := filepath.Join(t.TempDir(), "store.db")
		rs, _, err := store.Open(sp)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		seedRun(rs, "cccccccc-0002-0000-0000-000000000002", workflow.RunStateRunning, map[string]provider.MR{
			"unit-1": {ProjectID: "grp/other", IID: 7},
		})

		_, found, err := findRunTrackingMR(context.Background(), rs, trackedMRVal.ProjectID, trackedMRVal.IID)
		if err != nil {
			t.Fatalf("findRunTrackingMR: %v", err)
		}
		if found {
			t.Fatalf("expected found=false")
		}
	})

	t.Run("empty store", func(t *testing.T) {
		sp := filepath.Join(t.TempDir(), "store.db")
		rs, _, err := store.Open(sp)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}

		_, found, err := findRunTrackingMR(context.Background(), rs, trackedMRVal.ProjectID, trackedMRVal.IID)
		if err != nil {
			t.Fatalf("findRunTrackingMR: %v", err)
		}
		if found {
			t.Fatalf("expected found=false")
		}
	})
}

func TestDaemonBannerLine(t *testing.T) {
	orig, origCommit := version, gitCommit
	t.Cleanup(func() { version, gitCommit = orig, origCommit })

	version = "2.3.4"
	gitCommit = "deadbeef"

	banner := daemonBannerLine()

	wants := []string{
		"syntropy daemon starting",
		"version=2.3.4",
		"commit=deadbeef",
		fmt.Sprintf("pid=%d", os.Getpid()),
		fmt.Sprintf("go=%s", runtime.Version()),
		fmt.Sprintf("os=%s", runtime.GOOS),
		fmt.Sprintf("arch=%s", runtime.GOARCH),
	}
	for _, w := range wants {
		if !strings.Contains(banner, w) {
			t.Errorf("banner missing %q\n\nfull banner: %s", w, banner)
		}
	}
}

func TestBuildSweeper_WiredToDaemonDeps(t *testing.T) {
	dir := t.TempDir()
	backend, err := store.OpenSqlite(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("OpenSqlite: %v", err)
	}
	recordStore := backend.RecordStore()
	streamer := eventstream.New(backend.DB())
	threshold := 42 * time.Minute
	cooldown := 7 * time.Minute
	logger := discardLogger()

	sweeper := buildSweeper(recordStore, streamer, threshold, cooldown, logger)

	if sweeper.Store != recordStore {
		t.Errorf("Store = %v, want the daemon's recordStore", sweeper.Store)
	}
	if sweeper.Streamer != streamer {
		t.Errorf("Streamer = %v, want the daemon's EventStreamer", sweeper.Streamer)
	}
	if sweeper.WorkflowName != workflowName {
		t.Errorf("WorkflowName = %q, want %q", sweeper.WorkflowName, workflowName)
	}
	if sweeper.Threshold != threshold {
		t.Errorf("Threshold = %v, want %v", sweeper.Threshold, threshold)
	}
	if sweeper.RetriggerCooldown != cooldown {
		t.Errorf("RetriggerCooldown = %v, want %v", sweeper.RetriggerCooldown, cooldown)
	}
	if sweeper.Logger != logger {
		t.Errorf("Logger = %v, want the daemon's logger", sweeper.Logger)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDirectStatus_ListAllRuns(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "store.db")
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	goals := []string{"migrate alpha service", "migrate beta service"}
	runIDs := []string{
		"bbbbbbbb-0001-0000-0000-000000000001",
		"bbbbbbbb-0002-0000-0000-000000000002",
	}
	for i, goal := range goals {
		state := refactorsweep.AgentState{Goal: goal}
		obj, mErr := workflow.Marshal(&state)
		if mErr != nil {
			t.Fatalf("marshal: %v", mErr)
		}
		rec := &workflow.Record{
			WorkflowName: workflowName,
			ForeignID:    "fid",
			RunID:        runIDs[i],
			RunState:     workflow.RunStateRunning,
			Status:       int(refactorsweep.StatusWorking),
			Object:       obj,
			UpdatedAt:    time.Now(),
		}
		if err := rs.Store(context.Background(), rec); err != nil {
			t.Fatalf("store.Store: %v", err)
		}
	}

	flush := captureStdout(t)
	err = directStatus(context.Background(), sp, "")
	out := flush()

	if err != nil {
		t.Fatalf("directStatus: %v", err)
	}
	if !strings.Contains(out, "RUN ID") {
		t.Errorf("missing table header in output:\n%s", out)
	}
	for _, want := range []string{"migrate alpha service", "migrate beta service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing goal %q\n\nfull output:\n%s", want, out)
		}
	}
}

// Full-UUID run IDs sharing a leading substring, for prefix-matching tests.
var prefixRunIDs = []string{
	"00000000-0000-0000-0000-000000000001",
	"00000000-0000-0000-0000-000000000002",
	"00000000-0000-0000-0000-000000000003",
}

// TestPrefixMatching covers the three subcommands (status/abandon/resume) for
// ambiguous, unique-full, and no-match prefixes. abandon/resume mutate state
// on success, so each subtest reseeds a fresh temp store.
func TestPrefixMatching(t *testing.T) {
	const ambiguous = "000000"
	const noMatch = "00000000-0000-0000-0000-000000000fff"
	full := prefixRunIDs[0]

	// resume requires Cancelled/Failed/Paused; the others accept Working.
	subcmds := []struct {
		name   string
		status refactorsweep.AgentStatus
		invoke func(ctx context.Context, storePath, runID string) error
	}{
		{"status", refactorsweep.StatusWorking, directStatus},
		{"abandon", refactorsweep.StatusWorking, func(ctx context.Context, sp, rid string) error {
			return directAbandon(ctx, sp, rid, "", "", "")
		}},
		{"resume", refactorsweep.StatusPaused, directResume},
	}
	for _, sc := range subcmds {
		t.Run(sc.name, func(t *testing.T) {
			seed := func() string { return seedStoreMulti(t, prefixRunIDs, sc.status) }

			err := sc.invoke(context.Background(), seed(), ambiguous)
			if err == nil {
				t.Fatal("ambiguous: expected error")
			}
			for _, id := range prefixRunIDs {
				if !strings.Contains(err.Error(), id) {
					t.Errorf("ambiguous error missing %q; got: %s", id, err)
				}
			}

			flush := captureStdout(t)
			if err := sc.invoke(context.Background(), seed(), full); err != nil {
				_ = flush()
				t.Fatalf("full uuid: %v", err)
			}
			_ = flush()

			err = sc.invoke(context.Background(), seed(), noMatch)
			if err == nil || !strings.Contains(err.Error(), "no run matches prefix") {
				t.Errorf("no match: want 'no run matches prefix' err, got: %v", err)
			}
		})
	}
}

// TestDaemonUnreachableHint asserts that when the daemon is unreachable AND
// no store fallback exists, all three subcommands surface a hint pointing at
// --store. HOME is redirected to an empty temp dir so ~/.syntropy/store.db
// doesn't exist.
func TestDaemonUnreachableHint(t *testing.T) {
	const unreachable = "http://127.0.0.1:9" // reserved "discard" port
	t.Setenv("HOME", t.TempDir())

	subcmds := []struct {
		name string
		run  func(args []string) error
	}{
		{"status", cmdStatus},
		{"abandon", cmdAbandon},
		{"resume", cmdResume},
	}
	for _, sc := range subcmds {
		t.Run(sc.name, func(t *testing.T) {
			err := sc.run([]string{"--daemon", unreachable, prefixRunIDs[0]})
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "is unreachable") || !strings.Contains(msg, "--store") {
				t.Errorf("want 'is unreachable' and '--store' hint in error; got: %s", msg)
			}
		})
	}
}

// TestCmdSetup_NonInteractiveDefaultsToClaudeNoModel asserts that a
// non-interactive `everflow setup` (test binaries don't run with a stdin
// TTY) with no flags auto-selects the sole registered runner and leaves the
// model unset rather than hanging on a prompt.
func TestCmdSetup_NonInteractiveDefaultsToClaudeNoModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Runner != "claude" {
		t.Fatalf("got runner %q, want %q", cfg.Runner, "claude")
	}
	if cfg.Model != "" {
		t.Fatalf("got model %q, want empty (no TTY, no --model)", cfg.Model)
	}
	if cfg.SpecTool != "" {
		t.Fatalf("got spec tool %q, want empty (no TTY, no --spec-tool)", cfg.SpecTool)
	}
}

// TestCmdSetup_ModelFlagPersists asserts --model is persisted verbatim.
func TestCmdSetup_ModelFlagPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--model", "claude-haiku-4-5"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Model != "claude-haiku-4-5" {
		t.Fatalf("got model %q, want %q", cfg.Model, "claude-haiku-4-5")
	}
}

// TestCmdSetup_RerunWithoutModelFlagKeepsExisting asserts that re-running
// setup non-interactively without --model doesn't clobber a previously
// persisted model back to empty.
func TestCmdSetup_RerunWithoutModelFlagKeepsExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--model", "claude-sonnet-5"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup (rerun): %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Model != "claude-sonnet-5" {
		t.Fatalf("got model %q, want previously persisted value kept", cfg.Model)
	}
}

// TestCmdSetup_SpecToolFlagPersists asserts --spec-tool is persisted
// verbatim.
func TestCmdSetup_SpecToolFlagPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--spec-tool", "spec-kit"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.SpecTool != "spec-kit" {
		t.Fatalf("got spec tool %q, want %q", cfg.SpecTool, "spec-kit")
	}
}

// TestCmdSetup_RerunWithoutSpecToolFlagKeepsExisting asserts that
// re-running setup non-interactively without --spec-tool doesn't clobber a
// previously persisted spec tool back to empty.
func TestCmdSetup_RerunWithoutSpecToolFlagKeepsExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--spec-tool", "spec-kit"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup (rerun): %v", err)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.SpecTool != "spec-kit" {
		t.Fatalf("got spec tool %q, want previously persisted value kept", cfg.SpecTool)
	}
}

// TestCmdSetup_UnknownRunnerFlagErrors asserts an unrecognised --runner
// fails loudly instead of silently persisting a bogus default.
func TestCmdSetup_UnknownRunnerFlagErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdSetup([]string{"--runner", "not-a-real-runner"}); err == nil {
		t.Fatal("expected an error for an unknown runner")
	}
}

// TestCmdSetup_NoTitleConventionFlagWritesNothing asserts a non-interactive
// setup with no --title-convention leaves .syntropy.yml absent rather than
// writing an empty convention.
func TestCmdSetup_NoTitleConventionFlagWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := cmdSetup(nil); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	if _, err := os.Stat(".syntropy.yml"); !os.IsNotExist(err) {
		t.Fatalf("expected no .syntropy.yml, stat err = %v", err)
	}
}

// TestCmdSetup_TitleConventionFlagPersists asserts --title-convention is
// written verbatim to .syntropy.yml in the current directory.
func TestCmdSetup_TitleConventionFlagPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := cmdSetup([]string{"--title-convention", "Conventional Commits"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	data, err := os.ReadFile(".syntropy.yml")
	if err != nil {
		t.Fatalf("read .syntropy.yml: %v", err)
	}
	if !strings.Contains(string(data), "title_convention: Conventional Commits") {
		t.Fatalf("got %q, want it to contain the given title convention", string(data))
	}
}

// TestCmdSetup_TitleConventionDoesNotClobberExistingWithoutForce asserts a
// rerun without --force leaves a pre-existing .syntropy.yml untouched, even
// when a new --title-convention is passed.
func TestCmdSetup_TitleConventionDoesNotClobberExistingWithoutForce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := os.WriteFile(".syntropy.yml", []byte("title_convention: existing\n"), 0o644); err != nil {
		t.Fatalf("seed existing .syntropy.yml: %v", err)
	}

	if err := cmdSetup([]string{"--title-convention", "new convention"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	data, err := os.ReadFile(".syntropy.yml")
	if err != nil {
		t.Fatalf("read .syntropy.yml: %v", err)
	}
	if string(data) != "title_convention: existing\n" {
		t.Fatalf("existing .syntropy.yml was clobbered: %q", string(data))
	}
}

// TestCmdSetup_RepoSpecToolFlagPersists asserts --repo-spec-tool is written
// verbatim to .syntropy.yml in the current directory, and never counted as
// a missing field.
func TestCmdSetup_RepoSpecToolFlagPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if err := cmdSetup([]string{"--repo-spec-tool", "spec-kit"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}

	data, err := os.ReadFile(".syntropy.yml")
	if err != nil {
		t.Fatalf("read .syntropy.yml: %v", err)
	}
	if !strings.Contains(string(data), "spec_tool: spec-kit") {
		t.Fatalf("got %q, want it to contain the given repo spec tool", string(data))
	}

	var buf bytes.Buffer
	missing, err := checkRepoConfig(".", &buf)
	if err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if len(missing) != 1 || missing[0] != "title_convention" {
		t.Fatalf("got missing %v, want only [title_convention] — spec_tool must never be reported missing", missing)
	}
}

// --- config check (ADR-0083) ---

func TestCheckRepoConfig_AbsentFile_ReportsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	var buf bytes.Buffer
	missing, err := checkRepoConfig(dir, &buf)
	if err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if len(missing) != 1 || missing[0] != "title_convention" {
		t.Fatalf("got %v, want [title_convention]", missing)
	}
	if !strings.Contains(buf.String(), "MISSING") || !strings.Contains(buf.String(), "title_convention") {
		t.Errorf("output should report the missing field; got %q", buf.String())
	}
}

func TestCheckRepoConfig_RealValue_ReportsOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".syntropy.yml"), []byte("title_convention: Conventional Commits\n"), 0o644); err != nil {
		t.Fatalf("seed .syntropy.yml: %v", err)
	}
	var buf bytes.Buffer
	missing, err := checkRepoConfig(dir, &buf)
	if err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("got %v, want no missing fields", missing)
	}
	if !strings.Contains(buf.String(), "OK") {
		t.Errorf("output should report OK; got %q", buf.String())
	}
}

func TestCheckRepoConfig_BlankSentinel_ReportsOK(t *testing.T) {
	// A field explicitly recorded as setup.BlankSentinel ("blank") must
	// count as configured — the user already declined it, don't re-flag
	// it as missing.
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".syntropy.yml"), []byte("title_convention: blank\n"), 0o644); err != nil {
		t.Fatalf("seed .syntropy.yml: %v", err)
	}
	var buf bytes.Buffer
	missing, err := checkRepoConfig(dir, &buf)
	if err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("got %v, want no missing fields (blank counts as decided)", missing)
	}
	if !strings.Contains(buf.String(), "OK") {
		t.Errorf("output should report OK; got %q", buf.String())
	}
}

// --- config check: effective spec tool (ADR-0099's deferred consumption) ---

func TestCheckRepoConfig_SpecTool_NeitherSet_ReportsSyntropyDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	var buf bytes.Buffer
	if _, err := checkRepoConfig(dir, &buf); err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if !strings.Contains(buf.String(), "Spec tool: (none set — syntropy's own default spec flow)") {
		t.Errorf("got %q, want it to report syntropy's own default spec flow", buf.String())
	}
}

func TestCheckRepoConfig_SpecTool_GlobalOnly_ReportsGlobalDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{SpecTool: "spec-kit"}); err != nil {
		t.Fatalf("save global config: %v", err)
	}
	dir := t.TempDir()
	var buf bytes.Buffer
	if _, err := checkRepoConfig(dir, &buf); err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if !strings.Contains(buf.String(), "Spec tool: spec-kit (global default)") {
		t.Errorf("got %q, want it to report the global default", buf.String())
	}
}

func TestCheckRepoConfig_SpecTool_RepoOverride_TakesPrecedenceOverGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{SpecTool: "spec-kit"}); err != nil {
		t.Fatalf("save global config: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".syntropy.yml"), []byte("title_convention: blank\nspec_tool: other-tool\n"), 0o644); err != nil {
		t.Fatalf("seed .syntropy.yml: %v", err)
	}
	var buf bytes.Buffer
	if _, err := checkRepoConfig(dir, &buf); err != nil {
		t.Fatalf("checkRepoConfig: %v", err)
	}
	if !strings.Contains(buf.String(), "Spec tool: other-tool (repo override)") {
		t.Errorf("got %q, want the repo override to take precedence over the global default", buf.String())
	}
}

// startTriggerCapture spins up a fake daemon that records the decoded
// triggerRequest of the last /trigger POST it receives.
func startTriggerCapture(t *testing.T) (url string, got *triggerRequest) {
	t.Helper()
	got = &triggerRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decode trigger request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(triggerResponse{RunID: "run-1", ForeignID: "foreign-1"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, got
}

// TestCmdStart_FallsBackToPersistedDefaultModel asserts that when neither
// --model nor the spec's `model:` set a runner model, cmdStart falls back
// to the default persisted by `syntropy setup` (ADR-0051).
func TestCmdStart_FallsBackToPersistedDefaultModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{Runner: "claude", Model: "claude-haiku-4-5"}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	daemonURL, got := startTriggerCapture(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
	})
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got.RunnerModel != "claude-haiku-4-5" {
		t.Fatalf("got runner model %q, want persisted default %q", got.RunnerModel, "claude-haiku-4-5")
	}
}

// TestCmdStart_ModelFlagOverridesPersistedDefault asserts --model still
// wins over a persisted default.
func TestCmdStart_ModelFlagOverridesPersistedDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{Runner: "claude", Model: "claude-haiku-4-5"}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	daemonURL, got := startTriggerCapture(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
		"--model", "claude-sonnet-5",
	})
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got.RunnerModel != "claude-sonnet-5" {
		t.Fatalf("got runner model %q, want flag override %q", got.RunnerModel, "claude-sonnet-5")
	}
}

// TestCmdStart_NoConfigLeavesModelEmpty asserts that with no persisted
// config and no --model, the runner model stays empty (runner's own
// default applies).
func TestCmdStart_NoConfigLeavesModelEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	daemonURL, got := startTriggerCapture(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
	})
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if got.RunnerModel != "" {
		t.Fatalf("got runner model %q, want empty", got.RunnerModel)
	}
}

// TestCmdStart_PrintsUpdateNotice asserts that once a run is successfully
// triggered, cmdStart prints a notice if the cached update-check result
// (ADR-0102) says a newer release is available. Pre-seeding a fresh cache
// entry means this exercises the wiring without an outbound GitHub call.
func TestCmdStart_PrintsUpdateNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{
		UpdateCheckedAt:     time.Now(),
		UpdateLatestVersion: "v99.0.0",
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	daemonURL, _ := startTriggerCapture(t)
	restore := captureStdout(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
	})
	out := restore()
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if !strings.Contains(out, "newer syntropy release is available: v99.0.0") {
		t.Fatalf("got output %q, want an update notice for v99.0.0", out)
	}
}

// TestCmdStart_NoUpdateNoticeWhenUpToDate asserts that with a cached
// check reporting no newer release, cmdStart's output is unaffected.
func TestCmdStart_NoUpdateNoticeWhenUpToDate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(home, config.Config{
		UpdateCheckedAt:     time.Now(),
		UpdateLatestVersion: "v0.0.1",
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	daemonURL, _ := startTriggerCapture(t)
	restore := captureStdout(t)

	err := cmdStart([]string{
		"--units", "u1",
		"--provider", "gitlab",
		"--project", "acme/example",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
	})
	out := restore()
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if strings.Contains(out, "newer syntropy release") {
		t.Fatalf("got output %q, want no update notice", out)
	}
}

// --- isAutoPaused / directResume RunStatePaused tests (ADR-0062) ---

func TestIsAutoPaused(t *testing.T) {
	tests := []struct {
		name string
		rec  workflow.Record
		want bool
	}{
		{
			name: "RunStatePaused while Working — the circuit breaker case",
			rec:  workflow.Record{RunState: workflow.RunStatePaused, Status: int(refactorsweep.StatusWorking)},
			want: true,
		},
		{
			name: "RunStatePaused while our own business StatusPaused — human pause, not auto",
			rec:  workflow.Record{RunState: workflow.RunStatePaused, Status: int(refactorsweep.StatusPaused)},
			want: false,
		},
		{
			name: "RunStateRunning — not paused at all",
			rec:  workflow.Record{RunState: workflow.RunStateRunning, Status: int(refactorsweep.StatusWorking)},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAutoPaused(&tc.rec); got != tc.want {
				t.Errorf("isAutoPaused: want %v, got %v", tc.want, got)
			}
		})
	}
}

// seedAutoPausedStore mirrors seedStore but sets RunState to RunStatePaused
// with an arbitrary business Status, simulating a Run parked by the
// PauseAfterErrCount circuit breaker (ADR-0062) mid-step rather than at a
// callback-registered status.
func seedAutoPausedStore(t *testing.T, runID string, status refactorsweep.AgentStatus, state refactorsweep.AgentState) string {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "store.db")

	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	obj, err := workflow.Marshal(&state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := &workflow.Record{
		WorkflowName: workflowName,
		ForeignID:    "test-foreign-id",
		RunID:        runID,
		RunState:     workflow.RunStatePaused,
		Status:       int(status),
		Object:       obj,
		UpdatedAt:    time.Now(),
		Meta:         workflow.Meta{RunStateReason: "max error retry threshold hit - automatically paused"},
	}
	if err := rs.Store(context.Background(), rec); err != nil {
		t.Fatalf("store.Store: %v", err)
	}
	return sp
}

func TestDirectResume_AutoPaused_RestoresOriginalStatus(t *testing.T) {
	runID := "11111111-1111-1111-1111-111111111111"
	sp := seedAutoPausedStore(t, runID, refactorsweep.StatusWorking, refactorsweep.AgentState{
		Goal: "test", LastError: "stale error from a previous failed attempt",
	})

	if err := directResume(context.Background(), sp, runID); err != nil {
		t.Fatalf("directResume: %v", err)
	}

	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rec, err := rs.Lookup(context.Background(), runID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if rec.RunState != workflow.RunStateRunning {
		t.Errorf("RunState: want RunStateRunning, got %s", rec.RunState)
	}
	// The key behavior this test guards: reviving an auto-paused Run must
	// restore the Status it was actually stuck in (Working), NOT force it
	// back to StatusDiscovering the way the pre-existing Cancelled/Failed
	// revival path does.
	if got := refactorsweep.AgentStatus(rec.Status); got != refactorsweep.StatusWorking {
		t.Errorf("Status: want StatusWorking (restored, not forced to Discovering), got %s", got)
	}
	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if state.LastError != "" {
		t.Errorf("LastError: want cleared, got %q", state.LastError)
	}
}

// Regression: cmdAbandon had the same gap the resume fix closed — a Run
// auto-paused by PauseAfterErrCount (RunState=Paused, arbitrary
// AgentStatus) can't be abandoned via /control's wf.Callback dispatch
// either (no callback registered for e.g. StatusDiscovering), so
// sendControl silently no-ops. Unlike directResume, directAbandon needed
// no special-casing — it only ever gated on RunState.Finished(), which
// Paused doesn't satisfy — but it had zero test coverage before this fix
// started routing new traffic through it. This proves it actually works
// for the auto-paused shape.
func TestDirectAbandon_AutoPausedRun_Cancels(t *testing.T) {
	runID := "33333333-3333-3333-3333-333333333333"
	sp := seedAutoPausedStore(t, runID, refactorsweep.StatusDiscovering, refactorsweep.AgentState{
		Goal: "test",
	})

	if err := directAbandon(context.Background(), sp, runID, "test abandon", "", ""); err != nil {
		t.Fatalf("directAbandon: %v", err)
	}

	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rec, err := rs.Lookup(context.Background(), runID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if rec.RunState != workflow.RunStateCancelled {
		t.Errorf("RunState: want RunStateCancelled, got %s", rec.RunState)
	}
	if got := refactorsweep.AgentStatus(rec.Status); got != refactorsweep.StatusCancelled {
		t.Errorf("Status: want StatusCancelled, got %s", got)
	}
}

// Regression: found live — `syntropy resume` on a genuinely Failed run
// (not the AutoPaused circuit-breaker case) reported "resume sent" via
// sendControl's /control POST, which returned HTTP 200 (no error) but
// had no actual effect, since /control's wf.Callback dispatch has no
// registered consumer for a terminal Failed status. cmdResume must
// detect Failed/Cancelled via the daemon's own /status and route
// straight to directResume — the same path used for AutoPaused/daemon-
// unreachable — instead of trusting sendControl's lack of an HTTP error
// as proof anything happened.
func TestCmdResume_ReachableDaemon_FailedRun_UsesDirectResumeNotControl(t *testing.T) {
	runID := "33333333-3333-3333-3333-333333333333"
	sp := seedStoreMulti(t, []string{runID}, refactorsweep.StatusFailed)

	controlCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]runStatusResponse{
				{RunID: runID, Status: refactorsweep.StatusFailed.String(), AutoPaused: false},
			})
		case "/control":
			// If cmdResume mistakenly takes the sendControl path for a
			// Failed run, this would fire and (per the pre-fix bug)
			// silently succeed with no real effect.
			controlCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := cmdResume([]string{"--daemon", srv.URL, "--store", sp, runID}); err != nil {
		t.Fatalf("cmdResume: %v", err)
	}
	if controlCalled {
		t.Error("cmdResume must not call /control for a genuinely Failed run — it has no registered callback and silently no-ops")
	}

	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rec, err := rs.Lookup(context.Background(), runID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got := refactorsweep.AgentStatus(rec.Status); got != refactorsweep.StatusDiscovering {
		t.Errorf("Status: want StatusDiscovering (revived via directResume), got %s", got)
	}
	if rec.RunState != workflow.RunStateRunning {
		t.Errorf("RunState: want RunStateRunning, got %v", rec.RunState)
	}
}

func TestDirectResume_RegularCancelledStillForcesDiscovering(t *testing.T) {
	// Regression: the pre-existing Cancelled/Failed/StatusPaused revival
	// path must keep its original "always restart planning" behavior —
	// only the new RunStatePaused-while-mid-step case (tested above) skips
	// that and restores the original Status instead.
	runID := "22222222-2222-2222-2222-222222222222"
	dir := t.TempDir()
	sp := filepath.Join(dir, "store.db")
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	obj, err := workflow.Marshal(&refactorsweep.AgentState{Goal: "test"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rs.Store(context.Background(), &workflow.Record{
		WorkflowName: workflowName,
		ForeignID:    "test-foreign-id",
		RunID:        runID,
		RunState:     workflow.RunStateCancelled,
		Status:       int(refactorsweep.StatusCancelled),
		Object:       obj,
		UpdatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("store.Store: %v", err)
	}

	if err := directResume(context.Background(), sp, runID); err != nil {
		t.Fatalf("directResume: %v", err)
	}

	rec, err := rs.Lookup(context.Background(), runID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got := refactorsweep.AgentStatus(rec.Status); got != refactorsweep.StatusDiscovering {
		t.Errorf("Status: want StatusDiscovering (forced, as before), got %s", got)
	}
}

// --- unsetNestedClaudeCodeEnv tests (ADR-0064) ---

func TestUnsetNestedClaudeCodeEnv(t *testing.T) {
	// Simulate the daemon having been launched from inside an active
	// Claude Code session (e.g. via its Bash tool), which sets these on
	// every process it spawns.
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "some-other-session")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	// Not a nesting signal — must survive.
	t.Setenv("CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING", "1")

	unsetNestedClaudeCodeEnv()

	for _, v := range nestedClaudeCodeEnvVars {
		if val, ok := os.LookupEnv(v); ok {
			t.Errorf("%s: want unset, got %q", v, val)
		}
	}
	if val := os.Getenv("CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"); val != "1" {
		t.Errorf("CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING: want to survive unrelated, got %q", val)
	}
}

func TestReachedFirstCheckpoint(t *testing.T) {
	tests := []struct {
		name string
		s    runStatusResponse
		want bool
	}{
		{"fresh discovering, nothing yet", runStatusResponse{Status: refactorsweep.StatusDiscovering.String()}, false},
		{"initiated, nothing yet", runStatusResponse{Status: refactorsweep.StatusInitiated.String()}, false},
		{"working but no unit landed yet", runStatusResponse{Status: refactorsweep.StatusWorking.String()}, false},
		{"first MR in flight", runStatusResponse{Status: refactorsweep.StatusWorking.String(), InFlight: 1}, true},
		{"first unit completed", runStatusResponse{Status: refactorsweep.StatusDiscovering.String(), Completed: 1}, true},
		{"first unit blacklisted", runStatusResponse{Status: refactorsweep.StatusDiscovering.String(), Blacklisted: 1}, true},
		{"awaiting merge", runStatusResponse{Status: refactorsweep.StatusAwaitingMerge.String()}, true},
		{"paused for author", runStatusResponse{Status: refactorsweep.StatusPaused.String()}, true},
		{"auto-paused suffix still matches", runStatusResponse{Status: refactorsweep.StatusPaused.String() + " (auto-paused: circuit breaker)"}, true},
		{"awaiting abandon confirm", runStatusResponse{Status: refactorsweep.StatusAwaitingAbandonConfirm.String()}, true},
		{"completed", runStatusResponse{Status: refactorsweep.StatusCompleted.String()}, true},
		{"failed", runStatusResponse{Status: refactorsweep.StatusFailed.String()}, true},
		{"cancelled", runStatusResponse{Status: refactorsweep.StatusCancelled.String()}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reachedFirstCheckpoint(tt.s); got != tt.want {
				t.Errorf("reachedFirstCheckpoint(%+v) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

// TestCmdWait_StoreFallback_ReturnsImmediatelyWhenCheckpointReached asserts
// that `wait` returns as soon as a poll observes a checkpoint, without
// waiting out the timeout, and that it falls back to a direct store read
// when the daemon is unreachable (same fallback as `status`/`abandon`/
// `resume`).
func TestCmdWait_StoreFallback_ReturnsImmediatelyWhenCheckpointReached(t *testing.T) {
	const unreachable = "http://127.0.0.1:9" // reserved "discard" port
	runID := "bbbbbbbb-0000-0000-0000-000000000001"
	state := refactorsweep.AgentState{
		Goal: "migrate the acme/example service",
		Completed: []refactorsweep.CompletedUnit{
			{UnitID: "unit-1"},
		},
	}
	sp := seedStore(t, runID, state)

	flush := captureStdout(t)
	start := time.Now()
	err := cmdWait([]string{"--daemon", unreachable, "--store", sp, "--timeout", "5s", "--interval", "10ms", runID})
	elapsed := time.Since(start)
	out := flush()

	if err != nil {
		t.Fatalf("cmdWait: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("cmdWait took %s, want it to return promptly once the checkpoint is already present", elapsed)
	}
	if !strings.Contains(out, runID) {
		t.Errorf("output missing run id %q\n\nfull output:\n%s", runID, out)
	}
}

// TestCmdWait_TimesOutWhenNoCheckpointReached asserts that `wait` gives up
// and returns an error once --timeout elapses for a Run stuck before any
// checkpoint (still Discovering, nothing in flight/completed/blacklisted).
func TestCmdWait_TimesOutWhenNoCheckpointReached(t *testing.T) {
	const unreachable = "http://127.0.0.1:9"
	runID := "cccccccc-0000-0000-0000-000000000001"
	sp := seedStore(t, runID, refactorsweep.AgentState{Goal: "migrate the acme/example service"})

	// seedStore always writes StatusWorking; overwrite with Discovering and
	// no in-flight/completed/blacklisted units so no checkpoint is reached.
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rec, err := rs.Lookup(context.Background(), runID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	rec.Status = int(refactorsweep.StatusDiscovering)
	if err := rs.Store(context.Background(), rec); err != nil {
		t.Fatalf("Store: %v", err)
	}

	flush := captureStdout(t)
	err = cmdWait([]string{"--daemon", unreachable, "--store", sp, "--timeout", "50ms", "--interval", "10ms", runID})
	_ = flush()

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("want 'timed out' in error; got: %v", err)
	}
}

func TestCmdWait_MissingRunIDErrors(t *testing.T) {
	if err := cmdWait(nil); err == nil {
		t.Fatal("expected an error for a missing run-id argument")
	}
}

func TestCmdWait_DaemonUnreachableNoStoreFallback_ReturnsHint(t *testing.T) {
	const unreachable = "http://127.0.0.1:9"
	t.Setenv("HOME", t.TempDir())

	err := cmdWait([]string{"--daemon", unreachable, "--timeout", "50ms", prefixRunIDs[0]})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "is unreachable") || !strings.Contains(msg, "--store") {
		t.Errorf("want 'is unreachable' and '--store' hint in error; got: %s", msg)
	}
}

// startFakeGitLabMR starts an httptest server that answers GetMR for a
// single project/IID with the given branch/state, mimicking gitlab_test.go's
// TestGetMR fixture.
func startFakeGitLabMR(t *testing.T, projectID string, iid int, branch, state string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d", projectID, iid)
		if r.URL.Path != wantPath {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"source_branch":%q,"web_url":"https://gitlab.example/%s/-/merge_requests/%d","state":%q}`,
			branch, projectID, iid, state)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestCmdAdopt_TriggersRunWithAdoptFields asserts that `syntropy adopt`
// fetches the MR's current branch/URL from the provider and forwards them
// (plus the unit ID) as AdoptMR* fields on the trigger request, so the
// daemon can pre-populate CurrentUnit/InFlight (see triggerHandler) instead
// of discovering/planning from scratch.
func TestCmdAdopt_TriggersRunWithAdoptFields(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "static-token")
	gitlabURL := startFakeGitLabMR(t, "acme/example", 42, "feature-x", "opened")
	daemonURL, got := startTriggerCapture(t)

	err := cmdAdopt([]string{
		"--provider", "gitlab",
		"--project", "acme/example",
		"--mr", "42",
		"--unit", "svc-x",
		"--base-repo", "/tmp/repo",
		"--daemon", daemonURL,
		"--gitlab-base-url", gitlabURL,
		"--store", filepath.Join(t.TempDir(), "store.db"),
	})
	if err != nil {
		t.Fatalf("cmdAdopt: %v", err)
	}
	if got.AdoptUnitID != "svc-x" {
		t.Errorf("AdoptUnitID: got %q, want %q", got.AdoptUnitID, "svc-x")
	}
	if got.AdoptMRIID != 42 {
		t.Errorf("AdoptMRIID: got %d, want 42", got.AdoptMRIID)
	}
	if got.AdoptMRBranch != "feature-x" {
		t.Errorf("AdoptMRBranch: got %q, want %q", got.AdoptMRBranch, "feature-x")
	}
	if got.ProviderName != "gitlab" || got.ProjectID != "acme/example" {
		t.Errorf("got provider=%q project=%q", got.ProviderName, got.ProjectID)
	}
}

// TestCmdAdopt_AlreadyTrackedErrors asserts adopt refuses to double-track an
// MR that's already InFlight under a live Run — otherwise two Runs would
// race to react to the same webhook/poll events for it.
func TestCmdAdopt_AlreadyTrackedErrors(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "static-token")
	gitlabURL := startFakeGitLabMR(t, "acme/example", 42, "feature-x", "opened")
	_, _ = startTriggerCapture(t)

	sp := filepath.Join(t.TempDir(), "store.db")
	rs, _, err := store.Open(sp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	state := refactorsweep.AgentState{
		Goal:     "seed",
		InFlight: map[string]provider.MR{"svc-x": {ProjectID: "acme/example", IID: 42}},
	}
	obj, err := workflow.Marshal(&state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rs.Store(context.Background(), &workflow.Record{
		WorkflowName: workflowName,
		ForeignID:    "fid-existing",
		RunID:        "aaaaaaaa-1111-0000-0000-000000000001",
		RunState:     workflow.RunStateRunning,
		Status:       int(refactorsweep.StatusAwaitingMerge),
		Object:       obj,
		UpdatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("store.Store: %v", err)
	}

	err = cmdAdopt([]string{
		"--provider", "gitlab",
		"--project", "acme/example",
		"--mr", "42",
		"--unit", "svc-x",
		"--base-repo", "/tmp/repo",
		"--gitlab-base-url", gitlabURL,
		"--store", sp,
	})
	if err == nil {
		t.Fatal("want error for an already-tracked MR, got nil")
	}
	if !strings.Contains(err.Error(), "already tracked by run") {
		t.Errorf("want 'already tracked by run' in error; got: %v", err)
	}
}

// TestCmdAdopt_NotOpenErrors asserts adopt refuses to adopt an MR that
// isn't currently open — there'd be nothing left to track.
func TestCmdAdopt_NotOpenErrors(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "static-token")
	gitlabURL := startFakeGitLabMR(t, "acme/example", 42, "feature-x", "merged")

	err := cmdAdopt([]string{
		"--provider", "gitlab",
		"--project", "acme/example",
		"--mr", "42",
		"--unit", "svc-x",
		"--base-repo", "/tmp/repo",
		"--gitlab-base-url", gitlabURL,
		"--store", filepath.Join(t.TempDir(), "store.db"),
	})
	if err == nil {
		t.Fatal("want error for a non-open MR, got nil")
	}
	if !strings.Contains(err.Error(), "not opened") {
		t.Errorf("want 'not opened' in error; got: %v", err)
	}
}

// TestCmdAdopt_MissingFlagsError asserts the required-flag validation.
func TestCmdAdopt_MissingFlagsError(t *testing.T) {
	cases := [][]string{
		{"--mr", "42", "--unit", "svc-x", "--base-repo", "/tmp/repo"},                                        // missing provider/project
		{"--provider", "gitlab", "--project", "acme/example", "--unit", "svc-x", "--base-repo", "/tmp/repo"}, // missing --mr
		{"--provider", "gitlab", "--project", "acme/example", "--mr", "42", "--base-repo", "/tmp/repo"},      // missing --unit
		{"--provider", "gitlab", "--project", "acme/example", "--mr", "42", "--unit", "svc-x"},               // missing --base-repo
	}
	for _, args := range cases {
		if err := cmdAdopt(args); err == nil {
			t.Errorf("cmdAdopt(%v): want error, got nil", args)
		}
	}
}
