// Syntropy — bulk-refactor sweep CLI. See README.md, DESIGN.md, and the
// decisions/ log for the project's purpose and design.
//
// This file is the CLI surface; business logic lives under internal/.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/luno/workflow"
	"github.com/mattn/go-isatty"

	"github.com/luno/workflow/adapters/memrolescheduler"

	"github.com/andrewwormald/syntropy/internal/config"
	"github.com/andrewwormald/syntropy/internal/eventstream"
	"github.com/andrewwormald/syntropy/internal/git"
	"github.com/andrewwormald/syntropy/internal/poller"
	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/provider/github"
	"github.com/andrewwormald/syntropy/internal/provider/gitlab"
	"github.com/andrewwormald/syntropy/internal/reconciler"
	"github.com/andrewwormald/syntropy/internal/refactorsweep"
	"github.com/andrewwormald/syntropy/internal/retention"
	"github.com/andrewwormald/syntropy/internal/runner"
	"github.com/andrewwormald/syntropy/internal/runner/claude"
	"github.com/andrewwormald/syntropy/internal/setup"
	"github.com/andrewwormald/syntropy/internal/spec"
	"github.com/andrewwormald/syntropy/internal/store"
	"github.com/andrewwormald/syntropy/internal/update"
	"github.com/andrewwormald/syntropy/internal/webhook"
)

const (
	workflowName = "refactor-sweep"
)

var (
	version   = "0.0.1-scaffold"
	gitCommit = "none"
	buildTime = "unknown"
)

var commands = map[string]command{
	"daemon":  {usage: "run the long-lived daemon", run: cmdDaemon},
	"start":   {usage: "trigger a new refactor sweep Run", run: cmdStart},
	"status":  {usage: "show progress for a Run (or list all Runs)", run: cmdStatus},
	"wait":    {usage: "block (token-free, local polling) until a Run reaches its first real checkpoint", run: cmdWait},
	"list":    {usage: "list active and completed Runs", run: cmdList},
	"abandon": {usage: "request abandonment of a Run (two-tap confirmation)", run: cmdAbandon},
	"resume":  {usage: "resume a paused Run", run: cmdResume},
	"phrases": {usage: "manage the per-Run + global skip-phrase files", run: cmdPhrases},
	"setup":   {usage: "install the Claude Code Skill integration and set a default runner/model/spec-tool (see ADR-0002, ADR-0051)", run: cmdSetup},
	"config":  {usage: "check a repo's .syntropy.yml for missing fields (see ADR-0083)", run: cmdConfig},
	"version": {usage: "print the build version", run: cmdVersion},
}

type command struct {
	usage string
	run   func(args []string) error
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	verb := os.Args[1]
	if verb == "-h" || verb == "--help" || verb == "help" {
		printUsage(os.Stdout)
		return
	}
	cmd, ok := commands[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", verb)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	// Best-effort, non-interactive first-run hook (ADR-0002): install the
	// Claude Code Skill bundle if it isn't there yet. Never blocks the
	// actual command on failure. Skipped for `syntropy setup`, which is the
	// explicit, authoritative way to (re)install it.
	if verb != "setup" {
		if home, err := os.UserHomeDir(); err == nil {
			installed, err := setup.EnsureClaudeSkill(home)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: skill setup: %v\n", err)
			} else if installed {
				fmt.Fprintf(os.Stderr, "syntropy: installed the Claude Code Skill at %s (run `syntropy setup` to reinstall or customize)\n", setup.SkillPath(home))
			}
		}
	}
	if err := cmd.run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "syntropy — bulk-refactor sweep daemon\n\nusage: syntropy <command> [flags]\n\ncommands:\n")
	for _, name := range []string{"daemon", "start", "status", "wait", "list", "abandon", "resume", "phrases", "setup", "config", "version"} {
		fmt.Fprintf(w, "  %-9s %s\n", name, commands[name].usage)
	}
	fmt.Fprintf(w, "\nrun `syntropy <command> -h` for command-specific flags.\n")
}

// reconcilerStuckThresholdDefault is the --reconciler-stuck-threshold flag's
// default: how long a Run may sit in Working/Discovering with no progress
// before the reconciler re-triggers it.
const reconcilerStuckThresholdDefault = 30 * time.Minute

// reconcilerRetriggerCooldownDefault is the --reconciler-retrigger-cooldown
// flag's default: how long a re-triggered RunID is left alone before the
// sweeper will consider it stuck again. Deliberately short relative to
// reconcilerStuckThresholdDefault — long enough to kill the 30s-tick
// re-trigger spam observed in production, but short enough that a run which
// is genuinely stuck (not just slow to react to the re-trigger) gets another
// attempt quickly rather than waiting out a near-stuck-threshold-length gap.
const reconcilerRetriggerCooldownDefault = 3 * time.Minute

// buildSweeper constructs the reconciler.Sweeper the daemon runs alongside
// pollerLoop. Split out from cmdDaemon so tests can assert it's wired to the
// same store/streamer/workflow name as the rest of the daemon.
func buildSweeper(rs workflow.RecordStore, streamer workflow.EventStreamer, threshold, cooldown time.Duration, logger *slog.Logger) *reconciler.Sweeper {
	return &reconciler.Sweeper{
		Store:             rs,
		Streamer:          streamer,
		WorkflowName:      workflowName,
		Threshold:         threshold,
		RetriggerCooldown: cooldown,
		Logger:            logger,
	}
}

// retentionPeriodDefault is the --retention-period flag's default: how long
// a terminal Run's records and on-disk run directory are kept before the
// retention sweep deletes them. See ADR-0071.
const retentionPeriodDefault = 31 * 24 * time.Hour

// buildRetentionSweeper constructs the retention.Sweeper the daemon runs
// alongside pollerLoop and the reconciler sweeper. Split out from cmdDaemon
// so tests can assert it's wired to the same store/git/runsRoot as the rest
// of the daemon.
func buildRetentionSweeper(rs *store.RecordStore, g git.Git, runsRoot string, period time.Duration, logger *slog.Logger) *retention.Sweeper {
	return &retention.Sweeper{
		Store:           rs,
		Git:             g,
		RunsRoot:        runsRoot,
		RetentionPeriod: period,
		Logger:          logger,
	}
}

// nestedClaudeCodeEnvVars are set by Claude Code on every process it spawns
// (e.g. the `syntropy daemon &` invocation, if launched via its Bash tool)
// to identify that process as a nested child of the invoking interactive
// session. If left in the daemon's own environment, every `claude -p`
// subprocess the daemon later spawns (see internal/runner/claude) inherits
// them via os.Environ() and is itself mistaken for a nested child of
// whatever session originally started the daemon — a session that may
// still be actively running, unrelated to and completely unaware of this
// long-lived background process. Found live: a Run's planner step failed
// `claude exec: exit status 1` consistently (100% of attempts) while the
// launching session was active, and succeeded every time on manual,
// isolated reproduction — a session-identity conflict, not a real spec or
// environment problem. See ADR-0064.
var nestedClaudeCodeEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_CHILD_SESSION",
}

// unsetNestedClaudeCodeEnv clears nestedClaudeCodeEnvVars from the daemon's
// own environment so every subprocess it spawns for the rest of its
// lifetime — including every claude -p invocation — starts clean,
// regardless of whether `syntropy daemon` itself was launched from inside
// an active Claude Code session.
func unsetNestedClaudeCodeEnv() {
	for _, v := range nestedClaudeCodeEnvVars {
		_ = os.Unsetenv(v)
	}
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	var (
		storePath         = fs.String("store", "", "path to sqlite store (default ~/.syntropy/store.db; pass ':memory:' for volatile)")
		listenAddr        = fs.String("listen", ":8080", "address for the webhook HTTP server")
		publicBaseURL     = fs.String("public-base-url", "", "publicly reachable URL where webhooks land (e.g. https://everflow.example.com)")
		gitlabBaseURL     = fs.String("gitlab-base-url", "", "GitLab base URL (defaults to https://gitlab.com)")
		githubBaseURL     = fs.String("github-base-url", "", "GitHub API base URL (defaults to https://api.github.com; GHE users set this to https://<your-ghe>/api/v3)")
		triggerAddr       = fs.String("trigger-listen", "127.0.0.1:8081", "address for the localhost-only trigger HTTP server (used by `syntropy start`)")
		commitAuthor      = fs.String("commit-author", "", "git commit author name (default: host .gitconfig)")
		commitEmail       = fs.String("commit-email", "", "git commit author email (default: host .gitconfig)")
		stuckThreshold    = fs.Duration("reconciler-stuck-threshold", reconcilerStuckThresholdDefault, "how long a Run may sit in Working/Discovering with no progress before the reconciler re-triggers it (see ADR-0033)")
		retriggerCooldown = fs.Duration("reconciler-retrigger-cooldown", reconcilerRetriggerCooldownDefault, "how long a Run is left alone after the reconciler re-triggers it before it can be re-triggered again")
		retentionPeriod   = fs.Duration("retention-period", retentionPeriodDefault, "how long a terminal (completed/cancelled) Run's records and on-disk run directory are kept before the retention sweep deletes them; 0 disables the sweep (see ADR-0071)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	unsetNestedClaudeCodeEnv()
	if *storePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		*storePath = home + "/.syntropy/store.db"
	}
	// --public-base-url is only required for webhook-mode Runs. Poll mode
	// (the default since ADR-0031) doesn't need a public URL. If a webhook
	// Run is triggered and this is empty, setup() will fail with a clear
	// error referencing this flag.

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	providers, err := buildProviders(logger, *gitlabBaseURL, *githubBaseURL)
	if err != nil {
		return fmt.Errorf("provider setup: %w", err)
	}
	if len(providers) == 0 {
		logger.Warn("no providers configured — set GITLAB_TOKEN / GITHUB_TOKEN or run `glab auth login`")
	}

	// storePath is always non-empty here (defaulted above), so OpenSqlite
	// (rather than store.Open) is used directly: it hands back the shared
	// *sql.DB so the EventStreamer's durable event log lives in the same
	// sqlite file as the RecordStore/TimeoutStore, not a second handle.
	backend, err := store.OpenSqlite(*storePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	recordStore, timeoutStore := backend.RecordStore(), backend.TimeoutStore()

	// Per-Run filesystem layout sits next to the store file. If --store is
	// /tmp/x/store.db, runs root is /tmp/x/runs/. Both happily live under
	// ~/.syntropy/ when --store takes its default.
	runsRoot := filepath.Join(filepath.Dir(*storePath), "runs")

	secrets := webhook.NewSecretRegistry()
	if err := rehydrateSecrets(context.Background(), recordStore, secrets, logger); err != nil {
		logger.Warn("secret rehydration encountered errors; some Runs may have empty registry entries", "err", err)
	}
	runners := runner.NewRegistry()
	runners.Register(claude.NewRunner("")) // "claude" on $PATH; ADR-0004 + ADR-0027

	// Commit author falls back to the host's git config when blank — which is
	// what we want when pushing to a shared repo where the platform expects
	// verified email addresses. Override via --commit-author / --commit-email
	// (rare; useful for shared-bot deployments).
	gitClient := git.NewExec(*commitAuthor, *commitEmail)

	eventStreamer := eventstream.New(backend.DB())
	wf := refactorsweep.Build(workflowName, refactorsweep.Deps{
		RecordStore:   recordStore,
		TimeoutStore:  timeoutStore,
		EventStreamer: eventStreamer,
		RoleScheduler: memrolescheduler.New(),
		Providers:     providers,
		Runners:       runners,
		Git:           gitClient,
		Secrets:       secrets,
		PublicBaseURL: *publicBaseURL,
		RunsRoot:      runsRoot,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wf.Run(ctx)
	defer wf.Stop()

	dispatcher := func(ctx context.Context, runID string, ev provider.Event) error {
		logger.Info("webhook received",
			"run_id", runID,
			"kind", ev.Kind,
			"mr_iid", ev.MR.IID,
			"author", ev.Author.Handle,
		)
		// Look up the Run's foreignID + current status so we can route the
		// callback through the workflow library, which loads the Run by
		// (workflowName, foreignID) and only fires the callback if status
		// matches.
		rec, err := recordStore.Lookup(ctx, runID)
		if err != nil {
			return fmt.Errorf("dispatcher: lookup run %s: %w", runID, err)
		}
		buf, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("dispatcher: marshal event: %w", err)
		}
		status := refactorsweep.AgentStatus(rec.Status)
		if err := wf.Callback(ctx, rec.ForeignID, status, bytes.NewReader(buf)); err != nil {
			return fmt.Errorf("dispatcher: workflow.Callback: %w", err)
		}
		return nil
	}
	// TODO: secret rehydration on daemon restart. Currently if the daemon
	// restarts, the in-memory SecretRegistry is empty until each Run's
	// setup() runs again — which it won't, because Runs past Initiated
	// don't re-enter setup. Inbound webhooks will be rejected as
	// "unknown runID" until a follow-up commit iterates the store at
	// startup and re-populates the registry from AgentState.WebhookSecret.
	// The webhook server reuses the same SecretRegistry the workflow
	// populates from setup() — single source of truth.
	whSrv := webhook.NewServer(providers, dispatcher, secrets)

	// Two HTTP listeners (ADR-0028): the public one carries the webhook +
	// health routes; the localhost-only one carries /trigger so the public
	// URL doesn't expose a way for outside parties to start Runs.
	publicMux := http.NewServeMux()
	whSrv.Mount(publicMux)
	publicSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           publicMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	localMux := http.NewServeMux()
	localMux.HandleFunc("/trigger", triggerHandler(wf, logger))
	localMux.HandleFunc("/status", statusHandler(recordStore, logger))
	localMux.HandleFunc("/control", controlHandler(wf, recordStore, logger))
	localSrv := &http.Server{
		Addr:              *triggerAddr,
		Handler:           localMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	srvErrCh := make(chan error, 2)
	go func() {
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErrCh <- fmt.Errorf("public listener: %w", err)
		}
	}()
	go func() {
		if err := localSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErrCh <- fmt.Errorf("trigger listener: %w", err)
		}
	}()

	// Start the polling loop — ingests events for Runs whose EventSource
	// is "poll" (the default; ADR-0031). Web-hook-mode Runs co-exist;
	// the poller's RunSource just iterates active Runs and the poller
	// only acts on the ones with InFlight MRs.
	pollerLoop := &poller.Loop{
		Interval:   30 * time.Second,
		Providers:  providers,
		Source:     &poller.StoreSource{Store: recordStore, WorkflowName: workflowName, Decode: decodeActiveRun},
		Dispatcher: dispatcher,
		Logger:     logger,
	}
	go pollerLoop.Run(ctx)

	// Start the reconciliation sweep — detects Runs stuck on a lost
	// in-memory event (ADR-0033's EventStreamer has no durable queue) and
	// re-triggers them. See internal/reconciler's package doc.
	sweeper := buildSweeper(recordStore, eventStreamer, *stuckThreshold, *retriggerCooldown, logger)
	go sweeper.Run(ctx)

	// Start the retention sweep — deletes terminal Runs (and their on-disk
	// run directory) once they're older than --retention-period. See
	// internal/retention's package doc and ADR-0071.
	retentionSweeper := buildRetentionSweeper(recordStore, gitClient, runsRoot, *retentionPeriod, logger)
	go retentionSweeper.Run(ctx)

	fmt.Fprintf(os.Stderr, "%s\n", daemonBannerLine())
	logger.Info("syntropy daemon started",
		"version", version,
		"listen", *listenAddr,
		"trigger_listen", *triggerAddr,
		"public_base_url", *publicBaseURL,
		"workflow", workflowName,
		"store", *storePath,
		"runs_root", runsRoot,
		"retention_period", *retentionPeriod,
	)
	logger.Warn("v1 scaffold — runners, step bodies, and CLI commands are stubs; see DESIGN.md for the build roadmap")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigCh:
		logger.Info("shutdown signal received, stopping...")
	case err := <-srvErrCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = publicSrv.Shutdown(shutdownCtx)
	_ = localSrv.Shutdown(shutdownCtx)
	return nil
}

// decodeActiveRun turns a workflow.Record into a poller.ActiveRun by
// unmarshalling AgentState and projecting the fields the poller needs.
// Returns false for inactive / undecodable / non-poll Runs so they're
// silently skipped.
func decodeActiveRun(object []byte) (poller.ActiveRun, bool) {
	var s refactorsweep.AgentState
	if err := workflow.Unmarshal(object, &s); err != nil {
		return poller.ActiveRun{}, false
	}
	if !s.IsPollMode() {
		return poller.ActiveRun{}, false // webhook-mode Runs handled by the HTTP server
	}
	if len(s.InFlight) == 0 {
		return poller.ActiveRun{}, false // nothing to poll
	}
	return poller.ActiveRun{
		Provider:            s.ProviderName,
		ProjectID:           s.ProjectID,
		Author:              s.Author,
		InFlight:            s.InFlight,
		LastSeenNoteIDs:     s.LastSeenNoteIDs,
		LastSeenNoteCursors: s.LastSeenNoteIDsByStream,
		LastMRStates:        s.LastMRStates,
	}, true
}

// rehydrateSecrets iterates active Runs in the record store and re-populates
// the in-memory SecretRegistry from AgentState.WebhookSecret. Required on
// daemon restart — the registry lives in memory, so without this every
// inbound webhook for an existing Run would 401 after a restart.
//
// "Active" means: not terminal (Completed/Failed/Cancelled) AND has a
// non-empty WebhookSecret. Runs whose secret somehow ended up empty are
// logged and skipped (rather than failing the whole startup).
//
// See ADR-0029.
func rehydrateSecrets(ctx context.Context, store workflow.RecordStore, secrets *webhook.SecretRegistry, logger *slog.Logger) error {
	const pageSize = 200
	var offset int64
	rehydrated := 0
	for {
		records, err := store.List(ctx, workflowName, offset, pageSize, workflow.OrderTypeAscending)
		if err != nil {
			return fmt.Errorf("list records at offset %d: %w", offset, err)
		}
		if len(records) == 0 {
			break
		}
		for _, rec := range records {
			if rec.RunState.Finished() {
				continue
			}
			status := refactorsweep.AgentStatus(rec.Status)
			if !isActiveStatus(status) {
				continue
			}
			var state refactorsweep.AgentState
			if err := workflow.Unmarshal(rec.Object, &state); err != nil {
				logger.Warn("rehydrate: unmarshal AgentState failed; skipping",
					"run_id", rec.RunID, "err", err)
				continue
			}
			if state.WebhookSecret == "" || state.ProviderName == "" {
				continue
			}
			secrets.Set(state.ProviderName, rec.RunID, state.WebhookSecret)
			rehydrated++
		}
		if int64(len(records)) < pageSize {
			break
		}
		offset += int64(len(records))
	}
	if rehydrated > 0 {
		logger.Info("rehydrated webhook secrets from store", "count", rehydrated)
	}
	return nil
}

// isActiveStatus reports whether a Run in this state is expected to receive
// further webhook callbacks. Used by rehydrateSecrets to skip terminals.
func isActiveStatus(s refactorsweep.AgentStatus) bool {
	switch s {
	case refactorsweep.StatusCompleted,
		refactorsweep.StatusFailed,
		refactorsweep.StatusCancelled:
		return false
	}
	return true
}

// buildProviders registers providers from credentials. For GitLab we try
// GITLAB_TOKEN env first (PAT, sent as PRIVATE-TOKEN); if absent, fall
// back to the user's `glab auth login` token from the glab config
// (OAuth, sent as Bearer). The fallback is what lets a personal-laptop
// spike work without minting a separate PAT — ADR-0031.
func buildProviders(logger *slog.Logger, gitlabBase, githubBase string) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}

	if tok := os.Getenv("GITLAB_TOKEN"); tok != "" {
		p, err := gitlab.New(gitlab.Config{BaseURL: gitlabBase, Token: tok, AuthMode: gitlab.AuthPAT})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "gitlab", "auth", "private-token")
	} else if _, err := gitlab.LoadGlabToken(""); err == nil {
		// Re-resolve via TokenSource on every request rather than caching
		// the token read above — glab's OAuth access token is short-lived
		// and glab itself transparently refreshes it in its own config file
		// on every `glab` invocation. A one-time snapshot would silently go
		// stale for the life of this daemon process (see ADR-0063); the
		// LoadGlabToken call above is just a fail-fast check that a token
		// exists at all right now.
		//
		// RefreshGlabToken (not a plain LoadGlabToken) pokes glab itself
		// (`glab api user`) before reading the file — glab only refreshes
		// its own stored access token lazily, when something invokes it, so
		// reading fresh from disk isn't sufficient if nothing has triggered
		// that refresh recently. Found live: the daemon's own GitLab calls
		// 401'd on a genuinely-expired on-disk token while `glab auth
		// status`, run moments later, showed a healthy login (ADR-0065).
		p, err := gitlab.New(gitlab.Config{
			BaseURL:     gitlabBase,
			TokenSource: func() (string, error) { return gitlab.RefreshGlabToken("") },
			AuthMode:    gitlab.AuthBearer,
		})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "gitlab", "auth", "glab-oauth")
	} else if !errors.Is(err, gitlab.ErrGlabNotConfigured) {
		return nil, fmt.Errorf("gitlab: load glab token: %w", err)
	}

	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		p, err := github.New(github.Config{BaseURL: githubBase, Token: tok})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "github", "auth", "env-token")
	} else if _, err := github.LoadGhToken(""); err == nil {
		// Re-resolve via TokenSource on every request rather than caching
		// the token read above — same staleness bug as the GitLab provider
		// had (ADR-0063/0065). LoadGhToken already shells out to `gh auth
		// token` live on every call (unlike glab's plain config-file read),
		// so no separate "poke" step is needed here; the LoadGhToken call
		// above is just a fail-fast check that gh is logged in at all
		// right now.
		p, err := github.New(github.Config{
			BaseURL:     githubBase,
			TokenSource: func() (string, error) { return github.LoadGhToken("") },
		})
		if err != nil {
			return nil, err
		}
		out[p.Name()] = p
		logger.Info("provider registered", "name", "github", "auth", "gh-oauth")
	} else if !errors.Is(err, github.ErrGhNotConfigured) {
		return nil, fmt.Errorf("github: load gh token: %w", err)
	}
	return out, nil
}

// triggerRequest is the JSON body of POST /trigger. The CLI's cmdStart
// builds one of these (from --spec or --units flags) and posts it to the
// localhost-only trigger listener. The daemon turns it into an AgentState
// and calls wf.Trigger.
type triggerRequest struct {
	Mode         string   `json:"mode"` // "spec" | "sweep" — see ADR-0024
	Goal         string   `json:"goal"`
	ProviderName string   `json:"provider"`
	ProjectID    string   `json:"project"`
	RunnerName   string   `json:"runner"`
	RunnerModel  string   `json:"runner_model,omitempty"` // spec's `model:` override; empty means the runner's default
	BaseRepo     string   `json:"base_repo"`
	BaseBranch   string   `json:"base_branch"`
	Concurrency  int      `json:"concurrency"`
	Units        []string `json:"units,omitempty"`     // sweep mode
	SpecPath     string   `json:"spec_path,omitempty"` // spec mode
	SpecBody     string   `json:"spec_body,omitempty"` // spec mode
	DraftMRs     bool     `json:"draft_mrs,omitempty"` // open MRs as Draft / WIP
	ForeignID    string   `json:"foreign_id,omitempty"`
}

type triggerResponse struct {
	RunID     string `json:"run_id"`
	ForeignID string `json:"foreign_id"`
}

// triggerHandler validates the request, builds an AgentState, calls
// wf.Trigger, and responds with the assigned run ID. Used only on the
// localhost-only trigger listener (ADR-0028).
func triggerHandler(wf *workflow.Workflow[refactorsweep.AgentState, refactorsweep.AgentStatus], logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req triggerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.ProviderName == "" || req.ProjectID == "" || req.RunnerName == "" {
			http.Error(w, "provider, project, and runner are required", http.StatusBadRequest)
			return
		}
		if req.Mode == "" {
			if len(req.Units) > 0 {
				req.Mode = refactorsweep.ModeSweep
			} else if req.SpecBody != "" || req.SpecPath != "" {
				req.Mode = refactorsweep.ModeSpec
			} else {
				http.Error(w, "neither units nor spec provided", http.StatusBadRequest)
				return
			}
		}
		if req.Concurrency <= 0 {
			req.Concurrency = 1
		}
		if req.BaseBranch == "" {
			req.BaseBranch = "main"
		}
		foreignID := req.ForeignID
		if foreignID == "" {
			foreignID = uuid.NewString()
		}

		state := &refactorsweep.AgentState{
			Goal:         req.Goal,
			Mode:         req.Mode,
			ProviderName: req.ProviderName,
			ProjectID:    req.ProjectID,
			BaseRepo:     req.BaseRepo,
			BaseBranch:   req.BaseBranch,
			RunnerName:   req.RunnerName,
			RunnerModel:  req.RunnerModel,
			Concurrency:  req.Concurrency,
			Queue:        req.Units,
			SpecPath:     req.SpecPath,
			SpecBody:     req.SpecBody,
			DraftMRs:     req.DraftMRs,
			InFlight:     map[string]provider.MR{},
		}

		runID, err := wf.Trigger(r.Context(), foreignID,
			workflow.WithInitialValue[refactorsweep.AgentState, refactorsweep.AgentStatus](state))
		if err != nil {
			http.Error(w, "trigger: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("triggered run",
			"run_id", runID, "foreign_id", foreignID,
			"mode", req.Mode, "provider", req.ProviderName,
			"project", req.ProjectID,
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(triggerResponse{RunID: runID, ForeignID: foreignID})
	}
}

// --- /status and /control handlers ---

// runStatusResponse is the JSON shape returned by GET /status?run_id=xxx.
type runStatusResponse struct {
	RunID               string `json:"run_id"`
	ForeignID           string `json:"foreign_id"`
	Status              string `json:"status"`
	Goal                string `json:"goal"`
	Mode                string `json:"mode"`
	Provider            string `json:"provider"`
	Project             string `json:"project"`
	Completed           int    `json:"completed"`
	Blacklisted         int    `json:"blacklisted"`
	InFlight            int    `json:"in_flight"`
	Queued              int    `json:"queued"`
	SubagentInvocations int    `json:"subagent_invocations"`
	TotalTokens         int    `json:"total_tokens"`
	MaxTokens           int    `json:"max_tokens,omitempty"`
	MaxUnits            int    `json:"max_units,omitempty"`
	EventsSeen          int    `json:"events_seen"`
	EventsSkipped       int    `json:"events_skipped"`
	PauseReason         string `json:"pause_reason,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	// AutoPaused is true when RunState == workflow.RunStatePaused because
	// the PauseAfterErrCount circuit breaker tripped (ADR-0062) — distinct
	// from our own business-level AgentStatus.StatusPaused. Such a Run
	// can't be resumed via the normal /control callback dispatch (no
	// AddCallback is registered for whatever Status it was stuck in), so
	// callers must route to the direct-store-write revival path instead;
	// see cmdResume.
	AutoPaused  bool          `json:"auto_paused,omitempty"`
	StartedAt   time.Time     `json:"started_at,omitempty"`
	UpdatedAt   time.Time     `json:"updated_at"`
	RecentTurns []turnSummary `json:"recent_turns,omitempty"`
}

// turnSummary is the compact representation of a Turn for the status command.
type turnSummary struct {
	Phase   string `json:"phase"`
	UnitID  string `json:"unit_id,omitempty"`
	Tokens  int    `json:"tokens"`
	Summary string `json:"summary"`
}

// statusHandler returns a JSON status summary. GET /status?run_id=xxx returns
// one run; GET /status returns all runs (compact listing).
func statusHandler(rs workflow.RecordStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()
		runID := r.URL.Query().Get("run_id")
		if runID != "" {
			rec, err := rs.Lookup(ctx, runID)
			if err != nil {
				http.Error(w, "run not found: "+err.Error(), http.StatusNotFound)
				return
			}
			resp, err := recordToStatus(rec)
			if err != nil {
				http.Error(w, "decode run: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// List all runs.
		records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
		if err != nil {
			http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var out []runStatusResponse
		for _, rec := range records {
			resp, err := recordToStatus(&rec)
			if err != nil {
				logger.Warn("status: decode run", "run_id", rec.RunID, "err", err)
				continue
			}
			out = append(out, resp)
		}
		if out == nil {
			out = []runStatusResponse{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// isAutoPaused reports whether rec was paused by the PauseAfterErrCount
// circuit breaker (ADR-0062) rather than our own business-level
// AgentStatus.StatusPaused (a human decision point, set by /syntropy
// pause). A Run in this state cannot be resumed via the normal /control
// callback dispatch — see cmdResume.
func isAutoPaused(rec *workflow.Record) bool {
	return rec.RunState == workflow.RunStatePaused && refactorsweep.AgentStatus(rec.Status) != refactorsweep.StatusPaused
}

func recordToStatus(rec *workflow.Record) (runStatusResponse, error) {
	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		return runStatusResponse{}, err
	}

	// Last 5 turns for the status display.
	const maxRecentTurns = 5
	turns := state.History
	if len(turns) > maxRecentTurns {
		turns = turns[len(turns)-maxRecentTurns:]
	}
	recent := make([]turnSummary, len(turns))
	for i, t := range turns {
		excerpt := t.Summary
		if len(excerpt) > 80 {
			excerpt = excerpt[:77] + "..."
		}
		recent[i] = turnSummary{Phase: string(t.Phase), UnitID: t.UnitID, Tokens: t.Tokens, Summary: excerpt}
	}

	statusStr := refactorsweep.AgentStatus(rec.Status).String()
	autoPaused := isAutoPaused(rec)
	// A Run auto-paused by the workflow.PauseAfterErrCount circuit breaker
	// (ADR-0062) has RunState == RunStatePaused while Status still reports
	// whatever step it was stuck in (Working, Discovering, ...) — distinct
	// from our own business-level AgentStatus.StatusPaused (a human decision
	// point). Surface it explicitly; otherwise it's indistinguishable from a
	// normal in-progress Run in `status`/`list` output.
	if autoPaused {
		statusStr = fmt.Sprintf("%s (auto-paused: %s)", statusStr, rec.Meta.RunStateReason)
	}

	return runStatusResponse{
		RunID:               rec.RunID,
		ForeignID:           rec.ForeignID,
		Status:              statusStr,
		AutoPaused:          autoPaused,
		Goal:                state.Goal,
		Mode:                state.Mode,
		Provider:            state.ProviderName,
		Project:             state.ProjectID,
		Completed:           len(state.Completed),
		Blacklisted:         len(state.Blacklisted),
		InFlight:            len(state.InFlight),
		Queued:              len(state.Queue),
		SubagentInvocations: state.SubagentInvocations,
		TotalTokens:         state.TotalTokens,
		MaxTokens:           state.Budget.MaxTokens,
		MaxUnits:            state.Budget.MaxUnits,
		EventsSeen:          state.EventsSeen,
		EventsSkipped:       state.EventsSkippedByFilter,
		PauseReason:         state.PauseReason,
		LastError:           state.LastError,
		StartedAt:           state.StartedAt,
		UpdatedAt:           rec.UpdatedAt,
		RecentTurns:         recent,
	}, nil
}

// controlRequest is the JSON body for POST /control.
type controlRequest struct {
	RunID string `json:"run_id"`
	Verb  string `json:"verb"` // abandon | resume | pause | stop | skip | retry | prompt | status
	Args  string `json:"args,omitempty"`
}

// controlHandler injects a synthetic /syntropy control command into a Run by
// constructing a fake author NoteAdded event and calling wf.Callback. The
// event's Author is set to the Run's recorded author so the IsAuthor check in
// resume() passes. Only safe on the localhost-only trigger listener (ADR-0028).
func controlHandler(wf *workflow.Workflow[refactorsweep.AgentState, refactorsweep.AgentStatus], rs workflow.RecordStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req controlRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.RunID == "" || req.Verb == "" {
			http.Error(w, "run_id and verb are required", http.StatusBadRequest)
			return
		}
		ctx := r.Context()

		rec, err := rs.Lookup(ctx, req.RunID)
		if err != nil {
			http.Error(w, "run not found: "+err.Error(), http.StatusNotFound)
			return
		}
		var state refactorsweep.AgentState
		if err := workflow.Unmarshal(rec.Object, &state); err != nil {
			http.Error(w, "decode run state: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Pick any in-flight MR to post the ack comment against. If none,
		// the control handlers will attempt to post to MR IID=0 which will
		// fail gracefully (ignored error).
		var mr provider.MR
		for _, m := range state.InFlight {
			mr = m
			break
		}

		noteBody := "/syntropy " + req.Verb
		if req.Args != "" {
			noteBody += " " + req.Args
		}
		ev := provider.Event{
			Kind:      provider.EventNoteAdded,
			ProjectID: state.ProjectID,
			MR:        mr,
			Author:    state.Author,
			IsAuthor:  true,
			Note: provider.Note{
				Body: noteBody,
			},
			ReceivedAt: time.Now().UnixNano(),
		}
		buf, err := json.Marshal(ev)
		if err != nil {
			http.Error(w, "marshal event: "+err.Error(), http.StatusInternalServerError)
			return
		}

		status := refactorsweep.AgentStatus(rec.Status)
		if err := wf.Callback(ctx, rec.ForeignID, status, bytes.NewReader(buf)); err != nil {
			http.Error(w, "callback: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("injected control command", "run_id", req.RunID, "verb", req.Verb)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "verb": req.Verb, "run_id": req.RunID})
	}
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	var (
		specPath    = fs.String("spec", "", "path to a spec markdown file (spec mode; mutually exclusive with --units)")
		unitsCSV    = fs.String("units", "", "comma-separated unit IDs (sweep mode; mutually exclusive with --spec)")
		goal        = fs.String("goal", "", "one-sentence description (sweep mode; ignored in spec mode where the spec's `goal:` is used)")
		providerArg = fs.String("provider", "", "provider name (gitlab | github)")
		projectArg  = fs.String("project", "", "provider project ID, e.g. acme/example")
		runnerArg   = fs.String("runner", "claude", "runner name")
		modelArg    = fs.String("model", "", "runner model override (default: runner's default, or spec's `model:`)")
		baseBranch  = fs.String("base-branch", "", "base branch (default: main, or spec's `base_branch:`)")
		baseRepo    = fs.String("base-repo", "", "local path to a git checkout with origin remote (required)")
		concurrency = fs.Int("concurrency", 0, "max in-flight MRs (default 1, or spec's `concurrency:`)")
		draftMRs    = fs.Bool("draft-mrs", false, "open MRs as Draft / WIP (recommended for spikes against shared repos)")
		daemonURL   = fs.String("daemon", "http://127.0.0.1:8081", "daemon trigger endpoint")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*specPath == "" && *unitsCSV == "") || (*specPath != "" && *unitsCSV != "") {
		return errors.New("exactly one of --spec or --units must be provided")
	}

	req := triggerRequest{
		Goal:         *goal,
		ProviderName: *providerArg,
		ProjectID:    *projectArg,
		RunnerName:   *runnerArg,
		RunnerModel:  *modelArg,
		BaseRepo:     *baseRepo,
		BaseBranch:   *baseBranch,
		Concurrency:  *concurrency,
		DraftMRs:     *draftMRs,
	}

	if *specPath != "" {
		sp, err := spec.Parse(*specPath)
		if err != nil {
			return fmt.Errorf("parse spec: %w", err)
		}
		req.Mode = refactorsweep.ModeSpec
		req.SpecPath = sp.Path
		req.SpecBody = sp.Body
		req.DraftMRs = sp.DraftMRs
		// Spec frontmatter is authoritative unless explicitly overridden via flag.
		if req.Goal == "" {
			req.Goal = sp.Goal
		}
		if req.ProviderName == "" {
			req.ProviderName = sp.Provider
		}
		if req.ProjectID == "" {
			req.ProjectID = sp.Project
		}
		if req.RunnerName == "claude" && sp.Runner != "" {
			// "claude" is the flag default; defer to spec if it specified something.
			req.RunnerName = sp.Runner
		}
		if req.RunnerModel == "" {
			req.RunnerModel = sp.Model
		}
		if req.BaseRepo == "" {
			req.BaseRepo = sp.BaseRepo
		}
		if req.BaseBranch == "" {
			req.BaseBranch = sp.BaseBranch
		}
		if req.Concurrency == 0 {
			req.Concurrency = sp.Concurrency
		}
	} else {
		req.Mode = refactorsweep.ModeSweep
		req.Units = strings.Split(*unitsCSV, ",")
		for i, u := range req.Units {
			req.Units[i] = strings.TrimSpace(u)
		}
	}

	if req.RunnerModel == "" {
		// Neither --model nor (in spec mode) the spec's `model:` set one;
		// fall back to the default persisted by `syntropy setup` (ADR-0051).
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		cfg, err := config.Load(home)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		req.RunnerModel = cfg.Model
	}

	if req.ProviderName == "" || req.ProjectID == "" {
		return errors.New("provider and project are required (set --provider/--project or via spec frontmatter)")
	}
	if req.BaseRepo == "" {
		return errors.New("--base-repo is required (or set via spec's base_repo)")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal trigger request: %w", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, *daemonURL+"/trigger", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST %s/trigger: %w (is the daemon running?)", *daemonURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("trigger: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out triggerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	fmt.Printf("Triggered run %s (foreign id: %s, mode: %s)\n", out.RunID, out.ForeignID, req.Mode)

	printUpdateNotice()
	return nil
}

// printUpdateNotice checks (best-effort, cached) whether a newer syntropy
// release exists and prints a one-line notice if so. A failed check (e.g.
// offline) is silently ignored — it must never fail or slow down `start`
// itself, since the calling agent has already gotten what it needs.
func printUpdateNotice() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := update.Check(ctx, home, version, nil, "")
	if err != nil {
		return
	}
	if result.Available {
		fmt.Printf("A newer syntropy release is available: %s (you have %s)\n", result.Latest, version)
	}
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; when the daemon is unreachable the CLI falls back to the store (default: ~/.syntropy/store.db if it exists)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)

	url := *daemonURL + "/status"
	if runID != "" {
		full, err := resolveRunIDFromDaemon(*daemonURL, runID)
		if err != nil {
			// If the daemon is unreachable, fall back to the store when
			// possible; otherwise surface a hint pointing at --store.
			if isDaemonUnreachable(err) {
				if fallback, ok := tryStoreFallback(*storePath); ok {
					fmt.Fprintf(os.Stderr, "syntropy: daemon unreachable (%v); reading store directly\n", err)
					return directStatus(context.Background(), fallback, runID)
				}
				return daemonUnreachableError(*daemonURL, err)
			}
			return err
		}
		runID = full
		url += "?run_id=" + runID
	}
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		// Daemon unreachable — fall back to a direct sqlite read when the
		// store exists; otherwise emit a hint pointing at --store.
		if fallback, ok := tryStoreFallback(*storePath); ok {
			fmt.Fprintf(os.Stderr, "syntropy: daemon unreachable (%v); reading store directly\n", err)
			return directStatus(context.Background(), fallback, runID)
		}
		return daemonUnreachableError(*daemonURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	if runID != "" {
		var s runStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		printRunStatus(os.Stdout, s)
		return nil
	}

	var runs []runStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(runs) == 0 {
		fmt.Println("no runs found")
		return nil
	}
	fmt.Printf("%-10s  %-22s  %-9s  %s\n", "RUN ID", "UPDATED", "STATUS", "GOAL")
	for _, s := range runs {
		goal := s.Goal
		if len(goal) > 50 {
			goal = goal[:47] + "..."
		}
		fmt.Printf("%-10s  %-22s  %-9s  %s\n",
			s.RunID[:min(10, len(s.RunID))],
			s.UpdatedAt.Format("2006-01-02 15:04:05"),
			s.Status,
			goal,
		)
	}
	return nil
}

// directStatus prints a Run's state by reading directly from the sqlite store.
// It does not require the daemon to be running.
func directStatus(ctx context.Context, storePath, runID string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	if runID != "" {
		full, err := resolveRunIDFromStore(ctx, rs, runID)
		if err != nil {
			return err
		}
		rec, err := rs.Lookup(ctx, full)
		if err != nil {
			return fmt.Errorf("run %s not found: %w (hint: use 'syntropy list' or query the store directly)", full, err)
		}
		s, err := recordToStatus(rec)
		if err != nil {
			return fmt.Errorf("decode run state: %w", err)
		}
		printRunStatus(os.Stdout, s)
		return nil
	}

	records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(records) == 0 {
		fmt.Println("no runs found")
		return nil
	}
	fmt.Printf("%-10s  %-22s  %-9s  %s\n", "RUN ID", "UPDATED", "STATUS", "GOAL")
	for i := range records {
		s, err := recordToStatus(&records[i])
		if err != nil {
			continue
		}
		goal := s.Goal
		if len(goal) > 50 {
			goal = goal[:47] + "..."
		}
		fmt.Printf("%-10s  %-22s  %-9s  %s\n",
			s.RunID[:min(10, len(s.RunID))],
			s.UpdatedAt.Format("2006-01-02 15:04:05"),
			s.Status,
			goal,
		)
	}
	return nil
}

func printRunStatus(w io.Writer, s runStatusResponse) {
	fmt.Fprintf(w, "Run:      %s\n", s.RunID)
	fmt.Fprintf(w, "Status:   %s\n", s.Status)
	fmt.Fprintf(w, "Goal:     %s\n", s.Goal)
	fmt.Fprintf(w, "Provider: %s / %s\n", s.Provider, s.Project)
	fmt.Fprintf(w, "Mode:     %s\n", s.Mode)
	fmt.Fprintf(w, "Units:    %d completed, %d blacklisted, %d in-flight, %d queued\n",
		s.Completed, s.Blacklisted, s.InFlight, s.Queued)
	tokenStr := fmt.Sprintf("%d", s.TotalTokens)
	if s.MaxTokens > 0 {
		tokenStr = fmt.Sprintf("%d / %d", s.TotalTokens, s.MaxTokens)
	}
	fmt.Fprintf(w, "Tokens:   %s\n", tokenStr)
	fmt.Fprintf(w, "Invocations: %d (events: %d, skipped: %d)\n",
		s.SubagentInvocations, s.EventsSeen, s.EventsSkipped)
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(w, "Started:  %s\n", s.StartedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "Updated:  %s\n", s.UpdatedAt.Format(time.RFC3339))
	if s.PauseReason != "" {
		fmt.Fprintf(w, "Paused:   %s\n", s.PauseReason)
	}
	if s.LastError != "" {
		fmt.Fprintf(w, "Error:    %s\n", s.LastError)
	}
	if len(s.RecentTurns) > 0 {
		fmt.Fprintf(w, "\nRecent turns (last %d):\n", len(s.RecentTurns))
		for _, t := range s.RecentTurns {
			unit := t.UnitID
			if unit == "" {
				unit = "-"
			}
			fmt.Fprintf(w, "  [%-18s] %-16s tokens=%-6d %q\n", t.Phase, unit, t.Tokens, t.Summary)
		}
	}
}

// cmdWait blocks until a triggered Run reaches its first real checkpoint —
// the point at which there's actually something to look at (a unit opened,
// completed, or blacklisted; or the Run reached a state that needs human
// attention). It's meant to replace the fire-and-forget pattern of
// `syntropy start` immediately handing control back with nothing to show:
// callers (notably the Skill) can call `wait` right after triggering and
// then report something concrete instead of a bare Run ID.
//
// Polling is local only — status reads (daemon HTTP, or a direct sqlite
// read when the daemon is unreachable) rather than tokens — so this is safe
// to call without burning budget.
func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; used when the daemon is unreachable (default: ~/.syntropy/store.db if it exists)")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up and exit non-zero if no checkpoint is reached within this long")
	interval := fs.Duration("interval", 3*time.Second, "local poll interval (no tokens spent; just a status read)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)
	if runID == "" {
		return errors.New("usage: syntropy wait <run-id>")
	}
	if *interval <= 0 {
		return errors.New("--interval must be positive")
	}

	deadline := time.Now().Add(*timeout)
	for {
		s, err := fetchRunStatus(*daemonURL, *storePath, runID)
		if err != nil {
			return err
		}
		if reachedFirstCheckpoint(s) {
			printRunStatus(os.Stdout, s)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for run %s to reach its first checkpoint (still %s)", *timeout, s.RunID, s.Status)
		}
		time.Sleep(*interval)
	}
}

// reachedFirstCheckpoint reports whether s reflects a Run that has moved
// past pure setup (Initiated/Discovering with nothing to show yet) — either
// because a unit has actually landed somewhere (in flight, completed, or
// blacklisted), or because the Run reached a state that needs human
// attention (paused, awaiting a merge, or finished one way or another).
func reachedFirstCheckpoint(s runStatusResponse) bool {
	if s.InFlight > 0 || s.Completed > 0 || s.Blacklisted > 0 {
		return true
	}
	checkpointStatuses := []string{
		refactorsweep.StatusAwaitingMerge.String(),
		refactorsweep.StatusPaused.String(),
		refactorsweep.StatusAwaitingAbandonConfirm.String(),
		refactorsweep.StatusCompleted.String(),
		refactorsweep.StatusFailed.String(),
		refactorsweep.StatusCancelled.String(),
	}
	for _, cs := range checkpointStatuses {
		if strings.HasPrefix(s.Status, cs) {
			return true
		}
	}
	return false
}

// fetchRunStatus resolves runID (which may be a prefix) and returns its
// current status, preferring the daemon and falling back to a direct sqlite
// read when the daemon is unreachable — the same fallback cmdStatus uses.
func fetchRunStatus(daemonURL, storePath, runID string) (runStatusResponse, error) {
	s, err := daemonStatusFor(daemonURL, runID)
	if err == nil {
		return s, nil
	}
	if !isDaemonUnreachable(err) {
		return runStatusResponse{}, err
	}
	fallback, ok := tryStoreFallback(storePath)
	if !ok {
		return runStatusResponse{}, daemonUnreachableError(daemonURL, err)
	}
	ctx := context.Background()
	rs, _, err := store.Open(fallback)
	if err != nil {
		return runStatusResponse{}, fmt.Errorf("open store %s: %w", fallback, err)
	}
	full, err := resolveRunIDFromStore(ctx, rs, runID)
	if err != nil {
		return runStatusResponse{}, err
	}
	rec, err := rs.Lookup(ctx, full)
	if err != nil {
		return runStatusResponse{}, fmt.Errorf("run %s not found: %w", full, err)
	}
	return recordToStatus(rec)
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	storePath := fs.String("store", "", "path to sqlite store (default: ~/.syntropy/store.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return directList(context.Background(), *storePath)
}

// directList prints all Runs from the sqlite store, one per line, sorted newest first.
func directList(ctx context.Context, storePath string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(records) == 0 {
		fmt.Println("no runs found")
		return nil
	}
	for i := range records {
		rec := &records[i]
		var state refactorsweep.AgentState
		if err := workflow.Unmarshal(rec.Object, &state); err != nil {
			continue
		}
		runID := rec.RunID
		if len(runID) > 13 {
			runID = runID[:13] + "..."
		}
		mode := state.Mode
		if mode == "" {
			mode = "sweep"
		}
		status := refactorsweep.AgentStatus(rec.Status).String()
		if isAutoPaused(rec) {
			status = fmt.Sprintf("%s (auto-paused)", status)
		}
		turns := len(state.History)
		goal := state.Goal
		if len(goal) > 40 {
			goal = `"` + goal[:37] + `..."`
		} else {
			goal = `"` + goal + `"`
		}
		fmt.Printf("%-16s  %-5s  %-9s  %2d turns  goal: %s\n",
			runID, mode, status, turns, goal)
	}
	return nil
}

// resolveRunIDPrefix returns the unique candidate matched by strings.HasPrefix.
// Full UUIDs fall through cleanly (they match only themselves). Zero or
// multiple matches return errors; the multi-match error lists every hit so
// the user can pick.
func resolveRunIDPrefix(candidates []string, prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("empty run-id")
	}
	var matches []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no run matches prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("prefix %q matches multiple runs — please be more specific:\n  %s", prefix, strings.Join(matches, "\n  "))
	}
}

// resolveRunIDFromStore resolves a prefix against the sqlite store.
func resolveRunIDFromStore(ctx context.Context, rs workflow.RecordStore, prefix string) (string, error) {
	records, err := rs.List(ctx, workflowName, 0, 500, workflow.OrderTypeDescending)
	if err != nil {
		return "", fmt.Errorf("list runs: %w", err)
	}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.RunID)
	}
	return resolveRunIDPrefix(ids, prefix)
}

// resolveRunIDFromDaemon resolves a prefix by fetching /status from the daemon
// and filtering. Transport-level errors are returned unwrapped so callers can
// detect them via isDaemonUnreachable.
func resolveRunIDFromDaemon(daemonURL, prefix string) (string, error) {
	resp, err := http.Get(daemonURL + "/status") //nolint:noctx
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var runs []runStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	return resolveRunIDPrefix(ids, prefix)
}

// daemonStatusFor resolves prefix against the daemon's /status and returns
// the matching run's full response — used by cmdResume to detect the
// RunStatePaused-while-daemon-reachable case (ADR-0062), which /control
// can't handle via wf.Callback since no callback is registered for whatever
// business Status the circuit breaker tripped on.
func daemonStatusFor(daemonURL, prefix string) (runStatusResponse, error) {
	resp, err := http.Get(daemonURL + "/status") //nolint:noctx
	if err != nil {
		return runStatusResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return runStatusResponse{}, fmt.Errorf("status: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var runs []runStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		return runStatusResponse{}, fmt.Errorf("decode: %w", err)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	full, err := resolveRunIDPrefix(ids, prefix)
	if err != nil {
		return runStatusResponse{}, err
	}
	for _, r := range runs {
		if r.RunID == full {
			return r, nil
		}
	}
	return runStatusResponse{}, fmt.Errorf("run %s not found", full)
}

// isDaemonUnreachable reports transport-level HTTP failures (connection
// refused, timeout, DNS). http.Get returns errors only for transport failures
// — well-formed 4xx / 5xx come back via resp.StatusCode with a nil error — so
// a *url.Error in the chain is the reliable signal.
func isDaemonUnreachable(err error) bool {
	var urlErr *url.Error
	return err != nil && errors.As(err, &urlErr)
}

// daemonUnreachableError wraps a transport failure with a hint pointing users
// at --store as an alternative when the daemon isn't running.
func daemonUnreachableError(daemonURL string, err error) error {
	return fmt.Errorf("daemon at %s is unreachable: %w\nhint: pass --store /path/to/store.db to read the sqlite store directly (no daemon needed)", daemonURL, err)
}

// tryStoreFallback picks a store path for the offline-rescue path. --store
// wins; otherwise ~/.syntropy/store.db is used only when it exists — we
// don't silently create an empty store and pretend nothing is wrong.
func tryStoreFallback(userProvided string) (string, bool) {
	if userProvided != "" {
		return userProvided, true
	}
	sp, err := defaultStorePath("")
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(sp); err != nil {
		return "", false
	}
	return sp, true
}

func cmdAbandon(args []string) error {
	fs := flag.NewFlagSet("abandon", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; when the daemon is unreachable the CLI falls back to the store (default: ~/.syntropy/store.db if it exists)")
	reasonFlag := fs.String("reason", "", "optional reason for abandonment")
	gitlabBaseURL := fs.String("gitlab-base-url", "", "GitLab base URL (defaults to https://gitlab.com)")
	githubBaseURL := fs.String("github-base-url", "", "GitHub API base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)
	if runID == "" {
		return errors.New("usage: syntropy abandon <run-id>")
	}

	// A Run auto-paused by the PauseAfterErrCount circuit breaker (ADR-0062)
	// can't be abandoned via /control's wf.Callback dispatch either — the
	// same gap the resume fix closed (see cmdResume below): no callback is
	// registered for whatever business Status the breaker tripped on
	// (Working/Discovering/Initiated), so sendControl below would silently
	// no-op (HTTP 200, nothing actually happens — confirmed live: an
	// "abandon" sent this way left the Run's Updated timestamp and status
	// completely unchanged). Detect it up front and go straight to the
	// direct-store-write path, regardless of daemon reachability.
	if st, err := daemonStatusFor(*daemonURL, runID); err == nil && st.AutoPaused {
		fallback, ok := tryStoreFallback(*storePath)
		if !ok {
			return fmt.Errorf("run %s is auto-paused; abandoning it requires direct store access (pass --store or ensure ~/.syntropy/store.db exists)", runID)
		}
		return directAbandon(context.Background(), fallback, st.RunID, *reasonFlag, *gitlabBaseURL, *githubBaseURL)
	}

	// Try daemon first (preferred path: daemon handles two-tap confirmation
	// and provider-side MR cleanup gracefully). Resolve the prefix against
	// the daemon's view of the store so short IDs work.
	full, resolveErr := resolveRunIDFromDaemon(*daemonURL, runID)
	if resolveErr == nil {
		if err := sendControl(*daemonURL, full, "abandon", *reasonFlag); err == nil {
			return nil
		}
	} else if !isDaemonUnreachable(resolveErr) {
		return resolveErr
	}

	// Daemon not reachable — fall back to direct store manipulation. This is
	// the rescue path: no two-tap, immediate force-cancel. See ADR-0037.
	fallback, ok := tryStoreFallback(*storePath)
	if !ok {
		return daemonUnreachableError(*daemonURL, resolveErr)
	}
	fmt.Fprintln(os.Stderr, "syntropy: daemon unreachable; falling back to direct store write")
	return directAbandon(context.Background(), fallback, runID, *reasonFlag, *gitlabBaseURL, *githubBaseURL)
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	daemonURL := fs.String("daemon", "http://127.0.0.1:8081", "daemon address")
	storePath := fs.String("store", "", "path to sqlite store; when the daemon is unreachable the CLI falls back to the store (default: ~/.syntropy/store.db if it exists)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID := fs.Arg(0)
	if runID == "" {
		return errors.New("usage: syntropy resume <run-id>")
	}

	daemonSt, statusErr := daemonStatusFor(*daemonURL, runID)

	// A Run auto-paused by the PauseAfterErrCount circuit breaker (ADR-0062)
	// can't be resumed via /control's wf.Callback dispatch at all — no
	// callback is registered for whatever business Status it was stuck in
	// (Working/Discovering/Initiated), so sendControl below would silently
	// no-op (HTTP 200, nothing actually happens). Detect it up front via the
	// daemon's own /status and go straight to the direct-store-write path
	// regardless of whether the daemon is reachable; unlike the
	// Cancelled/Failed case, this one doesn't need a daemon restart to take
	// effect — the step's consumer is still actively subscribed, just
	// filtered out by RunState, so flipping RunState back to Running is
	// picked up on its next poll.
	if statusErr == nil && daemonSt.AutoPaused {
		fallback, ok := tryStoreFallback(*storePath)
		if !ok {
			return fmt.Errorf("run %s is auto-paused; resuming it requires direct store access (pass --store or ensure ~/.syntropy/store.db exists)", runID)
		}
		return directResume(context.Background(), fallback, daemonSt.RunID)
	}

	// A genuinely terminal Run (Failed/Cancelled, not the AutoPaused case
	// above) can't be revived via /control's "resume" verb either — that
	// path only handles the Paused → AwaitingMerge callback (see
	// directResume's own comment). Found live: sendControl's POST to
	// /control still returns HTTP 200 for a Failed run (the daemon accepts
	// the write; there's just no registered callback for a terminal status
	// to act on it), so the pre-fix code below reported "resume sent" with
	// no actual effect. Route straight to the direct-store-write revival
	// path — same one AutoPaused/daemon-unreachable already use — whenever
	// the daemon's own /status confirms the Run is Failed or Cancelled,
	// regardless of whether the daemon itself is reachable.
	if statusErr == nil && (daemonSt.Status == refactorsweep.StatusFailed.String() || daemonSt.Status == refactorsweep.StatusCancelled.String()) {
		fallback, ok := tryStoreFallback(*storePath)
		if !ok {
			return fmt.Errorf("run %s is %s; reviving it requires direct store access (pass --store or ensure ~/.syntropy/store.db exists)", runID, daemonSt.Status)
		}
		return directResume(context.Background(), fallback, daemonSt.RunID)
	}

	// Try daemon first (preferred: daemon can resume Paused → AwaitingMerge).
	// The daemon path handles the AwaitingMerge callback correctly; the direct
	// path is needed for Cancelled/Failed → Discovering revive.
	full, resolveErr := resolveRunIDFromDaemon(*daemonURL, runID)
	if resolveErr == nil {
		if err := sendControl(*daemonURL, full, "resume", ""); err == nil {
			return nil
		}
	} else if !isDaemonUnreachable(resolveErr) {
		return resolveErr
	}

	// Daemon not reachable — fall back to direct store write. The daemon must
	// be (re)started to process the outbox event that drives the Discovering
	// step. See ADR-0037.
	fallback, ok := tryStoreFallback(*storePath)
	if !ok {
		return daemonUnreachableError(*daemonURL, resolveErr)
	}
	fmt.Fprintln(os.Stderr, "syntropy: daemon unreachable; falling back to direct store write")
	return directResume(context.Background(), fallback, runID)
}

// defaultStorePath returns ~/.syntropy/store.db when path is blank.
func defaultStorePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return home + "/.syntropy/store.db", nil
}

// directAbandon force-cancels a Run by writing directly to the sqlite store.
// It does not require the daemon to be running. In-flight MRs are closed
// best-effort via the configured provider credentials.
func directAbandon(ctx context.Context, storePath, runID, reason, gitlabBaseURL, githubBaseURL string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	full, err := resolveRunIDFromStore(ctx, rs, runID)
	if err != nil {
		return err
	}
	runID = full
	rec, err := rs.Lookup(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %s not found: %w", runID, err)
	}
	if rec.RunState.Finished() {
		return fmt.Errorf("run %s is already in a terminal state (%s); nothing to abandon", runID, rec.RunState)
	}

	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		return fmt.Errorf("decode run state: %w", err)
	}

	// Close in-flight MRs best-effort via the provider.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	providers, _ := buildProviders(logger, gitlabBaseURL, githubBaseURL)
	if p, ok := providers[state.ProviderName]; ok {
		for _, mr := range state.InFlight {
			if cErr := p.CloseMR(ctx, mr.ProjectID, mr.IID); cErr != nil {
				fmt.Fprintf(os.Stderr, "syntropy: close MR #%d (best-effort): %v\n", mr.IID, cErr)
			}
		}
	}

	// Remove per-unit worktrees best-effort.
	runsRoot := filepath.Join(filepath.Dir(sp), "runs")
	g := git.NewExec("", "")
	for unitID := range state.InFlight {
		wt := filepath.Join(runsRoot, runID, "worktrees", unitID)
		if rErr := g.RemoveWorktree(ctx, state.BaseRepo, wt); rErr != nil {
			fmt.Fprintf(os.Stderr, "syntropy: remove worktree %s (best-effort): %v\n", wt, rErr)
		}
	}

	if reason == "" {
		reason = "abandoned via CLI"
	}
	state.LastError = reason

	obj, err := workflow.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	rec.Object = obj
	rec.Status = int(refactorsweep.StatusCancelled)
	rec.RunState = workflow.RunStateCancelled
	rec.Meta.Version++

	if err := rs.Store(ctx, rec); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	fmt.Printf("✓ Run %s cancelled (direct store write; reason: %s)\n", runID[:min(10, len(runID))], reason)
	return nil
}

// directResume revives a Cancelled or Failed Run to Discovering by writing
// directly to the sqlite store. The daemon must be running (or restarted)
// to process the outbox event and drive the Discovering step. See ADR-0037.
func directResume(ctx context.Context, storePath, runID string) error {
	sp, err := defaultStorePath(storePath)
	if err != nil {
		return err
	}
	rs, _, err := store.Open(sp)
	if err != nil {
		return fmt.Errorf("open store %s: %w", sp, err)
	}

	full, err := resolveRunIDFromStore(ctx, rs, runID)
	if err != nil {
		return err
	}
	runID = full
	rec, err := rs.Lookup(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %s not found: %w", runID, err)
	}

	status := refactorsweep.AgentStatus(rec.Status)

	// A Run auto-paused by the workflow.PauseAfterErrCount circuit breaker
	// (ADR-0062) has RunState == RunStatePaused while Status can be anything
	// the failing step happened to be in (Working, Discovering, Initiated —
	// none of which have a registered callback, so it can't be revived via
	// the normal /control "resume" path at all). Handle it distinctly:
	// restore the Status the step was actually stuck in, don't force it back
	// to Discovering the way the Cancelled/Failed/StatusPaused cases below
	// do.
	if rec.RunState == workflow.RunStatePaused && status != refactorsweep.StatusPaused {
		var state refactorsweep.AgentState
		if err := workflow.Unmarshal(rec.Object, &state); err != nil {
			return fmt.Errorf("decode run state: %w", err)
		}
		state.LastError = ""
		obj, err := workflow.Marshal(&state)
		if err != nil {
			return fmt.Errorf("marshal state: %w", err)
		}
		rec.Object = obj
		rec.RunState = workflow.RunStateRunning
		rec.Meta.Version++
		if err := rs.Store(ctx, rec); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		fmt.Printf("✓ Run %s revived to %s (auto-pause circuit breaker cleared; direct store write)\n", runID[:min(10, len(runID))], status)
		return nil
	}

	if status != refactorsweep.StatusCancelled && status != refactorsweep.StatusFailed && status != refactorsweep.StatusPaused {
		return fmt.Errorf("run %s is in status %s; only Cancelled, Failed, or Paused runs can be resumed via direct store write", runID, status)
	}

	var state refactorsweep.AgentState
	if err := workflow.Unmarshal(rec.Object, &state); err != nil {
		return fmt.Errorf("decode run state: %w", err)
	}

	state.LastError = ""
	state.PauseReason = ""

	obj, err := workflow.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	rec.Object = obj
	rec.Status = int(refactorsweep.StatusDiscovering)
	rec.RunState = workflow.RunStateRunning
	rec.Meta.Version++

	if err := rs.Store(ctx, rec); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	fmt.Printf("✓ Run %s revived to Discovering (direct store write)\n", runID[:min(10, len(runID))])
	fmt.Println("  The daemon must be running (or restarted) to pick up and process the outbox event.")
	return nil
}

// sendControl posts a control verb to the daemon's /control endpoint.
func sendControl(daemonURL, runID, verb, args string) error {
	req := controlRequest{RunID: runID, Verb: verb, Args: args}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := http.Post(daemonURL+"/control", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("POST /control: %w (is the daemon running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Printf("✓ %s sent to run %s\n", verb, runID)
	return nil
}

func cmdPhrases(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("usage: syntropy phrases <list|promote> [args]")
		return nil
	}
	switch args[0] {
	case "list", "promote":
		return fmt.Errorf("syntropy phrases %s: not implemented in scaffold", args[0])
	default:
		return fmt.Errorf("unknown subcommand %q (try list, promote)", args[0])
	}
}

// checkRepoConfig is cmdConfig's testable core: reads repoDir's
// .syntropy.yml and reports its missing fields, writing the same
// human/agent-readable summary cmdConfig prints to stdout, but returning
// the missing-field list instead of calling os.Exit — split out so tests
// can assert its output/return value in-process. See ADR-0083.
//
// It also prints the effective spec tool (ADR-0099's deferred
// consumption piece): repoDir's override if set, else the global default
// from ~/.syntropy/config.yaml, else syntropy's own default flow — so an
// agent following the syntropy Skill can read which spec tool to route
// to off this command's output instead of resolving RepoConfig and
// config.Config itself.
func checkRepoConfig(repoDir string, w io.Writer) ([]string, error) {
	cfg, err := setup.ReadRepoConfig(repoDir)
	if err != nil {
		return nil, fmt.Errorf("read repo config: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	globalCfg, err := config.Load(home)
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	effectiveSpecTool := cfg.EffectiveSpecTool(globalCfg.SpecTool)
	switch {
	case cfg.SpecTool != "":
		fmt.Fprintf(w, "Spec tool: %s (repo override)\n", effectiveSpecTool)
	case globalCfg.SpecTool != "":
		fmt.Fprintf(w, "Spec tool: %s (global default)\n", effectiveSpecTool)
	default:
		fmt.Fprintln(w, "Spec tool: (none set — syntropy's own default spec flow)")
	}

	missing := setup.MissingFields(cfg)
	if len(missing) == 0 {
		fmt.Fprintf(w, "OK: %s has every recognised field configured\n", setup.RepoConfigPath(repoDir))
		return nil, nil
	}
	fmt.Fprintf(w, "MISSING: %s is missing %d field(s):\n", setup.RepoConfigPath(repoDir), len(missing))
	for _, f := range missing {
		fmt.Fprintf(w, "  - %s\n", f)
	}
	fmt.Fprintln(w, "Ask the user about each field above, then run `syntropy setup --title-convention \"<answer>\"` (or `... blank` if they decline) to persist it.")
	return missing, nil
}

// cmdConfig checks a repo's .syntropy.yml for missing fields, purely via
// code — no LLM invocation. ADR-0083: an agent following the syntropy
// Skill should run this before triggering a spec instead of reading and
// reasoning about the YAML file itself; only the fields this prints as
// missing are worth spending a conversational turn on.
//
// usage: syntropy config check [--repo <dir>]
//
// Exit code doubles as a cheap machine-readable signal alongside stdout:
// 0 means every recognised field is configured (real value or
// setup.BlankSentinel); 1 means at least one field is missing and needs
// asking about.
func cmdConfig(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("usage: syntropy config check [--repo <dir>]")
		return nil
	}
	switch args[0] {
	case "check":
		fs := flag.NewFlagSet("config check", flag.ExitOnError)
		repoFlag := fs.String("repo", "", "repo root to check (default: current directory)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		repoDir := *repoFlag
		if repoDir == "" {
			var err error
			repoDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("cwd: %w", err)
			}
		}
		missing, err := checkRepoConfig(repoDir, os.Stdout)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			os.Exit(1)
		}
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (try check)", args[0])
	}
}

// cmdSetup installs the Claude Code Skill bundle (ADR-0002) and persists the
// user's default runner + model + spec tool choice to
// ~/.syntropy/config.yaml (ADR-0051).
// Unlike the automatic first-run hook in main(), this doesn't require
// ~/.claude to already exist, and --force lets a user pull down the current
// SKILL.md over a locally-edited copy.
//
// Claude-only today by design (ADR-0002); a companion command for another
// coding agent's own integration format (Codex, Qwen, ...) would live
// alongside this one rather than be bolted onto it.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing Skill file or .syntropy.yml with the current/given value")
	runnerFlag := fs.String("runner", "", "default runner to persist (default: the only registered runner, \"claude\")")
	modelFlag := fs.String("model", "", "default model override to persist for the chosen runner (default: prompt if interactive, else leave unset)")
	specToolFlag := fs.String("spec-tool", "", "default spec tool to persist, e.g. \"spec-kit\" (default: prompt if interactive, else leave unset)")
	titleConventionFlag := fs.String("title-convention", "", "this repo's PR/MR title convention, written to .syntropy.yml (default: prompt if interactive, else leave unset)")
	repoSpecToolFlag := fs.String("repo-spec-tool", "", "this repo's spec tool override, written to .syntropy.yml (default: prompt if interactive, else leave unset; falls back to the global --spec-tool default)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	skillPath := setup.SkillPath(home)

	installed, err := setup.InstallClaudeSkill(home, *force)
	if err != nil {
		return fmt.Errorf("install Claude Code Skill: %w", err)
	}
	if installed {
		fmt.Printf("Installed the Claude Code Skill at %s\n", skillPath)
	} else {
		fmt.Printf("Claude Code Skill already installed at %s (pass --force to overwrite)\n", skillPath)
	}

	cfg, err := config.Load(home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	runnerName, err := setup.ResolveRunner(*runnerFlag)
	if err != nil {
		return fmt.Errorf("resolve runner: %w", err)
	}

	interactive := isatty.IsTerminal(os.Stdin.Fd())
	model, err := setup.ResolveModel(*modelFlag, cfg.Model, interactive, promptForModel(runnerName, os.Stdin, os.Stdout))
	if err != nil {
		return fmt.Errorf("resolve model: %w", err)
	}

	specTool, err := setup.ResolveSpecTool(*specToolFlag, cfg.SpecTool, interactive, promptForSpecTool(os.Stdin, os.Stdout))
	if err != nil {
		return fmt.Errorf("resolve spec tool: %w", err)
	}

	cfg.Runner = runnerName
	cfg.Model = model
	cfg.SpecTool = specTool
	if err := config.Save(home, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("Default runner: %s\n", runnerName)
	if model != "" {
		fmt.Printf("Default model: %s\n", model)
	} else {
		fmt.Printf("Default model: (none set — %s's own default; pass --model or rerun interactively to set one)\n", runnerName)
	}
	if specTool != "" {
		fmt.Printf("Default spec tool: %s\n", specTool)
	} else {
		fmt.Printf("Default spec tool: (none set — syntropy's own default spec flow; pass --spec-tool or rerun interactively to set one)\n")
	}
	fmt.Printf("Saved to %s\n", config.Path(home))

	repoDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	titleConvention, err := setup.ResolveTitleConvention(*titleConventionFlag, interactive, promptForTitleConvention(os.Stdin, os.Stdout))
	if err != nil {
		return fmt.Errorf("resolve title convention: %w", err)
	}
	repoSpecTool, err := setup.ResolveRepoSpecTool(*repoSpecToolFlag, interactive, promptForRepoSpecTool(os.Stdin, os.Stdout))
	if err != nil {
		return fmt.Errorf("resolve repo spec tool: %w", err)
	}
	wroteRepoConfig, err := setup.WriteRepoConfig(repoDir, titleConvention, repoSpecTool, *force)
	if err != nil {
		return fmt.Errorf("write .syntropy.yml: %w", err)
	}
	switch {
	case wroteRepoConfig:
		fmt.Printf("Wrote repo config to %s\n", setup.RepoConfigPath(repoDir))
	case titleConvention != "" || repoSpecTool != "":
		fmt.Printf(".syntropy.yml already exists at %s (pass --force to overwrite)\n", setup.RepoConfigPath(repoDir))
	}
	return nil
}

// promptForTitleConvention returns a setup.ResolveTitleConvention prompt
// func that asks the user for this repo's PR/MR title convention on r.
// Only called when stdin is a TTY and no --title-convention flag was given.
func promptForTitleConvention(r io.Reader, w io.Writer) func() (string, error) {
	return func() (string, error) {
		fmt.Fprint(w, "This repo's PR/MR title convention (blank = none): ")
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", nil
		}
		return strings.TrimSpace(scanner.Text()), nil
	}
}

// promptForRepoSpecTool returns a setup.ResolveRepoSpecTool prompt func
// that asks the user for this repo's spec tool override on r. Only called
// when stdin is a TTY and no --repo-spec-tool flag was given.
func promptForRepoSpecTool(r io.Reader, w io.Writer) func() (string, error) {
	return func() (string, error) {
		fmt.Fprint(w, "This repo's spec tool override, e.g. spec-kit (blank = use the global default): ")
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", nil
		}
		return strings.TrimSpace(scanner.Text()), nil
	}
}

// promptForModel returns a setup.ResolveModel prompt func that asks the user
// for a default model on r, showing existing as the value a blank answer
// keeps. Only called when stdin is a TTY and no --model flag was given.
func promptForModel(runnerName string, r io.Reader, w io.Writer) func(existing string) (string, error) {
	return func(existing string) (string, error) {
		if existing != "" {
			fmt.Fprintf(w, "Default model for %s (blank keeps %q): ", runnerName, existing)
		} else {
			fmt.Fprintf(w, "Default model for %s (blank = %s's own default): ", runnerName, runnerName)
		}
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", nil
		}
		return strings.TrimSpace(scanner.Text()), nil
	}
}

// promptForSpecTool returns a setup.ResolveSpecTool prompt func that asks
// the user for a default spec tool on r, showing existing as the value a
// blank answer keeps. Only called when stdin is a TTY and no --spec-tool
// flag was given.
func promptForSpecTool(r io.Reader, w io.Writer) func(existing string) (string, error) {
	return func(existing string) (string, error) {
		if existing != "" {
			fmt.Fprintf(w, "Default spec tool (blank keeps %q): ", existing)
		} else {
			fmt.Fprint(w, "Default spec tool, e.g. spec-kit (blank = syntropy's own default): ")
		}
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", nil
		}
		return strings.TrimSpace(scanner.Text()), nil
	}
}

func versionString() string {
	return fmt.Sprintf("syntropy %s (commit: %s, built: %s)", strings.TrimSpace(version), gitCommit, buildTime)
}

func daemonBannerLine() string {
	return fmt.Sprintf("syntropy daemon starting version=%s commit=%s pid=%d go=%s os=%s arch=%s",
		version, gitCommit, os.Getpid(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func cmdVersion(_ []string) error {
	fmt.Println(versionString())
	return nil
}
