// Package runner is the abstraction between the workflow loop and a coding
// agent (Claude Code, Qwen Code, OpenHands). See ADR-0007 and ADR-0008.
package runner

import (
	"context"
	"fmt"
	"time"
)

// Decision is what a runner invocation tells the workflow to do next.
// The runner produces structured output (ADR-0008) that maps onto these.
// Defined here (not in refactorsweep) to avoid an import cycle: the runner
// is a leaf that other packages depend on.
type Decision int

const (
	DecisionUnknown  Decision = 0
	DecisionContinue Decision = 1 // make progress; stay in current macro-state
	DecisionAsk      Decision = 2 // pause and ask the author via MR comment
	DecisionDone     Decision = 3 // unit complete; ship the MR
	DecisionFail     Decision = 4 // unit unrecoverable; blacklist + move on
	DecisionNoChange Decision = 5 // nothing to do this invocation (e.g. conversational comment)
	DecisionRetryCI  Decision = 6 // CI failure looks transient/infra; re-run without code changes
)

func (d Decision) String() string {
	return [...]string{"Unknown", "Continue", "Ask", "Done", "Fail", "NoChange", "RetryCI"}[d]
}

// Budget caps a Run's cumulative cost. Hit any of these and the Run pauses.
// Same reason as Decision for living in this package — refactorsweep
// references it, refactorsweep imports runner, runner can't import back.
type Budget struct {
	MaxUnits   int           `json:"max_units"`
	MaxTokens  int           `json:"max_tokens"`
	MaxRuntime time.Duration `json:"max_runtime"`
}

type Runner interface {
	Name() string
	Run(ctx context.Context, req Request) (Response, error)
}

// Request is what the workflow hands a runner per invocation. Bounded by
// design: only this unit's scope, not the whole refactor's history.
type Request struct {
	Worktree     string
	SkillCommand string // "/refactor-logrus-to-slog services/payments"
	Goal         string
	UnitID       string
	UnitContext  string
	Model        string // spec-selected model override; empty means the runner's default

	// TitleConvention is the BaseRepo's .syntropy.yml title_convention
	// (ADR-0052), read once in setup() and threaded through unit-scoped
	// invocations so the runner shapes MR titles accordingly.
	TitleConvention string

	// Replayed inputs for "address comment" / "fix CI" / "resolve conflict"
	// invocations:
	CommentBody   string   // populated for address-comment invocations
	CIFailure     string   // populated for fix-CI invocations (last ~2KB of log)
	ConflictFiles []string // populated for resolve-conflict invocations

	// HookFailure is populated when this invocation is a retry after the
	// target repo's own pre-commit hooks rejected a prior commit attempt
	// during address_comment/fix_ci (ADR-0075). Holds the hook's rejection
	// output so the runner can see why its commit was refused and adjust
	// before retrying, instead of the Run pausing for a human immediately.
	HookFailure string

	// CommenterIsAuthor reports whether CommentBody came from the Run's
	// author. When false, the prompt tells the runner to auto-implement
	// only objective defects and route solution-steering suggestions to
	// DecisionAsk for the author's approval (ADR-0072). Meaningless when
	// CommentBody is empty.
	CommenterIsAuthor bool

	Timeout time.Duration
	Budget  Budget
}

// Response is what a runner reports back. Maps onto the workflow's next state
// transition via Decision.
type Response struct {
	Decision  Decision
	Summary   string    // one-paragraph "what I did this invocation"
	Question  string    // populated when Decision == Ask
	Learnings Learnings // see ADR-0018; populated when the subagent surfaces patterns worth caching
	Tokens    int
	StartedAt time.Time
	EndedAt   time.Time

	// Title is the runner's suggested MR title for this unit, phrased per
	// Request.TitleConvention (ADR-0052/ADR-0054). Populated only when
	// Decision == Done and a TitleConvention was set on the Request; empty
	// otherwise, in which case the caller falls back to its own default
	// title.
	Title string
}

// Learnings are the subagent's optional output to feed the cheap-filter loop.
// Bounded by the per-Run cap (50 entries) before flagging for review;
// promotion to global phrases is manual via `everflow phrases promote`.
type Learnings struct {
	AddPhrases  []string `json:"add_phrases"`  // skip-phrases to append to the per-Run file
	SkillUpdate string   `json:"skill_update"` // optional revised SKILL.md content
}

// Registry holds runners by name. Runners self-register at init() time.
type Registry struct {
	runners map[string]Runner
}

func NewRegistry() *Registry { return &Registry{runners: map[string]Runner{}} }

func (r *Registry) Register(rn Runner) { r.runners[rn.Name()] = rn }

func (r *Registry) Get(name string) (Runner, error) {
	rn, ok := r.runners[name]
	if !ok {
		return nil, fmt.Errorf("unknown runner %q (registered: %v)", name, r.Names())
	}
	return rn, nil
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.runners))
	for k := range r.runners {
		out = append(out, k)
	}
	return out
}
