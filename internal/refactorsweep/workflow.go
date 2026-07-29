// State machine wiring for the bulk-refactor sweep. See DESIGN.md § "The
// state machine" and ADR-0015.
//
// Step bodies are methods on Deps so they can access the provider registry,
// secret registry, public URL, and runs root via closure. Build() wires the
// graph; the daemon constructs a Deps once at startup and passes it in.
package refactorsweep

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/filter"
	"github.com/andrewwormald/syntropy/internal/git"
	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
	"github.com/andrewwormald/syntropy/internal/setup"
	"github.com/andrewwormald/syntropy/internal/webhook"
)

// Deps is the set of collaborators a built workflow needs. The store /
// streamer / scheduler / clock fields are passed to workflow.Build at the
// bottom; the rest are captured by the step closures.
type Deps struct {
	// luno/workflow plumbing
	RecordStore   workflow.RecordStore
	TimeoutStore  workflow.TimeoutStore
	EventStreamer workflow.EventStreamer
	RoleScheduler workflow.RoleScheduler

	// everflow-specific dependencies for the step bodies
	Providers     map[string]provider.Provider // by name; populated at daemon start
	Runners       *runner.Registry             // by name; agents (claude, qwen, openhands)
	Filter        filter.Filter                // Starlark filter; nil = StubFilter
	Git           git.Git                      // git CLI wrapper; required for work() + invokeForEvent
	Secrets       *webhook.SecretRegistry      // per-(provider, runID) HMAC/token secrets
	PublicBaseURL string                       // e.g. https://everflow.example.com
	RunsRoot      string                       // e.g. ~/.syntropy/runs
}

// stepErrBackOff is the delay the workflow library waits before retrying a
// step after it returns an error. This is a flat duration, not exponential —
// luno/workflow v0.5.0 doesn't offer exponential backoff. The default of 1s
// (see ADR-0062) is far too tight for steps that call external APIs
// (provider auth, the claude CLI): a genuinely broken step (expired
// credentials, a systematically-failing prompt) would otherwise hammer that
// dependency roughly once a second, indefinitely.
const stepErrBackOff = 45 * time.Second

// stepPauseAfterErrCount moves a Run to RunStatePaused after this many
// consecutive errors on a step, instead of retrying forever. This is the
// real circuit breaker (ADR-0062) — stepErrBackOff alone only slows the
// hammering down, it doesn't stop it. 6 consecutive failures at
// stepErrBackOff's cadence is ~4.5 minutes: enough to ride out a single
// transient blip (a dropped connection, one rate-limited request) without
// letting a systematically-broken step run unattended for tens of minutes.
const stepPauseAfterErrCount = 6

// Build wires the state machine described in DESIGN.md. Step bodies are
// closures over d so they have access to providers, secrets, etc.
func Build(name string, d Deps) *workflow.Workflow[AgentState, AgentStatus] {
	b := workflow.NewBuilder[AgentState, AgentStatus](name)

	stepOpts := []workflow.Option{
		workflow.ErrBackOff(stepErrBackOff),
		workflow.PauseAfterErrCount(stepPauseAfterErrCount),
	}

	b.AddStep(StatusInitiated, d.setup, StatusDiscovering, StatusFailed).
		WithOptions(stepOpts...)

	b.AddStep(StatusDiscovering, d.discover,
		StatusWorking,
		StatusCompleted,
		StatusPaused, // discoverSpec returns this on DecisionAsk (planner asks the author a clarifying question)
		StatusFailed, // discoverSpec returns this on planner errors / unexpected decisions
	).WithOptions(stepOpts...)

	b.AddStep(StatusWorking, d.work,
		StatusAwaitingMerge, // MR opened, await events
		StatusDiscovering,   // runner returned Done but worktree clean → blacklist + next unit
		StatusFailed,        // unrecoverable (runner err, MR create err, push err)
	).WithOptions(stepOpts...)
	// Note: StatusPaused not yet a destination from Working. Once an MR
	// exists in InFlight, the resume callback owns pause/retry/skip; work()
	// can only fail before that point.

	b.AddCallback(StatusAwaitingMerge, d.resume,
		StatusAwaitingMerge,
		StatusDiscovering,
		StatusPaused,
		StatusFailed,
		StatusCancelled,              // /syntropy stop
		StatusAwaitingAbandonConfirm, // /syntropy abandon (first tap)
	)

	b.AddCallback(StatusPaused, d.resume,
		StatusPaused,                 // stay paused (event filtered or non-control note arrived)
		StatusAwaitingMerge,
		StatusDiscovering,
		StatusFailed,
		StatusCancelled,              // /syntropy stop from paused
		StatusAwaitingAbandonConfirm, // /syntropy abandon (first tap) from paused
	)

	// AwaitingAbandonConfirm: only a second /syntropy abandon confirms;
	// anything else drops back to AwaitingMerge. Same resume() handler;
	// it dispatches based on r.Status.
	b.AddCallback(StatusAwaitingAbandonConfirm, d.resume,
		StatusAwaitingAbandonConfirm, // stay in confirm window for unrelated events
		StatusAwaitingMerge,          // any other activity = abandon cancelled
		StatusCancelled,              // confirmed
	)

	// 12-hour confirmation window. When the timer fires, drop back to
	// AwaitingMerge with a comment so the author sees the window closed.
	b.AddTimeout(StatusAwaitingAbandonConfirm,
		func(_ context.Context, _ *workflow.Run[AgentState, AgentStatus], now time.Time) (time.Time, error) {
			return now.Add(12 * time.Hour), nil
		},
		d.onAbandonConfirmTimeout,
		StatusAwaitingMerge,
	)

	// A step auto-paused by PauseAfterErrCount trips a Dead Letter Queue, but
	// it isn't necessarily a permanent failure — the library's default
	// pausedRecordsRetry behaviour (auto-resume after 1h) is left enabled
	// deliberately (see ADR-0062): onAutoPause fires and posts a fresh bot
	// comment on every pause, including re-pauses after a failed auto-retry,
	// so a genuinely broken step (e.g. an expired credential) still surfaces
	// repeatedly for a human to notice and intervene, while a merely
	// transient one (a rate-limited API call, a network blip) self-heals
	// without anyone needing to reply `/syntropy resume` by hand.
	b.OnPause(d.onAutoPause)

	return b.Build(
		d.EventStreamer,
		d.RecordStore,
		d.RoleScheduler,
		workflow.WithTimeoutStore(d.TimeoutStore),
	)
}

// onAutoPause fires, via the library's OnPause run-state-change hook, every
// time PauseAfterErrCount trips the circuit breaker on any step (ADR-0062) —
// including repeat trips after the library's own auto-retry-after-1h resumes
// a paused Run into the same failure. It posts a visible bot comment on
// every in-flight MR/PR (if any exist yet — a pause during setup/discover
// happens before one does) so a human sees why the Run stopped without
// having to poll `syntropy status`.
//
// The underlying step error itself isn't available here — the failing
// step's own AgentState mutations (e.g. LastError) are never persisted on
// the erroring attempt that trips the breaker, only record.Meta.RunStateReason
// (set by the library's own Pause() call) is guaranteed current. Point
// whoever reads this at the daemon log for the specific error rather than
// guessing at a message that may be stale.
func (d *Deps) onAutoPause(ctx context.Context, record *workflow.TypedRecord[AgentState, AgentStatus]) error {
	state := record.Object
	p, ok := d.Providers[state.ProviderName]
	if !ok || len(state.InFlight) == 0 {
		return nil
	}
	body := fmt.Sprintf(
		"⏸️ Auto-paused (run `%s`): %s\n\nThis will retry automatically in about an hour. If it's a real problem (not a transient blip), check the daemon log for the specific error and fix it before then — otherwise reply `/syntropy resume` any time to retry immediately.",
		record.RunID, record.Meta.RunStateReason,
	)
	for _, mr := range state.InFlight {
		if err := p.PostComment(ctx, mr.ProjectID, mr.IID, body); err != nil {
			return fmt.Errorf("onAutoPause: post comment for %s#%d: %w", mr.ProjectID, mr.IID, err)
		}
	}
	return nil
}

// setup runs once per Run when triggered. It:
//
//  1. Resolves the configured provider.
//  2. Captures the Author identity (whoever the token is for, unless
//     pre-set via the override at Trigger time).
//  3. Generates a per-Run webhook secret.
//  4. Registers a project-scoped webhook with the provider.
//  5. Stores webhook identity (ID, secret, URL) on AgentState so daemon
//     restart can resume + so teardown can deregister later.
//  6. Populates the in-memory SecretRegistry so the running daemon can
//     verify inbound webhooks.
//  7. Creates the per-Run filesystem layout (~/.syntropy/runs/<runID>/).
//  8. Defaults Concurrency and InFlight if not set.
//
// Idempotency: if WebhookID is already set, we assume a prior partial
// success and skip the registration step. Other side effects (filesystem
// dir, defaults) are themselves idempotent.
//
// On unrecoverable error, returns StatusFailed; the worktree dir is kept
// for inspection.
func (d *Deps) setup(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	p, ok := d.Providers[r.Object.ProviderName]
	if !ok {
		return StatusFailed, fmt.Errorf("setup: unknown provider %q (registered: %v)",
			r.Object.ProviderName, providerNames(d.Providers))
	}

	// Author capture, only if not pre-set via --author override at trigger time.
	if r.Object.Author.Handle == "" {
		user, err := p.AuthenticatedUser(ctx)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: authenticated user: %w", err)
		}
		r.Object.Author = user
	}

	// Poll-mode Runs don't register a webhook at all — the daemon's
	// poller loop ingests events. Skip the rest of webhook setup.
	if r.Object.IsPollMode() {
		// Initialise polling state maps so the poller can write into them.
		if r.Object.LastSeenNoteIDs == nil {
			r.Object.LastSeenNoteIDs = map[int]int64{}
		}
		if r.Object.LastMRStates == nil {
			r.Object.LastMRStates = map[int]string{}
		}
	} else // webhook mode: register on the provider as before
	// Webhook registration. Skip if already done (retry / restart).
	if r.Object.WebhookID == "" {
		secret, err := randomHex(32)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: random secret: %w", err)
		}
		callbackURL := fmt.Sprintf("%s/webhook/%s/%s", d.PublicBaseURL, p.Name(), r.RunID)
		kinds := []provider.EventKind{
			provider.EventNoteAdded,
			provider.EventMRMerged,
			provider.EventMRClosed,
			provider.EventMRUpdated,
			provider.EventPipelineSucceeded,
			provider.EventPipelineFailed,
		}
		webhookID, err := p.RegisterWebhook(ctx, r.Object.ProjectID, callbackURL, secret, kinds)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: register webhook: %w", err)
		}
		r.Object.WebhookID = webhookID
		r.Object.WebhookSecret = secret
		r.Object.WebhookURL = callbackURL
	}

	// Populate the in-process secret registry so inbound POSTs verify.
	// Safe to call on every setup invocation; the map is overwritten.
	if d.Secrets != nil {
		d.Secrets.Set(p.Name(), r.RunID, r.Object.WebhookSecret)
	}

	// Per-Run filesystem. MkdirAll is idempotent.
	runDir := filepath.Join(d.RunsRoot, r.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return StatusFailed, fmt.Errorf("setup: mkdir run dir: %w", err)
	}

	// Default filter file. If the user supplied a custom .star, FilterPath
	// is already set; otherwise write the default + record the path.
	if r.Object.FilterPath == "" {
		filterPath := filepath.Join(runDir, "note_added.star")
		if _, err := os.Stat(filterPath); os.IsNotExist(err) {
			if err := os.WriteFile(filterPath, filter.DefaultStarlark(), 0o644); err != nil {
				return StatusFailed, fmt.Errorf("setup: write default filter: %w", err)
			}
		}
		r.Object.FilterPath = filterPath
	}

	// Empty phrases file — append-only via Learnings.AddPhrases. Created
	// here so `Add` doesn't have to MkdirAll on its first call.
	phrasesPath := filepath.Join(runDir, "phrases.yaml")
	if _, err := os.Stat(phrasesPath); os.IsNotExist(err) {
		if err := os.WriteFile(phrasesPath, []byte("version: 1\nphrases: []\n"), 0o644); err != nil {
			return StatusFailed, fmt.Errorf("setup: write phrases.yaml: %w", err)
		}
	}

	// Defaults.
	if r.Object.Concurrency <= 0 {
		r.Object.Concurrency = 1
	}
	if r.Object.InFlight == nil {
		r.Object.InFlight = map[string]provider.MR{}
	}

	// Read BaseRepo's .syntropy.yml once, gated on StartedAt so a retry or
	// daemon restart replaying this step doesn't re-read it: an author
	// editing the file mid-Run shouldn't retroactively change the
	// convention already recorded for in-flight work (ADR-0052). Threaded
	// into MR-title generation via the runner's Title suggestion — see
	// work() below and ADR-0054.
	if r.Object.StartedAt.IsZero() && r.Object.BaseRepo != "" {
		cfg, err := setup.ReadRepoConfig(r.Object.BaseRepo)
		if err != nil {
			return StatusFailed, fmt.Errorf("setup: read repo config: %w", err)
		}
		// EffectiveTitleConvention, not the raw field: a user who was
		// asked and deliberately left this blank (setup.BlankSentinel)
		// must not have that sentinel leak into the runner's prompt as
		// if it were a real convention (ADR-00xx).
		r.Object.TitleConvention = cfg.EffectiveTitleConvention()
	}

	// Record when this Run first entered Discovering so MaxRuntime can be
	// checked on every subsequent discover() call.
	if r.Object.StartedAt.IsZero() {
		r.Object.StartedAt = time.Now()
	}

	return StatusDiscovering, nil
}

// checkBudget returns a non-empty pause reason if any budget limit is
// exceeded, or empty string if the Run may proceed. Called at the top of
// discover() so the Run pauses rather than spending more runner time once
// a cap is hit. See ADR-0036.
func checkBudget(s *AgentState, now time.Time) string {
	b := s.Budget
	if b.MaxUnits > 0 {
		done := len(s.Completed) + len(s.Blacklisted)
		if done >= b.MaxUnits {
			return fmt.Sprintf("budget: MaxUnits (%d) reached — %d completed + blacklisted", b.MaxUnits, done)
		}
	}
	if b.MaxTokens > 0 && s.TotalTokens >= b.MaxTokens {
		return fmt.Sprintf("budget: MaxTokens (%d) reached — %d tokens used", b.MaxTokens, s.TotalTokens)
	}
	if b.MaxRuntime > 0 && !s.StartedAt.IsZero() && now.Sub(s.StartedAt) >= b.MaxRuntime {
		return fmt.Sprintf("budget: MaxRuntime (%s) reached — run started %s ago", b.MaxRuntime, now.Sub(s.StartedAt).Round(time.Second))
	}
	return ""
}

// discover picks the next unit to work on. Branches on Mode:
//
//   sweep: pop from the static Queue, dedup against Completed + Blacklisted
//          + InFlight. Terminal when queue + in-flight are both empty.
//
//   spec:  invoke the runner with a planning prompt (spec body + plan
//          history); the runner returns either the next increment to do
//          or a Done signal. See ADR-0025.
//
// Empty Mode is treated as sweep for backwards compatibility with v1 Runs
// triggered before the Mode field existed.
func (d *Deps) discover(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if reason := checkBudget(r.Object, time.Now()); reason != "" {
		r.Object.PauseReason = reason
		return StatusPaused, nil
	}
	if r.Object.IsSpecMode() {
		return d.discoverSpec(ctx, r)
	}
	return d.discoverSweep(ctx, r)
}

// discoverSweep is the mechanical-sweep path: queue.Pop with dedup. Unchanged
// from v1.
func (d *Deps) discoverSweep(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	// Build the set of units we no longer need to consider.
	seen := make(map[string]struct{}, len(r.Object.Completed)+len(r.Object.Blacklisted)+len(r.Object.InFlight))
	for _, c := range r.Object.Completed {
		seen[c.UnitID] = struct{}{}
	}
	for _, b := range r.Object.Blacklisted {
		seen[b.UnitID] = struct{}{}
	}
	for unitID := range r.Object.InFlight {
		seen[unitID] = struct{}{}
	}

	// Drop already-processed entries from the head of the queue.
	for len(r.Object.Queue) > 0 {
		if _, dup := seen[r.Object.Queue[0]]; !dup {
			break
		}
		r.Object.Queue = r.Object.Queue[1:]
	}

	// Terminal condition: nothing queued, nothing in-flight → done.
	if len(r.Object.Queue) == 0 && len(r.Object.InFlight) == 0 {
		return StatusCompleted, nil
	}

	// If nothing's queued but units are in-flight, we'd normally wait. In
	// v1 (concurrency=1) this state is unreachable: a unit going to
	// AwaitingMerge stays there until merged/closed, and only then do we
	// re-enter discover. Defensive return when concurrency > 1 lands.
	if len(r.Object.Queue) == 0 {
		return StatusCompleted, nil
	}

	r.Object.CurrentUnit = r.Object.Queue[0]
	r.Object.Queue = r.Object.Queue[1:]
	return StatusWorking, nil
}

// discoverSpec asks the runner to plan the next increment. The runner
// receives the spec body, the history of merged/blacklisted increments,
// and any pending /syntropy prompt injection. It returns:
//
//   DecisionContinue → there's more work; we generate a unitID
//                      (increment-N), append to Plan, set CurrentUnit
//   DecisionDone     → the spec is fully implemented → StatusCompleted
//   DecisionAsk      → the planner needs human input → StatusPaused
//   DecisionFail     → planning failed unrecoverably → StatusFailed
//   DecisionNoChange → equivalent to Continue with no work right now;
//                      we treat as Completed (planner found nothing actionable)
//
// We don't validate the runner's *output* against the codebase here; that
// happens implicitly in work() (the runner does the change, git checks
// HasChanges, the gate proves it's real).
func (d *Deps) discoverSpec(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if d.Runners == nil {
		return StatusFailed, fmt.Errorf("discover: no Runners configured (spec mode requires a planner)")
	}
	rn, err := d.Runners.Get(r.Object.RunnerName)
	if err != nil {
		return StatusFailed, fmt.Errorf("discover: runner: %w", err)
	}

	// Planning worktree: a stable per-Run checkout the planner can inspect
	// for codebase state. EnsureBranch creates it on first call; HardReset
	// refreshes it to origin/<base> before every subsequent planning call
	// so the planner always sees fresh state. Local commits the planner
	// might leave behind are wiped — planning is read-only by convention.
	planningDir := filepath.Join(d.RunsRoot, r.RunID, "planning")
	baseBranch := defaultIfEmpty(r.Object.BaseBranch, "main")
	planBranch := "syntropy/plan/" + shortRunID(r.RunID)
	if d.Git != nil && r.Object.BaseRepo != "" {
		if err := d.Git.EnsureBranch(ctx, planningDir, r.Object.BaseRepo, baseBranch, planBranch); err != nil {
			return StatusFailed, fmt.Errorf("discover: planning worktree setup: %w", err)
		}
		if err := d.Git.HardReset(ctx, planningDir, baseBranch); err != nil {
			return StatusFailed, fmt.Errorf("discover: refresh planning worktree: %w", err)
		}
	}

	// Build the planning prompt: spec body + plan history. The prompt
	// injection (if any) takes priority — it represents the author's
	// latest steering.
	goal := buildPlanningPrompt(r.Object)
	if r.Object.PromptInjection != "" {
		goal = r.Object.PromptInjection + "\n\n---\n\n" + goal
		r.Object.PromptInjection = "" // consume single-use
	}

	req := runner.Request{
		Worktree:     planningDir,
		SkillCommand: "/syntropy-plan",
		Goal:         goal,
		UnitID:       "", // planning is not unit-scoped
		Model:        r.Object.RunnerModel,
		Budget:       r.Object.Budget,
	}

	if d.Git != nil {
		if err := d.verifyIsolatedWorktree(ctx, planningDir); err != nil {
			return StatusFailed, fmt.Errorf("discover: %w", err)
		}
	}

	resp, runErr := rn.Run(ctx, req)
	turn := Turn{
		Index:     len(r.Object.History),
		UnitID:    "", // planning turns have no unit
		Runner:    rn.Name(),
		Phase:     PhasePlan,
		Summary:   resp.Summary,
		Tokens:    resp.Tokens,
		StartedAt: orNow(resp.StartedAt),
		EndedAt:   orNow(resp.EndedAt),
	}
	if runErr != nil {
		turn.Error = runErr.Error()
	}
	r.Object.History = append(r.Object.History, turn)
	r.Object.SubagentInvocations++
	r.Object.TotalTokens += resp.Tokens

	if runErr != nil {
		r.Object.LastError = runErr.Error()
		return StatusFailed, fmt.Errorf("discover: planner: %w", runErr)
	}

	switch resp.Decision {
	case DecisionDone, DecisionNoChange:
		// Planner says we're done (Done) or there's nothing actionable
		// right now (NoChange). Either way: terminate the Run.
		return StatusCompleted, nil

	case DecisionAsk:
		// Planner needs the author's input. Park; the answer will arrive
		// via comment on the most recent MR (or a new comment thread).
		r.Object.PauseReason = "planner asks: " + resp.Question
		return StatusPaused, nil

	case DecisionFail:
		r.Object.LastError = "planner failed: " + resp.Summary
		return StatusFailed, nil

	case DecisionContinue:
		// New increment to do. Generate a stable unit ID and record the
		// rationale.
		unitID := fmt.Sprintf("increment-%d", len(r.Object.Plan)+1)
		r.Object.Plan = append(r.Object.Plan, PlannedIncrement{
			UnitID:    unitID,
			Rationale: resp.Summary,
			PlannedAt: time.Now(),
			Outcome:   "in_flight",
		})
		r.Object.CurrentUnit = unitID
		return StatusWorking, nil

	default:
		r.Object.LastError = fmt.Sprintf("planner returned unexpected decision %q", resp.Decision)
		return StatusFailed, nil
	}
}

// buildPlanningPrompt assembles the input the planner sees each iteration:
// the spec body + history of plan entries (what was decided, what
// happened). Short by design — bounded context — even after many merges.
func buildPlanningPrompt(s *AgentState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Goal\n\n%s\n\n", s.Goal)
	if s.SpecBody != "" {
		fmt.Fprintf(&b, "# Spec\n\n%s\n\n", s.SpecBody)
	}
	if len(s.Plan) > 0 {
		fmt.Fprintf(&b, "# Plan history (last %d entries)\n\n", len(s.Plan))
		for _, p := range s.Plan {
			outcome := p.Outcome
			if outcome == "" {
				outcome = "(pending)"
			}
			fmt.Fprintf(&b, "- **%s** [%s]: %s\n", p.UnitID, outcome, p.Rationale)
			if p.RemainderNote != "" {
				fmt.Fprintf(&b, "  - %s shipped a partial slice; remaining work: %s\n", p.UnitID, p.RemainderNote)
			}
		}
		fmt.Fprintln(&b)
	}
	if len(s.Completed) > 0 || len(s.Blacklisted) > 0 {
		fmt.Fprintf(&b, "# Merged so far: %d. Blacklisted: %d.\n\n",
			len(s.Completed), len(s.Blacklisted))
	}
	b.WriteString(`# Your task

Decide the next increment toward implementing this spec.

Return Decision=Continue with a one-line rationale (Summary) describing the
next increment, OR Decision=Done if the spec is fully implemented, OR
Decision=Ask with a Question if you need the author's input before deciding,
OR Decision=Fail if planning is impossible.
`)
	return b.String()
}

// updatePlanOutcome marks a planned increment's outcome (completed/
// blacklisted). Called from markUnitMerged / markUnitBlacklisted so the
// planner sees fresh outcomes on the next iteration.
func updatePlanOutcome(s *AgentState, unitID, outcome string) {
	for i := range s.Plan {
		if s.Plan[i].UnitID == unitID {
			s.Plan[i].Outcome = outcome
			return
		}
	}
}

// updatePlanRemainder records what's left of a unit that shipped a partial
// MR (DecisionContinue in the work phase) rather than the whole unit, so
// the planner sees it on the next iteration. No-op if there's no matching
// Plan entry (e.g. tests that set CurrentUnit directly without going
// through discoverSpec).
func updatePlanRemainder(s *AgentState, unitID, note string) {
	for i := range s.Plan {
		if s.Plan[i].UnitID == unitID {
			s.Plan[i].RemainderNote = note
			return
		}
	}
}

// planRationaleFor returns the planner's per-increment rationale for
// unitID from AgentState.Plan, or "" if there isn't one. When Plan
// contains multiple entries with the same UnitID (e.g. re-plan after
// blacklist), the latest wins — the planner's freshest thinking about
// this increment is the most useful scope guidance for the runner.
//
// This is the load-bearing piece of the scope-narrowing fix: without
// it, work() hands the runner the top-level spec Goal (often a
// multi-item shopping list) with no signal about which item(s) belong
// to THIS increment. The runner then over-scopes — the failure mode
// that produced the mega-PR #5 during Run b21a0cc6.
func planRationaleFor(s *AgentState, unitID string) string {
	for i := len(s.Plan) - 1; i >= 0; i-- {
		if s.Plan[i].UnitID == unitID {
			return s.Plan[i].Rationale
		}
	}
	return ""
}

// applyIncrementScope prepends the planner's rationale for the current
// increment to the runner Request's Goal so the runner sees per-increment
// scope directly rather than having to infer it from a unit-id string
// and a whole-spec Goal. No-op when no plan entry exists for the unit
// (edge case: legacy Runs pre-dating this fix). Order relative to
// PromptInjection: rationale goes UNDER the injection so the user's
// single-use override remains the highest-priority signal.
func applyIncrementScope(req *runner.Request, s *AgentState, unitID string) {
	rationale := planRationaleFor(s, unitID)
	if rationale == "" {
		return
	}
	req.Goal = "## Scope for this increment (from the planner)\n\n" +
		rationale +
		"\n\n---\n\n## Full spec goal (context)\n\n" +
		req.Goal
}

// work invokes the runner against the current unit and, on success,
// opens an MR via the provider.
//
// Full flow:
//   1. Resolve runner + provider
//   2. Git: EnsureBranch creates the worktree on a per-unit branch off
//      origin/<BaseBranch>
//   3. Invoke runner; runner writes files inside the worktree
//   4. Git: HasWorkBeyondBase. If no work (clean tree AND no commits
//      beyond origin/<BaseBranch>), runner returned Done without doing
//      anything → blacklist the unit ("no changes needed") and back to
//      Discovering
//   5. Git: Commit + Push the new branch
//   6. Provider: CreateMR; store in InFlight; post initial comment
//   7. → StatusAwaitingMerge
//
// Any error before MR creation → StatusFailed. The Run terminates; the
// user starts a new one. (A future ADR may add a pre-MR pause+retry path;
// for v1 we keep work() linear.)
func (d *Deps) work(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
	if r.Object.CurrentUnit == "" {
		return StatusFailed, fmt.Errorf("work: no CurrentUnit set")
	}
	p, ok := d.Providers[r.Object.ProviderName]
	if !ok {
		return StatusFailed, fmt.Errorf("work: unknown provider %q", r.Object.ProviderName)
	}
	if d.Runners == nil {
		return StatusFailed, fmt.Errorf("work: no Runners configured")
	}
	if d.Git == nil {
		return StatusFailed, fmt.Errorf("work: no Git configured")
	}
	rn, err := d.Runners.Get(r.Object.RunnerName)
	if err != nil {
		return StatusFailed, fmt.Errorf("work: runner: %w", err)
	}

	unitID := r.Object.CurrentUnit
	branch := branchName(r.RunID, unitID)
	worktree := filepath.Join(d.RunsRoot, r.RunID, "worktrees", unitID)
	baseBranch := defaultIfEmpty(r.Object.BaseBranch, "main")

	// 1. Set up worktree off origin/<base>.
	if err := d.Git.EnsureBranch(ctx, worktree, r.Object.BaseRepo, baseBranch, branch); err != nil {
		r.Object.LastError = fmt.Sprintf("work: git EnsureBranch: %v", err)
		return StatusFailed, nil
	}

	// 2. Invoke runner inside the worktree.
	req := runner.Request{
		Worktree:        worktree,
		SkillCommand:    fmt.Sprintf("/syntropy-unit %s", unitID), // overridden once SkillPath integration lands
		Goal:            r.Object.Goal,
		UnitID:          unitID,
		Model:           r.Object.RunnerModel,
		Budget:          r.Object.Budget,
		TitleConvention: r.Object.TitleConvention,
	}
	// Prepend the planner's per-increment rationale so the runner sees
	// this increment's scope directly, not just the whole-spec Goal. See
	// planRationaleFor + applyIncrementScope for the reasoning.
	applyIncrementScope(&req, r.Object, unitID)
	if r.Object.PromptInjection != "" {
		req.Goal = r.Object.PromptInjection + "\n\n---\n\n" + req.Goal
		r.Object.PromptInjection = "" // consume single-use
	}

	if err := d.verifyIsolatedWorktree(ctx, worktree); err != nil {
		r.Object.LastError = fmt.Sprintf("work: %v", err)
		return StatusFailed, nil
	}

	resp, runErr := rn.Run(ctx, req)
	turn := Turn{
		Index:     len(r.Object.History),
		UnitID:    unitID,
		Runner:    rn.Name(),
		Phase:     PhaseWork,
		Summary:   resp.Summary,
		Tokens:    resp.Tokens,
		StartedAt: orNow(resp.StartedAt),
		EndedAt:   orNow(resp.EndedAt),
	}
	if runErr != nil {
		turn.Error = runErr.Error()
	}
	r.Object.History = append(r.Object.History, turn)
	r.Object.SubagentInvocations++
	r.Object.TotalTokens += resp.Tokens

	if runErr != nil {
		r.Object.LastError = fmt.Sprintf("work: runner.Run: %v", runErr)
		return StatusFailed, nil
	}

	switch resp.Decision {
	case DecisionDone, DecisionContinue:
		// DecisionContinue is documented as planner-only ("there's more
		// to do; pick another increment"), but Claude sometimes returns
		// it from a work turn — semantically "I did some work but this
		// unit was bigger than one turn; there's a remainder". Treat it
		// like DecisionDone for shipping purposes: ship what's in the
		// worktree if dirty; blacklist if clean. The difference from Done
		// is that we record the runner's own account of what's left onto
		// the Plan entry (RemainderNote) instead of silently treating the
		// partial diff as if the unit were complete — the planner reads
		// this on its next iteration and can schedule a follow-on
		// increment rather than assuming the unit is done.
		//
		// Regressed on Run b723ebc4 (2026-07-03) when the runner returned
		// Continue in the work phase for increment-2, and work()'s catch-
		// all default routed it to StatusFailed — same shape as the
		// DecisionNoChange bug fixed in b6926b9.
		if resp.Decision == DecisionContinue {
			updatePlanRemainder(r.Object, unitID, resp.Summary)
		}

		// 3. Did the runner actually change anything? HasWorkBeyondBase,
		// not HasChanges: a runner that commits its own work leaves a
		// clean tree, and a porcelain-only check would discard that work
		// as "no changes" (see the Git interface doc for the four cases).
		hasWork, err := d.Git.HasWorkBeyondBase(ctx, worktree, baseBranch)
		if err != nil {
			r.Object.LastError = fmt.Sprintf("work: git HasWorkBeyondBase: %v", err)
			return StatusFailed, nil
		}
		if !hasWork {
			// Runner claims Done but didn't touch anything. Treat as a
			// no-op: blacklist with reason, cleanup, move to next unit.
			r.Object.Blacklisted = append(r.Object.Blacklisted, BlacklistedUnit{
				UnitID: unitID, Reason: "runner returned Done with no changes",
				At: time.Now(),
			})
			updatePlanOutcome(r.Object, unitID, "blacklisted")
			r.Object.CurrentUnit = ""
			_ = d.Git.RemoveWorktree(ctx, r.Object.BaseRepo, worktree) // best-effort
			return StatusDiscovering, nil
		}

		// 4. Commit + push.
		// StatusFailed must be terminal (see AgentStatus doc on
		// StatusFailed: "unrecoverable; worktree kept for forensics").
		// Returning (StatusFailed, err) makes the workflow library treat
		// the err as transient and retry the step forever — that's how the
		// spike on 2026-06-24 ended up looping every ~1s on a pre-commit
		// hook failure. Capture err in LastError, then return nil so the
		// state actually commits.
		// Keep the subject short — many shops cap commit message subjects
		// at 72/80 chars via a commit-msg hook. The full Goal goes in the
		// body so reviewers still see it without scanning the MR description.
		commitMsg := fmt.Sprintf("syntropy %s: %s\n\n%s\n\nGenerated by syntropy run %s.\n",
			shortRunID(r.RunID), unitID, r.Object.Goal, shortRunID(r.RunID))
		if err := d.Git.Commit(ctx, worktree, commitMsg); err != nil {
			if !errors.Is(err, git.ErrNoChanges) {
				r.Object.LastError = fmt.Sprintf("work: git Commit: %v", err)
				return StatusFailed, nil
			}
			// ErrNoChanges: nothing stageable. Two ways here: the runner
			// committed its own work (clean tree — the commits are what
			// HasWorkBeyondBase saw), or it produced only filtered content
			// (e.g. nothing but binary blobs). Committed changes beyond
			// base are real work — push them; otherwise treat the same as
			// "Done with no changes" — blacklist and move on rather than
			// terminate the Run.
			stat, sErr := d.Git.DiffShortstat(ctx, worktree, baseBranch)
			if sErr != nil {
				r.Object.LastError = fmt.Sprintf("work: git DiffShortstat: %v", sErr)
				return StatusFailed, nil
			}
			if stat == "" {
				r.Object.Blacklisted = append(r.Object.Blacklisted, BlacklistedUnit{
					UnitID: unitID, Reason: "commit produced no stageable changes",
					At: time.Now(),
				})
				updatePlanOutcome(r.Object, unitID, "blacklisted")
				r.Object.CurrentUnit = ""
				_ = d.Git.RemoveWorktree(ctx, r.Object.BaseRepo, worktree)
				return StatusDiscovering, nil
			}
			// Self-committed work: fall through to Push.
		}
		if err := d.Git.Push(ctx, worktree, branch); err != nil {
			r.Object.LastError = fmt.Sprintf("work: git Push: %v", err)
			return StatusFailed, nil
		}

		// 5. Open the MR. Title: prefer the runner's suggestion, phrased per
		// BaseRepo's .syntropy.yml title_convention (ADR-0052/ADR-0054); fall
		// back to the default shape when no convention is set or the runner
		// didn't include one.
		title := resp.Title
		if title == "" {
			title = fmt.Sprintf("%s: %s", r.Object.Goal, unitID)
		}
		mr, err := p.CreateMR(ctx, r.Object.ProjectID, provider.MRDraft{
			Branch:       branch,
			TargetBranch: baseBranch,
			Title:        title,
			Description:  resp.Summary,
			Labels:       []string{"syntropy", "syntropy:" + shortRunID(r.RunID)},
			Draft:        r.Object.DraftMRs,
		})
		if err != nil {
			r.Object.LastError = fmt.Sprintf("work: CreateMR: %v", err)
			return StatusFailed, nil
		}
		r.Object.InFlight[unitID] = mr

		// Apply any title/description fixes the runner requested this same
		// turn (ADR-0091), beyond what CreateMR already set from
		// resp.Title/resp.Summary above.
		applyMetadataUpdates(ctx, r, p, r.Object.ProjectID, mr.IID, resp)

		// 6. Initial status comment so the author can see who's driving.
		// Append the actual diff shortstat as a cheap hallucination guard:
		// the reviewer sees both the runner's summary and the real extent of
		// changes pushed. See spec item 4 (Approach A).
		body := fmt.Sprintf("🤖 Opened by syntropy run `%s` (unit `%s`). I'll babysit this MR through review and CI — reply `/syntropy status` for progress, or `/syntropy skip` to abandon.",
			shortRunID(r.RunID), unitID)
		if d.Git != nil {
			if stat, sErr := d.Git.DiffShortstat(ctx, worktree, baseBranch); sErr == nil && stat != "" {
				body += "\n\nDiff: " + stat
			}
		}
		if err := postBotComment(ctx, r, p, r.Object.ProjectID, mr.IID, body); err != nil {
			// Comment failure is non-fatal — the MR exists, the run continues.
			r.Object.LastError = fmt.Sprintf("post initial comment: %v", err)
		}

		return StatusAwaitingMerge, nil

	case DecisionFail:
		r.Object.LastError = fmt.Sprintf("runner declined unit %q: %s", unitID, resp.Summary)
		return StatusFailed, nil

	case DecisionNoChange:
		// The runner evaluated and decided this unit needs no change
		// (e.g. planner's reasoning was stale, or the prior increment
		// already covered it). Semantically the same as Done+!dirty:
		// blacklist with a clear reason, drop the worktree, and let
		// the planner re-evaluate. If the planner keeps picking the
		// same shape of "done" unit, the blacklist accumulates and
		// discoverSpec eventually returns no-more-units → Completed.
		r.Object.Blacklisted = append(r.Object.Blacklisted, BlacklistedUnit{
			UnitID: unitID,
			Reason: fmt.Sprintf("runner returned NoChange: %s", resp.Summary),
			At:     time.Now(),
		})
		updatePlanOutcome(r.Object, unitID, "blacklisted")
		r.Object.CurrentUnit = ""
		_ = d.Git.RemoveWorktree(ctx, r.Object.BaseRepo, worktree)
		return StatusDiscovering, nil

	default:
		// Only Ask is genuinely unexpected here: it's the planner's way
		// of asking a clarifying question, but by the time we're in work
		// the planner has already picked the unit — a runner Ask would
		// mean the runner is confused about what to do rather than what
		// to plan. Done/Continue/Fail/NoChange are all handled above.
		r.Object.LastError = fmt.Sprintf("unexpected decision %q in work phase for unit %q",
			resp.Decision, unitID)
		return StatusFailed, nil
	}
}

// recentOutgoingHashCap bounds AgentState.RecentOutgoingHashes. See the
// field's doc comment on AgentState for the reasoning.
const recentOutgoingHashCap = 32

// hashBody returns a SHA-256 hex digest of a comment body. Deterministic
// and stable — the same body always produces the same hash, so a comment
// the daemon posts and the poller reads back will collide.
func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// applyMetadataUpdates pushes the runner's optional TitleUpdate/
// DescriptionUpdate (ADR-0091) to the MR. The runner can emit these tags on
// any turn to fix stale/wrong MR metadata itself instead of asking a human
// to hand-edit it; applying them is the harness's job, never the runner's
// own (ADR-0073's "harness owns every push" precedent). Best-effort: a
// failure here is recorded but never fails the turn, since any code change
// this turn already landed independently of the metadata update.
func applyMetadataUpdates(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], p provider.Provider, projectID string, mrIID int, resp runner.Response) {
	if resp.TitleUpdate != "" {
		if err := p.UpdateMRTitle(ctx, projectID, mrIID, resp.TitleUpdate); err != nil {
			r.Object.LastError = fmt.Sprintf("apply title update: %v", err)
		}
	}
	if resp.DescriptionUpdate != "" {
		if err := p.UpdateMRDescription(ctx, projectID, mrIID, resp.DescriptionUpdate); err != nil {
			r.Object.LastError = fmt.Sprintf("apply description update: %v", err)
		}
	}
}

// postBotComment posts a top-level comment on the provider AND records its
// hash on the Run's RecentOutgoingHashes ring so the next poll tick can
// silently drop the echo. Hash is stored BEFORE the network call so no race
// can let the poll see the comment before the state update lands.
//
// All daemon-originating top-level comments in refactorsweep must go
// through this helper rather than calling p.PostComment directly, or the
// self-comment loop reintroduces itself. When replying to a specific
// reviewer comment, use postBotReply instead so the response lands inline
// in that comment's thread.
func postBotComment(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], p provider.Provider, projectID string, mrIID int, body string) error {
	return postBotReply(ctx, r, p, projectID, mrIID, "", body)
}

// postBotReply is postBotComment's discussion-aware counterpart: when
// discussionID is non-empty (i.e. the triggering event was a reply within an
// existing review thread — see Note.DiscussionID), it replies inline via
// ReplyToDiscussion instead of posting a new top-level MR comment. An empty
// discussionID falls back to PostComment's original top-level behaviour.
func postBotReply(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], p provider.Provider, projectID string, mrIID int, discussionID string, body string) error {
	h := hashBody(body)
	r.Object.RecentOutgoingHashes = append(r.Object.RecentOutgoingHashes, h)
	if n := len(r.Object.RecentOutgoingHashes); n > recentOutgoingHashCap {
		r.Object.RecentOutgoingHashes = r.Object.RecentOutgoingHashes[n-recentOutgoingHashCap:]
	}
	if discussionID != "" {
		return p.ReplyToDiscussion(ctx, projectID, mrIID, discussionID, body)
	}
	return p.PostComment(ctx, projectID, mrIID, body)
}

// isOwnEcho reports whether an inbound note is one the daemon itself
// posted recently. Called at the top of resume() to short-circuit the
// self-comment loop before any filter, control-verb parsing, or runner
// invocation runs.
func isOwnEcho(r *workflow.Run[AgentState, AgentStatus], body string) bool {
	if len(r.Object.RecentOutgoingHashes) == 0 {
		return false
	}
	h := hashBody(body)
	for _, seen := range r.Object.RecentOutgoingHashes {
		if seen == h {
			return true
		}
	}
	return false
}

// resume handles webhook callbacks. The payload is a JSON-encoded
// provider.Event marshalled by the daemon's webhook dispatcher.
//
// Flow:
//  1. Decode the event; bump EventsSeen; tag IsAuthor.
//  2. Detect /syntropy control commands and route them (TODO: real
//     parsing in the next commit). When paused, only control commands
//     and MR lifecycle truth (merged/closed) have any effect; everything
//     else stays paused.
//  3. Look up which in-flight unit this event is for. Events for MRs we
//     don't track (cross-talk via the shared project webhook) are dropped.
//  4. Lifecycle events (MRMerged/MRClosed) bypass the filter — they
//     unconditionally move the unit out of InFlight and return to
//     Discovering so the next unit can be picked up.
//  5. Informational events (MRUpdated, PipelineSucceeded) are no-ops.
//  6. NoteAdded and PipelineFailed go through the filter. The cheap-filter
//     decides SKIP / INVOKE_SUBAGENT / PAUSE; subagent invocations are
//     handled by invokeForEvent.
func (d *Deps) resume(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], payload io.Reader) (AgentStatus, error) {
	var ev provider.Event
	if err := json.NewDecoder(payload).Decode(&ev); err != nil {
		return r.Status, fmt.Errorf("resume: decode event: %w", err)
	}
	r.Object.EventsSeen++
	ev.IsAuthor = isFromAuthor(ev.Author, r.Object.Author)

	// Polling watermarks — keep monotonic so the next poll won't re-fire
	// already-handled events. Safe to update unconditionally; webhook
	// events use the same fields with the same semantics.
	//
	// This runs BEFORE the echo-skip below so an echoed note still
	// advances LastSeenNoteIDs — otherwise the poller would re-fetch
	// and re-dispatch the same note every 30s (skipped correctly each
	// time, but inflating events_seen and generating log noise). This
	// was ADR-0035's tail bug caught by the b21a0cc6 dogfood Run.
	switch ev.Kind {
	case provider.EventNoteAdded:
		if r.Object.LastSeenNoteIDs == nil {
			r.Object.LastSeenNoteIDs = map[int]int64{}
		}
		if ev.Note.ID > r.Object.LastSeenNoteIDs[ev.MR.IID] {
			r.Object.LastSeenNoteIDs[ev.MR.IID] = ev.Note.ID
		}
		// Per-stream cursor (ADR-0041) — see provider.NoteCursor. Note.Stream
		// is empty for events synthesised before this field existed (or from
		// a provider that hasn't been updated); skip rather than bucket
		// those under a bogus "" stream key.
		if ev.Note.Stream != "" {
			if r.Object.LastSeenNoteIDsByStream == nil {
				r.Object.LastSeenNoteIDsByStream = map[int]map[string]int64{}
			}
			if r.Object.LastSeenNoteIDsByStream[ev.MR.IID] == nil {
				r.Object.LastSeenNoteIDsByStream[ev.MR.IID] = map[string]int64{}
			}
			if ev.Note.ID > r.Object.LastSeenNoteIDsByStream[ev.MR.IID][ev.Note.Stream] {
				r.Object.LastSeenNoteIDsByStream[ev.MR.IID][ev.Note.Stream] = ev.Note.ID
			}
		}
	case provider.EventMRMerged:
		if r.Object.LastMRStates == nil {
			r.Object.LastMRStates = map[int]string{}
		}
		r.Object.LastMRStates[ev.MR.IID] = "merged"
	case provider.EventMRClosed:
		if r.Object.LastMRStates == nil {
			r.Object.LastMRStates = map[int]string{}
		}
		r.Object.LastMRStates[ev.MR.IID] = "closed"
	}

	// Self-comment echo suppression. The daemon posts several comments
	// per Run (initial "🤖 Opened", "✓ Addressed", "📝 Recorded prompt",
	// info replies) via the user's OAuth identity, which means the
	// poller can't distinguish those from real user comments by author
	// alone. Every echoed comment used to trigger another claude -p
	// call, doubling+ runner spend per Run. We hash outgoing comments
	// into RecentOutgoingHashes and drop the echo on ingress.
	if ev.Kind == provider.EventNoteAdded && isOwnEcho(r, ev.Note.Body) {
		r.Object.EventsSkippedByFilter++
		return r.Status, nil
	}

	// AwaitingAbandonConfirm has the most restrictive semantics: only a
	// second /syntropy abandon (or a plain "yes"-shaped reply — see
	// isAffirmativeConfirmation) from the author confirms; ANY other event
	// drops the confirmation window. Handle before the generic control-
	// command path so cmdAbandon sees r.Status == AwaitingAbandonConfirm.
	if r.Status == StatusAwaitingAbandonConfirm {
		if ev.IsAuthor && ev.Kind == provider.EventNoteAdded {
			verb, _ := parseControlVerb(ev.Note.Body)
			if verb == "abandon" {
				d.reactToNote(ctx, r, ev)
				return d.handleControlCommand(ctx, r, ev)
			}
			if isAffirmativeConfirmation(ev.Note.Body) {
				d.reactToNote(ctx, r, ev)
				return d.cmdAbandon(ctx, r, ev, "")
			}
		}
		return d.dropAbandonConfirm(ctx, r, ev), nil
	}

	// Control commands from the author always take priority. Real
	// dispatcher: parseControlVerb + handleControlCommand. matchControlPrefix
	// is typo-tolerant (Levenshtein distance ≤2), so this also catches
	// misspellings like "/syntopy".
	if ev.IsAuthor && ev.Kind == provider.EventNoteAdded {
		if _, ok := matchControlPrefix(ev.Note.Body); ok {
			d.reactToNote(ctx, r, ev)
			return d.handleControlCommand(ctx, r, ev)
		}
	}

	// Provider auth events are handled here before the Paused early-return so
	// EventProviderAuthRestored can clear an auth-pause regardless of current
	// status. This path also handles the initial auth-failure notification
	// (transitions AwaitingMerge → Paused). See ADR-0038.
	if ev.Kind == provider.EventProviderAuthFailure || ev.Kind == provider.EventProviderAuthRestored {
		return d.handleProviderAuthEvent(ctx, r, ev)
	}

	// MR lifecycle truth (merged/closed) also bypasses the Paused
	// early-return below, same rationale as the auth-restore bypass above
	// (ADR-0077): whatever the Run is paused about — an unanswered
	// question, a hook failure, anything — is moot once the MR itself has
	// actually merged or closed on the provider side. Found live: a Run
	// sat Paused for over an hour after its MR had already merged, because
	// the merge event arrived while paused and the pre-ADR-0077 code had
	// no path to apply it until a human noticed and replied `/syntropy
	// resume` — syntropy should align its own state with reality the
	// moment it learns the truth, not wait to be told to look.
	if ev.Kind == provider.EventMRMerged || ev.Kind == provider.EventMRClosed {
		if unitID := unitForMR(r.Object.InFlight, ev.MR); unitID != "" {
			switch ev.Kind {
			case provider.EventMRMerged:
				return d.markUnitMerged(ctx, r, unitID, ev.MR), nil
			case provider.EventMRClosed:
				return d.markUnitBlacklisted(ctx, r, unitID, ev.MR, "MR closed without merge"), nil
			}
		}
		// No matching in-flight unit — cross-talk on an MR we no longer
		// track. Fall through to the normal Paused/AwaitingMerge handling
		// below, same as any other unmatched event.
	}

	// While paused, only control commands and MR lifecycle truth (handled
	// above) progress the Run. All other inbound events are noted
	// (EventsSeen above) but produce no transition.
	if r.Status == StatusPaused {
		return StatusPaused, nil
	}

	// Identify which in-flight unit this event is about. Cross-talk from
	// other Runs sharing the project webhook (or events on MRs we no longer
	// track) gets dropped here.
	unitID := unitForMR(r.Object.InFlight, ev.MR)
	if unitID == "" {
		return StatusAwaitingMerge, nil
	}

	// EventMRConflict is synthetic and deterministic (ADR-0080/ADR-0081):
	// the poller already established there's a conflict via GetMRState, so
	// there's nothing for the cheap filter to decide — go straight to the
	// runner, same as NoteAdded/PipelineFailed do once the filter picks
	// OutcomeInvokeSubagent. Placed after the Paused gate above (unlike
	// MRMerged/MRClosed), since a conflict is not lifecycle truth that
	// should override whatever the Run is paused about.
	if ev.Kind == provider.EventMRConflict {
		return d.invokeForEvent(ctx, r, unitID, ev)
	}

	// Remaining informational lifecycle events (MRMerged/MRClosed are
	// already handled above, before the Paused gate).
	switch ev.Kind {
	case provider.EventMRUpdated, provider.EventPipelineSucceeded:
		if ev.Kind == provider.EventPipelineSucceeded {
			// A green pipeline clears any DecisionRetryCI streak (ADR-0068)
			// so the next failure on this unit starts counting from zero
			// rather than inheriting an unrelated earlier run's near-miss.
			delete(r.Object.CIRetryCounts, unitID)
		}
		return StatusAwaitingMerge, nil // informational; runner doesn't care
	}

	// NoteAdded / PipelineFailed go through the cheap filter. Filter
	// loaded from the per-Run .star path (set by setup); falls back to
	// StubFilter for tests that don't set FilterPath.
	f := d.resolveFilter(r)
	ps := d.loadPhrases(r) // PhraseSet for the filter; nil-safe
	outcome, err := f.Eval(ev, stateToMap(r.Object), ps)
	if err != nil {
		return StatusAwaitingMerge, fmt.Errorf("resume: filter eval: %w", err)
	}

	switch outcome {
	case filter.OutcomeSkip:
		r.Object.EventsSkippedByFilter++
		return StatusAwaitingMerge, nil
	case filter.OutcomeControlCommand:
		// Shouldn't reach here — control commands are detected above. If
		// the filter promotes a non-/syntropy comment to a control command,
		// drop it for safety until the next commit handles the parser path.
		return StatusAwaitingMerge, nil
	case filter.OutcomePause:
		r.Object.PauseReason = fmt.Sprintf("filter paused on %s event", ev.Kind)
		return StatusPaused, nil
	case filter.OutcomeInvokeSubagent:
		return d.invokeForEvent(ctx, r, unitID, ev)
	}
	return StatusAwaitingMerge, fmt.Errorf("resume: unknown filter outcome %v", outcome)
}

// reactToNote acknowledges receipt of a control-command comment immediately,
// before handleControlCommand runs, so the author knows everflow picked it
// up. Best-effort — reaction failure must never block command handling.
func (d *Deps) reactToNote(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) {
	p := d.Providers[r.Object.ProviderName] // already validated in setup
	_ = p.ReactToNote(ctx, ev.MR.ProjectID, ev.MR.IID, ev.Note.ID, ev.Note.Stream, "eyes")
}

// invokeForEvent runs a subagent against a NoteAdded or PipelineFailed
// event. Syncs the unit worktree with origin/<base> first (ADR-0045),
// then builds a bounded RunRequest with the event-specific payload
// (CommentBody or CIFailure), invokes the runner, records a Turn, and
// branches on Decision:
//
//   Done       → status comment + stay AwaitingMerge
//   Ask        → pause + relay question via MR comment
//   Fail       → pause + relay reason via MR comment
//   Continue   → stay AwaitingMerge (no-op for this event)
//   NoChange   → stay AwaitingMerge (e.g. conversational comment)
//   RetryCI    → re-run the failed job(s) (ADR-0068); pause after
//                maxCIRetries consecutive retries on the same unit
//
// A Done/Continue commit rejected by the target repo's own pre-commit hook
// doesn't pause immediately: the rejection is fed back to the runner as
// Request.HookFailure for up to maxHookRetries retried turns before falling
// back to the pause (ADR-0075/ADR-0076).
//
// Git push of any code changes the runner made is deferred to the next
// commit (alongside work()'s push). Until then, status comments are still
// posted so the human can see what the agent decided.
func (d *Deps) invokeForEvent(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], unitID string, ev provider.Event) (AgentStatus, error) {
	rn, err := d.Runners.Get(r.Object.RunnerName)
	if err != nil {
		return StatusAwaitingMerge, fmt.Errorf("invokeForEvent: runner: %w", err)
	}
	p := d.Providers[r.Object.ProviderName] // already validated in setup

	// Acknowledge receipt immediately, before the (potentially long) runner
	// invocation below, so the commenter knows everflow picked it up rather
	// than missed it. Only NoteAdded events carry a comment to react to;
	// best-effort — reaction failure (or a stream with no reactions
	// endpoint, see ADR-0050) must never block the actual work.
	if ev.Kind == provider.EventNoteAdded {
		_ = p.ReactToNote(ctx, ev.MR.ProjectID, ev.MR.IID, ev.Note.ID, ev.Note.Stream, "eyes")
	}

	worktree := filepath.Join(d.RunsRoot, r.RunID, "worktrees", unitID)
	baseBranch := defaultIfEmpty(r.Object.BaseBranch, "main")

	// Refresh the view of base before the runner judges anything, so
	// conflict resolution never runs against a stale main (ADR-0045).
	// An ordinary merge conflict is not an error — SyncWithBase leaves
	// the unmerged paths in the worktree for the runner to resolve as
	// part of its turn. A genuine failure (fetch error, dirty worktree)
	// pauses the Run like the other git failures below.
	if sErr := d.Git.SyncWithBase(ctx, worktree, baseBranch); sErr != nil {
		mr := r.Object.InFlight[unitID]
		r.Object.PauseReason = fmt.Sprintf("git SyncWithBase failed before handling %s: %v", ev.Kind, sErr)
		_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
			fmt.Sprintf("⚠️ Paused — couldn't sync this branch with `%s` before handling the event: `%v`. Reply `/syntropy retry` after fixing.", baseBranch, sErr))
		return StatusPaused, nil
	}

	req := runner.Request{
		Worktree:        worktree,
		Goal:            r.Object.Goal,
		UnitID:          unitID,
		Model:           r.Object.RunnerModel,
		Budget:          r.Object.Budget,
		TitleConvention: r.Object.TitleConvention,
	}
	var phase Phase
	switch ev.Kind {
	case provider.EventNoteAdded:
		req.CommentBody = ev.Note.Body
		req.CommenterIsAuthor = ev.IsAuthor
		req.SkillCommand = fmt.Sprintf("/syntropy-address-comment %s", unitID)
		phase = PhaseAddressComment
	case provider.EventPipelineFailed:
		req.CIFailure = formatCIFailure(ev.Pipeline)
		req.SkillCommand = fmt.Sprintf("/syntropy-fix-ci %s", unitID)
		phase = PhaseFixCI
	case provider.EventMRConflict:
		// SyncWithBase above already merged origin/<base> and left any
		// conflicting paths unmerged in the worktree; list them so the
		// runner knows exactly what to resolve instead of re-deriving it.
		files, cErr := d.Git.ConflictedFiles(ctx, worktree)
		if cErr != nil {
			mr := r.Object.InFlight[unitID]
			r.Object.PauseReason = fmt.Sprintf("couldn't list conflicted files before handling %s: %v", ev.Kind, cErr)
			_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
				fmt.Sprintf("⚠️ Paused — couldn't list conflicted files for this branch: `%v`. Reply `/syntropy retry` after fixing.", cErr))
			return StatusPaused, nil
		}
		req.ConflictFiles = files
		req.SkillCommand = fmt.Sprintf("/syntropy-resolve-conflict %s", unitID)
		phase = PhaseResolveConflict
	default:
		return StatusAwaitingMerge, fmt.Errorf("invokeForEvent: unexpected event kind %s", ev.Kind)
	}
	// Per-increment scope: same rationale threading as work(). A reviewer
	// comment on an in-flight MR should be addressed within THIS
	// increment's scope, not re-triggered against the whole-spec Goal.
	applyIncrementScope(&req, r.Object, unitID)
	if r.Object.PromptInjection != "" {
		req.Goal = r.Object.PromptInjection + "\n\n---\n\n" + req.Goal
		r.Object.PromptInjection = "" // consume single-use
	}

	if err := d.verifyIsolatedWorktree(ctx, worktree); err != nil {
		mr := r.Object.InFlight[unitID]
		r.Object.PauseReason = fmt.Sprintf("worktree isolation check failed before handling %s: %v", ev.Kind, err)
		_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
			fmt.Sprintf("⚠️ Paused — worktree isolation check failed before handling the event: `%v`. Reply `/syntropy retry` after fixing.", err))
		return StatusPaused, nil
	}

	// Bounded retry loop: a Done/Continue turn whose commit is rejected by
	// the target repo's own pre-commit hook gets fed the rejection as
	// Request.HookFailure and one more runner turn to self-correct, rather
	// than pausing immediately for a human (ADR-0075/ADR-0076). Bounded by
	// maxHookRetries, mirroring the DecisionRetryCI cap pattern (ADR-0069).
	hookRetries := 0
	for {
		resp, runErr := rn.Run(ctx, req)
		turn := Turn{
			Index:     len(r.Object.History),
			UnitID:    unitID,
			Runner:    rn.Name(),
			Phase:     phase,
			Summary:   resp.Summary,
			Tokens:    resp.Tokens,
			StartedAt: orNow(resp.StartedAt),
			EndedAt:   orNow(resp.EndedAt),
		}
		if runErr != nil {
			turn.Error = runErr.Error()
		}
		r.Object.History = append(r.Object.History, turn)
		r.Object.SubagentInvocations++
		r.Object.TotalTokens += resp.Tokens

		mr := r.Object.InFlight[unitID]

		// Apply any title/description fixes the runner requested this turn
		// (ADR-0091), independent of resp.Decision — the tags are a
		// separate protocol from the decision marker.
		applyMetadataUpdates(ctx, r, p, mr.ProjectID, mr.IID, resp)

		// Phrase learning: runner can return phrases it judged safe to skip
		// next time. Appended to the per-Run YAML; capped at MaxPerRunEntries
		// before we surface a warning (ADR-0018 §4.2).
		if len(resp.Learnings.AddPhrases) > 0 {
			if ps, ok := d.loadPhrases(r).(*filter.YAMLPhraseSet); ok && ps != nil {
				if added, perr := ps.Add(resp.Learnings.AddPhrases, "subagent", mr.IID); perr == nil && added > 0 {
					if ps.OverCap() {
						_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
							fmt.Sprintf("ℹ️ The per-Run skip-phrase list has grown past %d entries. Review with `syntropy phrases promote` or trim by hand.", filter.MaxPerRunEntries))
					}
				}
			}
		}

		if runErr != nil {
			// Runner had an infrastructure-level error (timeout, API down).
			// Pause so the author can investigate; we still have the MR to
			// recover with.
			r.Object.PauseReason = fmt.Sprintf("runner error during %s: %v", phase, runErr)
			_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
				fmt.Sprintf("⚠️ Paused — runner error during %s: `%v`. Reply `/syntropy retry` to try again.", phase, runErr))
			return StatusPaused, nil
		}

		switch resp.Decision {
		case DecisionDone, DecisionContinue:
			// DecisionContinue here means the same thing ADR-0045 gave it in
			// the planned-work loop: a real partial slice was shipped, more
			// remains. Bundling it with DecisionNoChange (as this switch used
			// to) silently dropped that work — found live when a "/syntropy I
			// would like tests to cover this from the beginning" instruction
			// produced a real, passing test file that a Continue decision then
			// left permanently uncommitted, eventually tripping SyncWithBase's
			// uncommitted-changes guard on a later event. Commit/push exactly
			// like Done; only the final messaging and discussion-resolution
			// differ (see isDone below), since Continue means the reviewer's
			// thread isn't actually settled yet.
			isDone := resp.Decision == DecisionDone

			// Did the runner change anything this turn? Compare against the
			// unit's own pushed tip (origin/<branch>), NOT origin/<base>: the
			// branch always has commits beyond base once the MR exists, so a
			// base comparison would report "work" unconditionally. And
			// HasWorkBeyondBase rather than HasChanges so a runner that
			// commits its own work (clean tree) isn't mistaken for "nothing
			// changed" and its commits silently never pushed.
			branch := branchName(r.RunID, unitID)
			hasWork, gErr := d.Git.HasWorkBeyondBase(ctx, req.Worktree, branch)
			if gErr != nil {
				r.Object.PauseReason = fmt.Sprintf("git HasWorkBeyondBase error after %s: %v", phase, gErr)
				_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
					fmt.Sprintf("⚠️ Paused — couldn't inspect worktree after %s: `%v`. Reply `/syntropy retry`.", phase, gErr))
				return StatusPaused, nil
			}
			if !hasWork {
				// Runner thought it addressed the feedback but didn't actually
				// change anything. Note that on the MR and stay AwaitingMerge —
				// the reviewer can clarify if needed. Resolve the thread anyway
				// (the comment was answered, even if not via code).
				_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
					fmt.Sprintf("ℹ️ %s: %s\n\n(No code changes were needed.)", phase, resp.Summary))
				if discID := ev.Note.DiscussionID; discID != "" {
					_ = p.ResolveDiscussion(ctx, mr.ProjectID, mr.IID, discID)
				}
				return StatusAwaitingMerge, nil
			}

			// Commit + push the additional changes onto the existing branch.
			commitMsg := buildCommitMessage(phase, unitID, ev, r.RunID)
			if gErr := d.Git.Commit(ctx, req.Worktree, commitMsg); gErr != nil {
				var hookErr *git.HookRejectionError
				if errors.As(gErr, &hookErr) && hookRetries < maxHookRetries {
					// The target repo's own pre-commit hook rejected the
					// commit — almost always something the runner itself
					// can fix given the hook's complaint (ADR-0075). Feed
					// it back as HookFailure and give the runner one more
					// turn, up to maxHookRetries, before falling back to
					// pausing for a human like any other commit failure.
					hookRetries++
					req.HookFailure = hookErr.Output
					continue
				}
				if hookErr != nil {
					r.Object.PauseReason = fmt.Sprintf("commit rejected by pre-commit hook %d times in a row during %s: %s", hookRetries, phase, hookErr.Output)
					_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
						fmt.Sprintf("⚠️ Paused — the pre-commit hook keeps rejecting the commit after %d retries during %s:\n```\n%s\n```\nReply `/syntropy retry` after fixing.", hookRetries, phase, hookErr.Output))
					return StatusPaused, nil
				}
				if !errors.Is(gErr, git.ErrNoChanges) {
					r.Object.PauseReason = fmt.Sprintf("git Commit failed during %s: %v", phase, gErr)
					_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
						fmt.Sprintf("⚠️ Paused — git commit failed during %s: `%v`.", phase, gErr))
					return StatusPaused, nil
				}
				// ErrNoChanges: nothing stageable. Either the runner committed
				// its own work (clean tree — the unpushed commits are what
				// HasWorkBeyondBase saw), or it left only unstageable dirt
				// (e.g. a compiled binary Commit's filter excluded) with
				// nothing new committed. Only the former has anything to push.
				stat, sErr := d.Git.DiffShortstat(ctx, req.Worktree, branch)
				if sErr != nil {
					r.Object.PauseReason = fmt.Sprintf("git DiffShortstat error after %s: %v", phase, sErr)
					_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
						fmt.Sprintf("⚠️ Paused — couldn't inspect worktree after %s: `%v`. Reply `/syntropy retry`.", phase, sErr))
					return StatusPaused, nil
				}
				if stat == "" {
					// Same outcome as the !hasWork branch above: post a note,
					// stay AwaitingMerge. Do NOT pause — the runner addressing
					// a comment verbally (without code change) is normal.
					_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
						fmt.Sprintf("ℹ️ %s: %s\n\n(No code changes were needed.)", phase, resp.Summary))
					// Best-effort resolve so the thread doesn't sit open.
					if discID := ev.Note.DiscussionID; discID != "" {
						_ = p.ResolveDiscussion(ctx, mr.ProjectID, mr.IID, discID)
					}
					return StatusAwaitingMerge, nil
				}
				// Self-committed work: fall through to Push.
			}
			if gErr := d.Git.Push(ctx, req.Worktree, branch); gErr != nil {
				r.Object.PauseReason = fmt.Sprintf("git Push failed during %s: %v", phase, gErr)
				_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
					fmt.Sprintf("⚠️ Paused — git push failed during %s: `%v`. Reply `/syntropy retry` after fixing.", phase, gErr))
				return StatusPaused, nil
			}

			// A pushed fix_ci fix means the runner judged this a real code
			// problem rather than transient/infra noise (ADR-0068), so it
			// clears the unit's DecisionRetryCI streak the same way a green
			// pipeline does (see EventPipelineSucceeded above) — the next CI
			// failure is a fresh issue, not a continuation of an earlier
			// near-miss streak.
			if phase == PhaseFixCI {
				delete(r.Object.CIRetryCounts, unitID)
			}

			// Push landed. Resolve the originating discussion thread so the
			// reviewer sees their comment closed automatically — only when the
			// runner actually finished (Done). A Continue decision means the
			// reviewer's feedback isn't fully addressed yet, so the thread
			// stays open for whatever event continues it. Best-effort — if
			// resolving fails (auth, deleted thread, provider stub), the
			// resolve just doesn't happen and the reviewer closes manually.
			if isDone {
				if discID := ev.Note.DiscussionID; discID != "" {
					if rErr := p.ResolveDiscussion(ctx, mr.ProjectID, mr.IID, discID); rErr != nil {
						// Surface but don't fail — the change is pushed, that's
						// what matters.
						_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
							fmt.Sprintf("ℹ️ Pushed the change but couldn't resolve the thread automatically: `%v`. Please mark resolved manually.", rErr))
					}
				}
			}

			// Append actual diff shortstat as a hallucination guard so reviewers
			// can see whether the runner's summary matches what was actually pushed.
			var addressedBody string
			if isDone {
				addressedBody = fmt.Sprintf("✓ Addressed (%s): %s", phase, resp.Summary)
			} else {
				addressedBody = fmt.Sprintf("🔄 Partial progress (%s): %s\n\nMore work is needed — comment again (or reply `/syntropy prompt <text>`) to continue.", phase, resp.Summary)
			}
			if d.Git != nil {
				if stat, sErr := d.Git.DiffShortstat(ctx, req.Worktree, baseBranch); sErr == nil && stat != "" {
					addressedBody += "\n\nDiff: " + stat
				}
			}
			_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID, addressedBody)
			return StatusAwaitingMerge, nil
		case DecisionNoChange:
			// The runner can still have edited files even though it concluded
			// nothing shippable resulted — commit/push them first so a stray
			// edit doesn't sit uncommitted and trip a future SyncWithBase
			// (found live — see ADR-0074). Done/Continue above already cover
			// themselves via their own hasWork check; this is the same safety
			// net for the decisions that don't otherwise touch git.
			d.commitStrayWork(ctx, req.Worktree, branchName(r.RunID, unitID), buildCommitMessage(phase, unitID, ev, r.RunID))
			// A real human comment (EventNoteAdded) deserves an actual reply
			// with the runner's reasoning — same treatment as the Done+
			// !hasWork "(No code changes were needed.)" path above. Found
			// live: a genuine question ("why remove windows support?") got
			// a full runner turn and a real answer in resp.Summary, but the
			// original blanket silence here ("avoid a webhook loop") meant
			// it never reached the human at all — the loop concern is
			// already fully covered by isOwnEcho/RecentOutgoingHashes
			// (ADR-0035), so staying silent here no longer buys anything
			// except leaving real questions unanswered (ADR-0089). Still
			// silent for fix_ci (EventPipelineFailed): a CI failure judged
			// as needing no action can recur every retry, and posting "no
			// changes needed" on every one of those would be genuine noise
			// a human never asked for, unlike a one-off comment.
			if ev.Kind == provider.EventNoteAdded {
				_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
					fmt.Sprintf("ℹ️ %s: %s\n\n(No code changes were needed.)", phase, resp.Summary))
				if discID := ev.Note.DiscussionID; discID != "" {
					_ = p.ResolveDiscussion(ctx, mr.ProjectID, mr.IID, discID)
				}
			}
			return StatusAwaitingMerge, nil
		case DecisionAsk:
			d.commitStrayWork(ctx, req.Worktree, branchName(r.RunID, unitID), buildCommitMessage(phase, unitID, ev, r.RunID))
			r.Object.PauseReason = resp.Question
			_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
				fmt.Sprintf("❓ Paused — I need your input: %s\n\nReply `/syntropy resume` after answering, or `/syntropy skip` to abandon.", resp.Question))
			return StatusPaused, nil
		case DecisionFail:
			d.commitStrayWork(ctx, req.Worktree, branchName(r.RunID, unitID), buildCommitMessage(phase, unitID, ev, r.RunID))
			r.Object.PauseReason = resp.Summary
			_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
				fmt.Sprintf("⚠️ Paused — I couldn't address %s: %s\n\nReply `/syntropy retry`, `/syntropy skip`, or push a fix yourself.", phase, resp.Summary))
			return StatusPaused, nil
		case DecisionRetryCI:
			// Runner judged this CI failure transient/infra noise (ADR-0068) —
			// re-run the failed job(s) without touching code, up to
			// maxCIRetries times per unit. Exceeding the cap means retrying
			// alone isn't clearing it, so pause for a human rather than loop
			// forever.
			d.commitStrayWork(ctx, req.Worktree, branchName(r.RunID, unitID), buildCommitMessage(phase, unitID, ev, r.RunID))
			if r.Object.CIRetryCounts == nil {
				r.Object.CIRetryCounts = map[string]int{}
			}
			r.Object.CIRetryCounts[unitID]++
			count := r.Object.CIRetryCounts[unitID]
			if count > maxCIRetries {
				r.Object.PauseReason = fmt.Sprintf("CI failure retried %d times without resolving (%s): %s", maxCIRetries, phase, resp.Summary)
				_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
					fmt.Sprintf("⚠️ Paused — CI has looked transient %d times in a row but retrying hasn't cleared it: %s\n\nReply `/syntropy retry` after investigating, or `/syntropy skip` to abandon.", maxCIRetries, resp.Summary))
				return StatusPaused, nil
			}
			for _, job := range ev.Pipeline.FailedJobs {
				if rErr := p.RetryPipelineJob(ctx, mr.ProjectID, job.ID); rErr != nil {
					r.Object.PauseReason = fmt.Sprintf("failed to retry CI job %d during %s: %v", job.ID, phase, rErr)
					_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
						fmt.Sprintf("⚠️ Paused — CI looked transient but retrying job %d failed: `%v`. Reply `/syntropy retry` after fixing.", job.ID, rErr))
					return StatusPaused, nil
				}
			}
			_ = postBotReply(ctx, r, p, mr.ProjectID, mr.IID, ev.Note.DiscussionID,
				fmt.Sprintf("🔁 CI failure looked transient (retry %d/%d): %s", count, maxCIRetries, resp.Summary))
			return StatusAwaitingMerge, nil
		}
		return StatusAwaitingMerge, fmt.Errorf("invokeForEvent: unhandled decision %v", resp.Decision)
	}
}

// maxCIRetries caps how many consecutive DecisionRetryCI outcomes
// invokeForEvent will act on per unit before giving up and pausing for a
// human (ADR-0068).
const maxCIRetries = 3

// maxHookRetries caps how many times, within a single invokeForEvent call,
// a Done/Continue turn gets retried after its commit is rejected by the
// target repo's own pre-commit hook before invokeForEvent gives up and
// pauses for a human (ADR-0075/ADR-0076). Scoped to one event rather than
// persisted across events like maxCIRetries — each retry re-invokes the
// runner synchronously in the same call, so there's no daemon-restart gap
// to survive.
const maxHookRetries = 2

// --- resume helpers ---

func (d *Deps) markUnitMerged(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], unitID string, mr provider.MR) AgentStatus {
	delete(r.Object.InFlight, unitID)
	r.Object.Completed = append(r.Object.Completed, CompletedUnit{
		UnitID:   unitID,
		MR:       mr,
		MergedAt: time.Now(),
	})
	updatePlanOutcome(r.Object, unitID, "completed")
	if r.Object.CurrentUnit == unitID {
		r.Object.CurrentUnit = ""
	}
	d.cleanupWorktree(ctx, r, unitID)
	return StatusDiscovering
}

func (d *Deps) markUnitBlacklisted(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], unitID string, mr provider.MR, reason string) AgentStatus {
	delete(r.Object.InFlight, unitID)
	r.Object.Blacklisted = append(r.Object.Blacklisted, BlacklistedUnit{
		UnitID: unitID,
		MR:     mr,
		Reason: reason,
		At:     time.Now(),
	})
	updatePlanOutcome(r.Object, unitID, "blacklisted")
	if r.Object.CurrentUnit == unitID {
		r.Object.CurrentUnit = ""
	}
	d.cleanupWorktree(ctx, r, unitID)
	return StatusDiscovering
}

// providerAuthPausePrefix is the PauseReason prefix the workflow uses when
// parking a Run due to a provider authentication failure. The poller checks
// for this prefix on recovery to distinguish auth-pauses from human-pauses.
const providerAuthPausePrefix = "provider-auth: "

// handleProviderAuthEvent handles EventProviderAuthFailure and
// EventProviderAuthRestored events synthesised by the poller (ADR-0038).
//
//   - AuthFailure: park the Run (Paused with providerAuthPausePrefix) and
//     post a comment on the in-flight MR. Idempotent — if the Run is already
//     in an auth-pause, stay there without posting a duplicate comment.
//   - AuthRestored: if the Run is in an auth-pause, clear PauseReason and
//     return to AwaitingMerge. No-op for any other pause reason.
//
// Since ADR-0078, the provider's do() already retries once with a
// force-refreshed token on a 401 before returning an error at all — so by
// the time a 401 reaches the poller (and this handler), a same-token
// refresh has already been tried and failed. That makes this a full
// interactive re-login case, not just a stale access token, hence the
// wording below.
func (d *Deps) handleProviderAuthEvent(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) (AgentStatus, error) {
	switch ev.Kind {
	case provider.EventProviderAuthFailure:
		if strings.HasPrefix(r.Object.PauseReason, providerAuthPausePrefix) {
			return StatusPaused, nil // already parked; skip duplicate comment
		}
		r.Object.PauseReason = providerAuthPausePrefix +
			"full re-login required — a force-refreshed retry still got a 401; run `gh auth login` (GitHub) or `glab auth login` (GitLab) and restart the daemon"
		if p, ok := d.Providers[r.Object.ProviderName]; ok {
			for _, mr := range r.Object.InFlight {
				_ = postBotComment(ctx, r, p, mr.ProjectID, mr.IID,
					"⚠️ Paused — the provider token needs a full re-login (HTTP 401 persisted even after a forced refresh). "+
						"Please re-authenticate (`gh auth login` / `glab auth login`) and restart the daemon. "+
						"The Run will resume automatically on the next successful poll.")
				break // one comment is enough; concurrency=1 in v1
			}
		}
		return StatusPaused, nil

	case provider.EventProviderAuthRestored:
		if !strings.HasPrefix(r.Object.PauseReason, providerAuthPausePrefix) {
			// Not in an auth-pause; nothing to do.
			return r.Status, nil
		}
		r.Object.PauseReason = ""
		return StatusAwaitingMerge, nil
	}
	return r.Status, nil
}

// dropAbandonConfirm is the "any non-abandon activity" handler for the
// 12h confirmation window. Clears AbandonRequestedAt, posts an ack
// comment, returns AwaitingMerge. The event that triggered the drop is
// itself dropped — the author re-comments if they want it processed.
func (d *Deps) dropAbandonConfirm(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) AgentStatus {
	r.Object.AbandonRequestedAt = time.Time{}
	if p, ok := d.Providers[r.Object.ProviderName]; ok {
		_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
			"ℹ️ Activity detected during the abandon confirmation window — abandon cancelled; watching for events again.")
	}
	return StatusAwaitingMerge
}

// onAbandonConfirmTimeout fires 12h after the /syntropy abandon was
// requested. Posts a comment so the author sees the window closed, then
// drops back to AwaitingMerge. Comment is best-effort against whatever
// in-flight MR we can find (there's at most one in v1 with concurrency=1).
func (d *Deps) onAbandonConfirmTimeout(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], _ time.Time) (AgentStatus, error) {
	r.Object.AbandonRequestedAt = time.Time{}
	if p, ok := d.Providers[r.Object.ProviderName]; ok {
		for _, mr := range r.Object.InFlight {
			_ = postBotComment(ctx, r, p, mr.ProjectID, mr.IID,
				"⏰ Abandon confirmation window (12h) expired — staying with the Run.")
			break
		}
	}
	return StatusAwaitingMerge, nil
}

// cleanupWorktree is best-effort — failure here doesn't block the Run.
// Orphaned worktrees can be cleaned up out-of-band via a future `everflow
// gc` command.
func (d *Deps) cleanupWorktree(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], unitID string) {
	if d.Git == nil || r.Object.BaseRepo == "" {
		return
	}
	worktree := filepath.Join(d.RunsRoot, r.RunID, "worktrees", unitID)
	_ = d.Git.RemoveWorktree(ctx, r.Object.BaseRepo, worktree)
}

// unitForMR matches an inbound MR to the unitID that owns it in InFlight.
// Returns "" when the event is for an MR we don't track (cross-talk).
func unitForMR(inFlight map[string]provider.MR, mr provider.MR) string {
	for unitID, m := range inFlight {
		if m.ProjectID == mr.ProjectID && m.IID == mr.IID {
			return unitID
		}
	}
	return ""
}

func isFromAuthor(commenter, runAuthor provider.User) bool {
	if commenter.Handle == "" || runAuthor.Handle == "" {
		return false
	}
	return commenter.Handle == runAuthor.Handle
}

// formatCIFailure produces a compact summary of failed jobs for the runner.
// LogTail fetching is the provider client's job (not done in this commit);
// we pass through the names and stages so the runner can decide what to do.
func formatCIFailure(p provider.Pipeline) string {
	if len(p.FailedJobs) == 0 {
		return fmt.Sprintf("Pipeline %d failed with status %q (no per-job details available).", p.ID, p.Status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pipeline %d failed. Failed jobs:\n", p.ID)
	for _, j := range p.FailedJobs {
		fmt.Fprintf(&b, "  - %s (stage: %s)\n", j.Name, j.Stage)
		if j.LogTail != "" {
			fmt.Fprintf(&b, "    tail:\n%s\n", indent(j.LogTail, "      "))
		}
	}
	return b.String()
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

// --- helpers ---

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// resolveFilter picks the Filter to use for this Run. Priority:
//   1. Test override via Deps.Filter
//   2. Starlark filter loaded from AgentState.FilterPath
//   3. StubFilter (defensive — should never be reached after setup())
func (d *Deps) resolveFilter(r *workflow.Run[AgentState, AgentStatus]) filter.Filter {
	if d.Filter != nil {
		return d.Filter
	}
	if r.Object.FilterPath != "" {
		return filter.NewStarlarkFilter(r.Object.FilterPath)
	}
	return filter.StubFilter{}
}

// loadPhrases loads the per-Run + global phrase files. Returns nil if
// loading fails (the filter handles a nil PhraseSet gracefully). Per-Run
// path is <RunsRoot>/<runID>/phrases.yaml; global path is
// <parent(RunsRoot)>/phrases.global.yaml.
func (d *Deps) loadPhrases(r *workflow.Run[AgentState, AgentStatus]) filter.PhraseSet {
	if d.RunsRoot == "" {
		return nil
	}
	perRun := filepath.Join(d.RunsRoot, r.RunID, "phrases.yaml")
	global := filepath.Join(filepath.Dir(d.RunsRoot), "phrases.global.yaml")
	ps, err := filter.LoadYAMLPhrases(perRun, global)
	if err != nil {
		return nil
	}
	return ps
}

// stateToMap exposes a curated subset of AgentState to the Starlark
// filter. We keep this conservative — only fields a filter author
// plausibly needs to make a decision. Adding fields is easy; removing
// them is a breaking change for users with custom filters.
func stateToMap(s *AgentState) map[string]any {
	return map[string]any{
		"goal":              s.Goal,
		"mode":              s.Mode,
		"provider":          s.ProviderName,
		"project":           s.ProjectID,
		"completed_count":   int64(len(s.Completed)),
		"blacklisted_count": int64(len(s.Blacklisted)),
		"in_flight_count":   int64(len(s.InFlight)),
		"queue_count":       int64(len(s.Queue)),
		"plan_count":        int64(len(s.Plan)),
		"events_seen":       int64(s.EventsSeen),
		"subagent_invocations": int64(s.SubagentInvocations),
	}
}

func providerNames(m map[string]provider.Provider) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// branchName picks the source branch name for a unit's MR. Format:
// `syntropy/<short-runID>/<unitID>`. Short runID keeps the branch readable
// while staying unique across concurrent refactors.
func branchName(runID, unitID string) string {
	return fmt.Sprintf("syntropy/%s/%s", shortRunID(runID), unitID)
}

// shortRunID is used in branch names, commit footers, and labels.
func shortRunID(runID string) string {
	if len(runID) > 8 {
		return runID[:8]
	}
	return runID
}

// commitStrayWork is a safety net for invokeForEvent decision paths that
// don't otherwise touch git (NoChange/Ask/Fail/RetryCI): the runner can
// still have edited files even after concluding nothing shippable came of
// it — e.g. a partial edit made before deciding to ask a question, or
// before giving up. Left uncommitted, that silently trips a future
// SyncWithBase's uncommitted-changes guard (found live — see ADR-0074).
//
// Commits ONLY — deliberately does not push. These decisions are the
// runner saying "this isn't the deliverable" (gave up, needs input,
// judged CI as infra noise), so there's no verification the stray edit
// even builds; pushing it straight to a branch reviewers/CI will see
// would trade the uncommitted-tree bug for a worse one (unreviewed,
// unverified code landing on the MR). A local commit already satisfies
// SyncWithBase (it only checks for a dirty tree) without that exposure —
// the work isn't lost, either: the next Continue/Done turn's own
// HasWorkBeyondBase check (or a later push some other command triggers)
// will find it and push it once something actually says it's shippable.
//
// Best-effort: any error here is swallowed rather than surfaced, since it
// must not block the decision's own pause/reply handling below — worst
// case is the same uncommitted-changes guard firing as before this fix
// existed, not a new failure mode.
func (d *Deps) commitStrayWork(ctx context.Context, worktree, branch, commitMsg string) {
	if d.Git == nil {
		return
	}
	hasWork, err := d.Git.HasWorkBeyondBase(ctx, worktree, branch)
	if err != nil || !hasWork {
		return
	}
	_ = d.Git.Commit(ctx, worktree, commitMsg)
}

// buildCommitMessage produces a commit message for a follow-up commit
// (addressing a review comment or fixing CI). Includes the event's source
// so the audit trail in `git log` matches the MR conversation.
func buildCommitMessage(phase Phase, unitID string, ev provider.Event, runID string) string {
	var subject string
	switch phase {
	case PhaseAddressComment:
		who := ev.Author.Handle
		if who == "" {
			who = "reviewer"
		}
		subject = fmt.Sprintf("Address review feedback on %s from @%s", unitID, who)
	case PhaseFixCI:
		subject = fmt.Sprintf("Fix CI on %s (pipeline %d)", unitID, ev.Pipeline.ID)
	case PhaseResolveConflict:
		subject = fmt.Sprintf("Resolve merge conflict on %s", unitID)
	default:
		subject = fmt.Sprintf("%s on %s", phase, unitID)
	}
	return fmt.Sprintf("%s\n\nGenerated by syntropy run %s.\n", subject, shortRunID(runID))
}

// verifyIsolatedWorktree is the deterministic, non-LLM guard checked
// immediately before every runner invocation that grants
// --dangerously-skip-permissions (see the claude runner's BuildArgs and
// git.Git.IsIsolatedWorktree). It refuses to proceed unless dir is a real
// linked git worktree distinct from the main checkout, so a misconfigured
// or reused directory can never receive filesystem-write access meant only
// for a disposable per-unit worktree.
func (d *Deps) verifyIsolatedWorktree(ctx context.Context, dir string) error {
	if d.Git == nil {
		return fmt.Errorf("verifyIsolatedWorktree: no Git configured")
	}
	isolated, err := d.Git.IsIsolatedWorktree(ctx, dir)
	if err != nil {
		return fmt.Errorf("verifyIsolatedWorktree: %w", err)
	}
	if !isolated {
		return fmt.Errorf("verifyIsolatedWorktree: %s is not an isolated git worktree; refusing to invoke runner with --dangerously-skip-permissions", dir)
	}
	return nil
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
