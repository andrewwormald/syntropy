// Package refactorsweep defines the v1 state machine: AgentState held in the
// workflow's Run.Object, the AgentStatus enum that drives transitions, and
// the Decision/Turn/Budget types shared with the runner.
//
// See ../../DESIGN.md § "The state machine" and ADRs 0014, 0015, 0017.
package refactorsweep

import (
	"time"

	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
)

// AgentStatus enumerates the workflow states. Cycles allowed:
//
//	Initiated → Discovering → Working → AwaitingMerge → Working (next unit)
//	                                  → Paused (author intervention)
//	                                  → Completed (no units left)
type AgentStatus int

const (
	StatusUnknown                AgentStatus = 0
	StatusInitiated              AgentStatus = 1 // Run created; webhook not yet registered
	StatusDiscovering            AgentStatus = 2 // looking for the next unit
	StatusWorking                AgentStatus = 3 // subagent making the change + opening MR
	StatusAwaitingMerge          AgentStatus = 4 // MR open; webhook-driven idle
	StatusPaused                 AgentStatus = 5 // author intervention required
	StatusCompleted              AgentStatus = 6 // refactor done; no units left
	StatusFailed                 AgentStatus = 7 // unrecoverable; worktree kept for forensics
	StatusCancelled              AgentStatus = 8 // author stopped the Run (/syntropy stop or /abandon-confirm)
	StatusAwaitingAbandonConfirm AgentStatus = 9 // /syntropy abandon issued; awaiting second tap within 12h (ADR-0026)
)

func (s AgentStatus) String() string {
	return [...]string{
		"Unknown", "Initiated", "Discovering", "Working",
		"AwaitingMerge", "Paused", "Completed", "Failed", "Cancelled",
		"AwaitingAbandonConfirm",
	}[s]
}

// AgentState is the per-Run durable object. Everything everflow needs to
// resume after a daemon restart lives here.
type AgentState struct {
	// Set at Trigger, immutable after:
	Goal            string         `json:"goal"`
	ProviderName    string         `json:"provider_name"`     // "gitlab" | "github"
	ProjectID       string         `json:"project_id"`
	BaseRepo        string         `json:"base_repo"`         // local path to a git checkout the daemon can clone worktrees off
	BaseBranch      string         `json:"base_branch"`
	RunnerName      string         `json:"runner_name"`       // "claude" | "qwen" | "openhands" — see ADR-0007
	RunnerModel     string         `json:"runner_model"`      // spec's `model:` override, passed to every runner.Request.Model — see ADR-0044
	Budget          runner.Budget  `json:"budget"`
	Author          provider.User  `json:"author"`            // see ADR-0017
	Concurrency     int            `json:"concurrency"`       // semaphore size; v1 = 1

	// Mode picks how discover() behaves: sweep (queue-pop) or spec (planner).
	// See ADR-0024. Empty == ModeSweep for backwards compatibility.
	Mode            string         `json:"mode"`              // "" | "sweep" | "spec"
	SpecPath        string         `json:"spec_path"`         // populated in spec mode
	SpecBody        string         `json:"spec_body"`         // markdown body the planner reads each iteration
	DraftMRs        bool           `json:"draft_mrs"`         // when true, opened MRs are marked Draft / WIP (safety net for spikes against shared repos)
	EventSource     string         `json:"event_source"`      // "poll" (default) | "webhook" — ADR-0031

	SkillPath       string         `json:"skill_path"`        // ~/.syntropy/runs/<runID>/SKILL.md
	FilterPath      string         `json:"filter_path"`       // ~/.syntropy/runs/<runID>/note_added.star
	DiscoveryPath   string         `json:"discovery_path"`    // optional discovery rule
	TitleConvention string         `json:"title_convention"`  // BaseRepo's .syntropy.yml title_convention, read once in setup() — ADR-0052
	WebhookID       string         `json:"webhook_id"`        // platform's hook ID, for cleanup
	WebhookSecret   string         `json:"webhook_secret"`    // HMAC secret we registered with
	WebhookURL      string         `json:"webhook_url"`       // public URL we registered

	// Mutated through the Run's lifecycle:
	Queue            []string             `json:"queue"`              // unit IDs awaiting processing (sweep mode only)
	Plan             []PlannedIncrement   `json:"plan"`               // planner history (spec mode only) — ADR-0025
	InFlight         map[string]provider.MR `json:"in_flight"`        // unit ID → MR
	Completed        []CompletedUnit      `json:"completed"`
	Blacklisted      []BlacklistedUnit    `json:"blacklisted"`
	CurrentUnit      string               `json:"current_unit"`       // populated while StatusWorking | StatusAwaitingMerge
	History          []Turn               `json:"history"`
	LastError          string             `json:"last_error"`
	PauseReason        string             `json:"pause_reason"`         // populated when StatusPaused
	PromptInjection    string             `json:"prompt_injection"`     // /syntropy prompt <text>; consumed by next runner call
	AbandonRequestedAt time.Time          `json:"abandon_requested_at"` // populated when StatusAwaitingAbandonConfirm (ADR-0026)

	// IdenticalPauseStreak counts consecutive freeform-note invocations,
	// during a non-Ask pause, that left PauseReason exactly as it was
	// beforehand — i.e. the runner looked at the note and landed back on
	// the same stuck conclusion. Reset to 0 whenever PauseReason changes or
	// a control command clears/re-establishes the pause. Once it reaches
	// maxIdenticalPauseStreak, resume() stops invoking the runner for this
	// pause until a control command breaks the loop (ADR-0111) — otherwise
	// a human idly replying to a pause comment burns tokens forever without
	// the Run ever moving.
	IdenticalPauseStreak int `json:"identical_pause_streak,omitempty"`

	// CIRetryCounts tracks, per in-flight unit ID, how many consecutive
	// times invokeForEvent has retried a CI failure the runner judged
	// transient (DecisionRetryCI, ADR-0068). The entry is deleted on the
	// next EventPipelineSucceeded for that unit, and capped at
	// maxCIRetries before invokeForEvent gives up and pauses for a human.
	CIRetryCounts map[string]int `json:"ci_retry_counts,omitempty"`

	// Counters for the "is learning working?" signal (DESIGN.md open question 1):
	EventsSeen           int `json:"events_seen"`
	EventsSkippedByFilter int `json:"events_skipped_by_filter"`
	SubagentInvocations  int `json:"subagent_invocations"`

	// Token + runtime accounting for Budget enforcement (ADR-0036).
	// TotalTokens accumulates resp.Tokens across every runner invocation;
	// StartedAt is set once in setup() to enable MaxRuntime checks.
	TotalTokens int       `json:"total_tokens,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`

	// Polling state — populated only in poll mode (ADR-0031).
	// Keyed by MR IID. Updated by the poller as new comments arrive and
	// MR states transition.
	//
	// LastSeenNoteIDs is the pre-ADR-0041 single scalar watermark per MR.
	// It's still updated (never removed — old Runs in flight depend on
	// it) and used as provider.NoteCursor.Legacy: the floor applied to
	// any comment stream not yet present in LastSeenNoteIDsByStream.
	LastSeenNoteIDs map[int]int64 `json:"last_seen_note_ids,omitempty"`
	// LastSeenNoteIDsByStream maps MR IID → per-stream (provider-defined,
	// e.g. GitHub's "issue_comment" / "pull_request_review_comment" /
	// "pull_request_review"; GitLab's single "note") high-water mark.
	// Added by ADR-0041 to fix a cross-stream watermark bug: GitHub's
	// comment endpoints draw ids from independent sequences, so the old
	// single LastSeenNoteIDs scalar could silently and permanently drop
	// a comment whose id was lower than one already seen on a different
	// stream. Additive: existing Runs have this field empty/nil and fall
	// back to LastSeenNoteIDs per stream until each stream sees its first
	// comment post-migration.
	LastSeenNoteIDsByStream map[int]map[string]int64 `json:"last_seen_note_ids_by_stream,omitempty"`
	LastMRStates            map[int]string            `json:"last_mr_states,omitempty"`

	// RecentOutgoingHashes is a bounded FIFO of SHA-256 hex hashes of the
	// most recent comment bodies the daemon has posted on behalf of this
	// Run. resume() consults it on every inbound note_added event and
	// silently drops any note whose body matches — the self-comment loop
	// prevention. Without this, every daemon-posted comment (initial
	// "🤖 Opened", "✓ Addressed", etc.) fires a redundant claude -p on
	// the next poll tick because the poll can't distinguish the daemon
	// from the author (they share the same OAuth identity).
	//
	// Cap: 32 (see recentOutgoingHashCap in workflow.go). Poll cadence
	// is 30s; a realistic burst of daemon comments in that window is
	// well under 20. Extra headroom is cheap — 32 × 64 hex chars ≈ 2KB
	// per Run of durable state.
	RecentOutgoingHashes []string `json:"recent_outgoing_hashes,omitempty"`
}

// EventSource values for AgentState.EventSource.
const (
	EventSourcePoll    = "poll"    // poll the provider periodically (default — ADR-0031)
	EventSourceWebhook = "webhook" // register a webhook on the provider (legacy / VPS-on-stable-URL deployments)
)

// IsPollMode reports whether this Run gets events via polling rather
// than webhooks. Empty defaults to poll for safety — no accidental
// webhook registration on shared / production repos.
func (s *AgentState) IsPollMode() bool {
	return s.EventSource == "" || s.EventSource == EventSourcePoll
}

// Mode values for AgentState.Mode. Empty Mode behaves as ModeSweep for
// backwards compatibility with v1 Runs created before this field existed.
const (
	ModeSweep = "sweep"
	ModeSpec  = "spec"
)

// IsSpecMode reports whether this Run plans each increment via the runner
// (spec mode) rather than popping a static queue (sweep mode). Empty Mode
// is treated as sweep.
func (s *AgentState) IsSpecMode() bool {
	return s.Mode == ModeSpec
}

// PlannedIncrement is one entry in the planner's audit trail. Each entry
// records a unit the planner chose, the rationale, and the eventual
// outcome — useful both for debugging "why did the agent pick this?" and
// for re-priming the planner on the next iteration.
type PlannedIncrement struct {
	UnitID    string    `json:"unit_id"`
	Rationale string    `json:"rationale"`            // the planner's stated reason for this increment
	PlannedAt time.Time `json:"planned_at"`
	Outcome   string    `json:"outcome,omitempty"`    // "in_flight" | "completed" | "blacklisted" | ""

	// RemainderNote is set when the unit's work turn shipped a partial MR
	// (DecisionContinue instead of DecisionDone) rather than silently
	// treating the partial diff as the whole unit. It carries the
	// runner's own account of what's left so the planner can pick it up
	// as a follow-on increment. Empty when the unit shipped in full.
	RemainderNote string `json:"remainder_note,omitempty"`
}

// CompletedUnit records a shipped MR.
type CompletedUnit struct {
	UnitID    string    `json:"unit_id"`
	MR        provider.MR `json:"mr"`
	MergedAt  time.Time `json:"merged_at"`
	Tokens    int       `json:"tokens"`
}

// BlacklistedUnit records a unit we won't retry. Reason matters for diagnosis
// ("reviewer rejected with: please don't do this here", "CI permanently red").
type BlacklistedUnit struct {
	UnitID    string    `json:"unit_id"`
	MR        provider.MR `json:"mr"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// Turn records one subagent invocation. Useful for audit, debugging,
// and computing the "learning is working" signal.
type Turn struct {
	Index     int       `json:"index"`
	UnitID    string    `json:"unit_id"`         // empty for non-unit invocations
	Runner    string    `json:"runner"`           // "claude" | "qwen" | ...
	Phase     Phase     `json:"phase"`
	Summary   string    `json:"summary"`
	Tokens    int       `json:"tokens"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Error     string    `json:"error,omitempty"`
}

// Phase identifies what kind of runner invocation a Turn records. String-
// backed (rather than an int like runner.Decision) so existing persisted
// Run JSON keeps deserializing without a migration.
type Phase string

const (
	PhasePlan            Phase = "plan"
	PhaseWork            Phase = "work"
	PhaseAddressComment  Phase = "address_comment"
	PhaseFixCI           Phase = "fix_ci"
	PhaseResolveConflict Phase = "resolve_conflict"
)

// Decision is re-exported from internal/runner as an alias so step-body
// callers within refactorsweep don't have to type-qualify everywhere.
type Decision = runner.Decision

const (
	DecisionContinue = runner.DecisionContinue
	DecisionAsk      = runner.DecisionAsk
	DecisionDone     = runner.DecisionDone
	DecisionFail     = runner.DecisionFail
	DecisionNoChange = runner.DecisionNoChange
	DecisionRetryCI  = runner.DecisionRetryCI
)
