// Package claude implements runner.Runner by shelling out to the
// `claude` CLI. See ADR-0027 for the prompt-marker protocol and
// ADR-0004 for the original "shell out, not the SDK" decision.
//
// The adapter is dumb: it composes a prompt from the runner.Request
// fields (Goal, Worktree, UnitID, CommentBody, CIFailure, ConflictFiles,
// HookFailure, ParseFailure), appends a
// decision-marker instruction, runs `claude -p --output-format json`,
// and parses the JSON envelope for token counts and the decision marker
// embedded in the result text. It does not interpret SkillCommand — the
// step body is responsible for setting Goal to a fully-formed task; this
// adapter just adds the protocol envelope claude needs to signal back.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/andrewwormald/syntropy/internal/runner"
)

// claudeJSONResult is the envelope `claude -p --output-format json` writes to
// stdout. Only the fields syntropy needs are unmarshalled; unknown fields are
// silently ignored.
type claudeJSONResult struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	// Result is the full text the model produced, including the
	// <syntropy-decision> marker that ParseDecision reads.
	Result string `json:"result"`
	// Usage is populated by claude ≥ 1.x; older builds omit it.
	Usage *claudeUsage `json:"usage,omitempty"`
}

// claudeUsage mirrors the Anthropic API usage block embedded in the JSON
// output. Input + output token counts are the primary interest; cache
// tokens are included so the sum represents total tokens billed.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// totalTokens returns the sum of all token fields.
func (u *claudeUsage) totalTokens() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens
}

// parseJSONOutput tries to decode rawOut as a claudeJSONResult. On success it
// returns (resultText, tokens, true). On any parse failure it returns
// ("", 0, false) so the caller can fall back to treating rawOut as plain text.
func parseJSONOutput(rawOut string) (result string, tokens int, ok bool) {
	var parsed claudeJSONResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawOut)), &parsed); err != nil {
		return "", 0, false
	}
	// Guard against an unrecognised envelope (e.g. a plain-JSON error message
	// from a wrapper script). We require type=="result" AND a non-empty Result.
	if parsed.Type != "result" || parsed.Result == "" {
		return "", 0, false
	}
	return parsed.Result, parsed.Usage.totalTokens(), true
}

// Runner is the claude.Runner. The zero value is usable (uses `claude`
// from $PATH with no extra args). NewRunner is the canonical constructor.
type Runner struct {
	// Binary is the path to the claude CLI. Defaults to "claude".
	Binary string

	// ExtraArgs is prepended to the call's args (after -p). Useful for
	// --model overrides, --debug, etc.
	ExtraArgs []string

	// Env, if non-nil, replaces os.Environ() for the subprocess. The
	// default (nil) inherits the daemon's env, which is what production
	// wants — ANTHROPIC_API_KEY, GIT_TOKEN, $HOME are all inherited.
	Env []string
}

// NewRunner constructs a Runner. Both fields are optional.
func NewRunner(binary string, extraArgs ...string) *Runner {
	if binary == "" {
		binary = "claude"
	}
	return &Runner{Binary: binary, ExtraArgs: extraArgs}
}

// Verify Runner satisfies runner.Runner at compile time.
var _ runner.Runner = (*Runner)(nil)

func (c *Runner) Name() string { return "claude" }

// nestedClaudeCodeEnvPrefixes match environment variables Claude Code sets
// on every process it spawns to identify that process as a nested child of
// the invoking interactive session. If this Runner's own process inherited
// them (e.g. the daemon was launched via `syntropy daemon &` from inside an
// active Claude Code session's Bash tool — main.go's unsetNestedClaudeCodeEnv
// is the primary defense for that specific case, called once at daemon
// startup), every claude -p subprocess spawned here would otherwise inherit
// them too via os.Environ() and be mistaken for a nested child of that
// session — a session that may still be active and completely unrelated to
// this invocation, causing spurious exit-1 failures. Filtered defensively
// here as well in case something else in the process re-sets them after
// startup. See ADR-0064.
var nestedClaudeCodeEnvPrefixes = []string{
	"CLAUDECODE=",
	"CLAUDE_CODE_SESSION_ID=",
	"CLAUDE_CODE_ENTRYPOINT=",
	"CLAUDE_CODE_CHILD_SESSION=",
}

// stripNestedClaudeCodeEnv returns env with any nestedClaudeCodeEnvPrefixes
// entries removed. Exported behavior (not the function itself) is covered
// by claude_test.go; kept unexported since it's an implementation detail of
// Run, not part of this package's public API.
func stripNestedClaudeCodeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		strip := false
		for _, prefix := range nestedClaudeCodeEnvPrefixes {
			if strings.HasPrefix(kv, prefix) {
				strip = true
				break
			}
		}
		if !strip {
			out = append(out, kv)
		}
	}
	return out
}

func (c *Runner) Run(ctx context.Context, req runner.Request) (runner.Response, error) {
	start := time.Now()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args := BuildArgs(req, c.ExtraArgs)

	cmd := exec.CommandContext(ctx, c.Binary, args...)
	if req.Worktree != "" {
		cmd.Dir = req.Worktree
	}
	if c.Env != nil {
		cmd.Env = c.Env
	} else {
		cmd.Env = os.Environ()
	}
	cmd.Env = stripNestedClaudeCodeEnv(cmd.Env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	end := time.Now()
	rawOut := stdout.String()

	// Attempt JSON envelope parse to extract token counts and clean result
	// text. If parsing fails (older CLI version, wrapper script, error page),
	// fall back to treating rawOut as plain text — that preserves backward
	// compatibility and degrades gracefully to Tokens=0.
	resultText, tokens, jsonOK := parseJSONOutput(rawOut)
	if !jsonOK {
		fmt.Fprintf(os.Stderr, "claude: warning: could not parse --output-format json envelope; falling back to plain text (tokens will be 0)\n")
		resultText = rawOut
	}

	// Independent of Decision/ParseDecision — these tags can appear on any
	// turn, so they're parsed regardless of how the decision marker itself
	// turns out (ADR-0091).
	titleUpdate := ParseTitleUpdate(resultText)
	descriptionUpdate := ParseDescriptionUpdate(resultText)

	if runErr != nil {
		// Even on non-zero exit we try to parse a decision — the model
		// might have flagged failure via the marker before exiting. Fall
		// back to wrapping the OS error.
		decision, summary, question, title, parseErr := ParseDecision(resultText)
		if parseErr != nil {
			return runner.Response{
					Decision:  runner.DecisionFail,
					Summary:   strings.TrimSpace(stderr.String()),
					Tokens:    tokens,
					StartedAt: start, EndedAt: end,
				}, fmt.Errorf("claude exec: %w (stderr: %s)", runErr,
					strings.TrimSpace(stderr.String()))
		}
		return runner.Response{
			Decision:          decision,
			Summary:           summary,
			Question:          question,
			Title:             title,
			TitleUpdate:       titleUpdate,
			DescriptionUpdate: descriptionUpdate,
			Tokens:            tokens,
			StartedAt:         start, EndedAt: end,
		}, fmt.Errorf("claude exec: %w (parsed decision: %s)", runErr, decision)
	}

	decision, summary, question, title, err := ParseDecision(resultText)
	if err != nil {
		// Surface resultText (the model's actual, already-extracted natural-
		// language response — e.g. "I need your confirmation before
		// force-pushing...") rather than rawOut (the full JSON envelope:
		// cost, token usage, tool-call metadata, session IDs). This error's
		// %v ends up verbatim in a human-facing MR/PR comment
		// (invokeForEvent's "runner error during %s" pause message) — a raw
		// JSON dump there is exactly the "lumping logs at the user" this
		// avoids. Cap it defensively; a model that goes on at length
		// shouldn't produce an unbounded comment.
		shown := strings.TrimSpace(resultText)
		const maxShown = 2000
		if len(shown) > maxShown {
			shown = shown[:maxShown] + "…"
		}
		return runner.Response{Summary: shown, Tokens: tokens, StartedAt: start, EndedAt: end},
			fmt.Errorf("parse claude output: %w; response was:\n%s", err, shown)
	}
	return runner.Response{
		Decision:          decision,
		Summary:           summary,
		Question:          question,
		Title:             title,
		TitleUpdate:       titleUpdate,
		DescriptionUpdate: descriptionUpdate,
		Tokens:            tokens,
		StartedAt:         start, EndedAt: end,
	}, nil
}

// --- argv construction ---

// BuildArgs composes the argv passed to the claude binary (everything after
// the binary name). extraArgs is prepended, mirroring Runner.ExtraArgs, so
// callers can still override --model etc. by putting their own flag first —
// claude uses last-flag-wins for repeated flags.
//
// Exported so step bodies / tests can assert what argv a given Request
// produces without shelling out.
func BuildArgs(req runner.Request, extraArgs []string) []string {
	args := append([]string{}, extraArgs...)
	args = append(args,
		"-p", BuildPrompt(req),
		"--output-format", "json", // machine-readable output + token counts
		"--dangerously-skip-permissions", // ADR-0006 — yolo inside the worktree
	)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

// --- prompt construction ---

// BuildPrompt composes the full prompt sent to claude. Exported so step
// bodies can assert what their pre-cooked Goal will get wrapped with.
//
// Layout:
//  1. Header lines (Skill / Unit / Worktree, if set)
//  2. The body — req.Goal, taken verbatim
//  3. Event-specific blocks (comment to address — plus the non-author
//     triage guidance when the commenter isn't the Run's author, see
//     ADR-0072 — CI failure to fix, a hook-rejected commit to fix, see
//     ADR-0075, and an unparseable prior response to fix, see ADR-0092)
//  4. The scope-discipline reminder (always appended; flavour depends on
//     whether req.UnitID is set — see ADR-0045)
//  5. The decision-marker protocol instructions (always appended)
func BuildPrompt(req runner.Request) string {
	var b strings.Builder

	if req.SkillCommand != "" {
		fmt.Fprintf(&b, "## Skill\n\n%s\n\n", req.SkillCommand)
	}
	if req.UnitID != "" {
		fmt.Fprintf(&b, "## Unit\n\n%s\n\n", req.UnitID)
	}
	if req.Worktree != "" {
		fmt.Fprintf(&b, "## Worktree\n\n%s\n\n", req.Worktree)
	}

	fmt.Fprintf(&b, "## Task\n\n%s\n\n", req.Goal)

	if req.CommentBody != "" {
		fmt.Fprintf(&b, "## Reviewer feedback to address\n\n%s\n\n", req.CommentBody)
		if !req.CommenterIsAuthor {
			b.WriteString(nonAuthorCommentGuidance)
		}
	}
	if req.CIFailure != "" {
		fmt.Fprintf(&b, "## CI failure to investigate\n\n```\n%s\n```\n\n", req.CIFailure)
	}
	if len(req.ConflictFiles) > 0 {
		fmt.Fprintf(&b, "## Merge conflict to resolve\n\nThis branch has a merge conflict with base in the following files:\n\n```\n%s\n```\n\n", strings.Join(req.ConflictFiles, "\n"))
	}
	if req.HookFailure != "" {
		fmt.Fprintf(&b, "## Commit rejected by pre-commit hook\n\n```\n%s\n```\n\n%s", req.HookFailure, hookFailureGuidance)
	}
	if req.ParseFailure != "" {
		fmt.Fprintf(&b, "## Previous response could not be parsed\n\n```\n%s\n```\n\n%s", req.ParseFailure, parseFailureGuidance)
	}

	// UnitID is only set for unit-execution invocations (see work() in
	// refactorsweep/workflow.go); planning invocations leave it empty.
	if req.UnitID == "" {
		b.WriteString(planningScopeDiscipline)
	} else {
		b.WriteString(unitScopeDiscipline)
		if req.TitleConvention != "" {
			fmt.Fprintf(&b, "## MR title convention\n\n%s\n\nWhen you finish with Decision=done, phrase the MR title per this convention and put it after \"done: \" in the decision marker, e.g. `<syntropy-decision>done: <title></syntropy-decision>`.\n\n", req.TitleConvention)
		}
	}
	b.WriteString(decisionProtocol)
	return b.String()
}

// nonAuthorCommentGuidance follows the reviewer-feedback block when the
// comment came from someone other than the Run's author. ADR-0072 extends
// the author-vs-reviewer privilege model (ADR-0017) from control commands
// to review comments: a reviewer can get an objective defect fixed
// directly, but a change of solution or direction needs the author's
// sign-off, so the runner routes those to Decision=Ask instead of
// implementing them.
const nonAuthorCommentGuidance = `This comment is from a reviewer, not the Run's author. Before acting on
it, classify it:

- **Objective defect** — it points out a correctness problem: broken
  logic, a bug, a failing or missing test, a crash, a security flaw, or
  code that plainly doesn't do what the task requires. Implement the fix
  as usual.
- **Solution steering** — it suggests a different approach, design,
  structure, naming, scope, or a style preference; the current code is
  not objectively wrong. Do NOT implement it. Finish with Decision=Ask
  and a question that summarises the reviewer's suggestion so the Run's
  author can approve or decline it.

If you are unsure which it is, treat it as solution steering and ask.

`

// hookFailureGuidance follows the hook-rejection block. ADR-0075: a commit
// rejected by the target repo's own pre-commit hooks (lint, formatting,
// secret scanning, file-size caps, etc.) is something the runner caused and
// can usually fix itself, so this asks it to self-correct and retry rather
// than treat the rejection as a dead end.
const hookFailureGuidance = `Your last commit attempt in this worktree was rejected by the repo's own
pre-commit hooks — the output above is what the hook reported. Read it,
fix whatever it's complaining about (formatting, lint, secrets, file
size, etc.), and commit again. Do not use ` + "`--no-verify`" + ` or any other
way of bypassing the hook.

`

// parseFailureGuidance follows the parse-failure block. ADR-0092: a
// response with no valid decision marker (malformed, missing, or otherwise
// unparseable) means syntropy couldn't tell what you decided, so this asks
// you to end your response with a single, correctly-formed
// <syntropy-decision> tag rather than treating the failure as a dead end.
const parseFailureGuidance = `Your previous response could not be parsed — the error above is what
syntropy reported. Your response must end with exactly one
<syntropy-decision>...</syntropy-decision> tag, alone on its own line, using
one of the documented forms (continue, done, ask: <question>, fail: <reason>,
nochange, retryci: <reason>). Finish your work and emit a single well-formed
tag.

`

// planningScopeDiscipline is the scope-discipline flavour for planning
// invocations (req.UnitID == ""). See ADR-0045: a planner left to its own
// devices tends toward a handful of large increments, which is exactly the
// shape that produces multi-concern MRs downstream. This instruction pushes
// the bias the other way, toward more, smaller increments.
const planningScopeDiscipline = `## Scope discipline

Bias toward more, smaller increments rather than fewer, larger ones. Each
increment should cover a single concern that can land as its own small MR.
If a candidate increment would touch more than one concern, split it into
separate increments instead of bundling them together.

`

// unitScopeDiscipline is the scope-discipline flavour for unit-execution
// invocations (req.UnitID != ""). See ADR-0045: units kept ballooning into
// multi-concern MRs because nothing in the prompt told the runner to stay
// narrow — the model reasonably filled the silence by doing as much as it
// could reach.
const unitScopeDiscipline = `## Scope discipline

Keep this unit small and narrowly scoped to the task above. Do only what
is asked — do not bundle in unrelated fixes, refactors, or improvements
you happen to notice along the way. If you spot other work worth doing,
mention it in your summary instead of doing it now; a future unit can
pick it up.

## Git: never push, never rewrite history

The harness owns every push, using a plain (non-force) ` + "`git push`" + `
— never run ` + "`git push`" + ` yourself, in any form, including
` + "`--force`" + ` or ` + "`--force-with-lease`" + `. After you finish, the
harness commits and pushes whatever you've left in the worktree; you
don't need to (and must not) push it yourself.

For the same reason, never rewrite existing commits — no
` + "`git commit --amend`" + `, no rebase, no ` + "`git reset`" + ` past a
commit that's already landed. Because the harness's own push is a plain,
non-force push, any local history rewrite makes that push fail as
"non-fast-forward". If an earlier commit's message or content needs
fixing, make a **new** commit that fixes it forward instead — that's
always safe to push normally.

`

// decisionProtocol is the marker-based signalling everflow uses to read
// claude's outcome. Appended to every prompt.
//
// Why prompt-marker over claude's --output-format json: claude's JSON
// surfaces the message stream, not a domain decision. We'd still need a
// way to express "Continue / Done / Ask / Fail / NoChange" — a tail-line
// marker is the smallest contract that does it without depending on
// tool-use registration.
const decisionProtocol = `## How to finish

After completing your work (or deciding you can't), end your response
with EXACTLY ONE of these tags on its own line:

- ` + "`<syntropy-decision>continue</syntropy-decision>`" + ` — during planning, there's more to do (signals the next increment). During a work turn on a unit, this also means: this unit turned out to be bigger than one turn, you shipped a real partial slice of it, and there's a well-defined remainder left. State the remainder clearly in your summary — what's done and what's left — so the planner can schedule it as a follow-on increment instead of assuming the unit is finished. Don't use it to avoid finishing small units; use it only when the unit genuinely doesn't fit in one turn.
- ` + "`<syntropy-decision>done</syntropy-decision>`" + ` — task is complete
- ` + "`<syntropy-decision>ask: <one-line question></syntropy-decision>`" + ` — you need the human's input before proceeding
- ` + "`<syntropy-decision>fail: <one-line reason></syntropy-decision>`" + ` — you cannot proceed
- ` + "`<syntropy-decision>nochange</syntropy-decision>`" + ` — nothing to do (e.g. the change was already applied)
- ` + "`<syntropy-decision>retryci: <one-line reason></syntropy-decision>`" + ` — only for a CI failure to investigate: the failure looks transient or infra-related (flaky test, runner/network hiccup, timeout unrelated to your change), not a real problem with the code. Do not make any code changes when using this decision.

When investigating a CI failure, choose between these four outcomes:
- retryci — the failure is transient/infra noise; re-running without changes would likely pass.
- continue / done — the failure points to a real bug in the code; fix it the same way you would for an explicit human instruction.
- ask — the fix requires an ambiguous behavior change (more than one reasonable way to make it pass, with different user-visible outcomes); pause and ask before choosing one.

The text before the tag becomes the recorded Summary; syntropy strips
the tag itself from the output. Only the LAST occurrence of the tag in
your response is read, so feel free to write naturally up to that point.

This tag is mandatory on every turn, with no exceptions — including a
turn where a tool call you needed gets denied or blocked partway
through (e.g. a permission rule refuses a command you tried to run).
When that happens, do not just stop and describe the situation in
plain text — that produces no decision and leaves the run stuck with
nothing useful recorded. Instead, end with
` + "`<syntropy-decision>ask: <one-line question></syntropy-decision>`" + `
describing exactly what you needed to do and why it was blocked, so a
human can unblock or redirect you.

If the MR's title or description is stale or wrong, fix it yourself instead
of asking a human to hand-edit it: emit
` + "`<syntropy-title-update>new title</syntropy-title-update>`" + ` and/or
` + "`<syntropy-description-update>new description</syntropy-description-update>`" + `
on their own line(s), anywhere in your response, on any turn (not just when
you finish). The description tag's content may span multiple lines. Only
the last occurrence of each tag is used. These are independent of the
` + "`<syntropy-decision>`" + ` marker — include them alongside it, not
instead of it.
`

// --- decision parsing ---

// decisionRE matches the decision marker. Per the protocol (ADR-0027), the
// real marker must sit on its own line, so the pattern requires only
// leading/trailing horizontal whitespace around the tag on that line. This
// stops an incidental mention of the tag inside prose (e.g. the model
// quoting the protocol instructions back, or discussing the tag in a
// sentence) from being mistaken for a genuine marker: such mentions share a
// line with other text and so never match. Among genuine own-line markers,
// we still extract the LAST one (sometimes the model echoes the protocol as
// its own standalone line while reasoning before producing the real one).
//
// Accepts both <syntropy-decision> (current) and <everflow-decision>
// (pre-rename) tag names — see ADR-0057. Go's RE2 engine has no
// backreferences, so this can't enforce the open/close tag names match
// each other; in practice a model never produces a mismatched pair, so
// this is a pragmatic tradeoff, not a real gap.
var decisionRE = regexp.MustCompile(`(?m)^[ \t]*<(?:syntropy|everflow)-decision>\s*(.*?)\s*</(?:syntropy|everflow)-decision>[ \t]*$`)

// ErrNoDecisionMarker is returned by ParseDecision when claude's response
// contains no <syntropy-decision>...</syntropy-decision> (or legacy
// <everflow-decision>) tag. Treated as a runner-level failure by the
// workflow's step bodies (they see the error and pause / fail accordingly).
var ErrNoDecisionMarker = errors.New("claude: no <syntropy-decision> marker in response")

// ParseDecision extracts the Decision + Summary + Question + Title from a
// claude response. The Summary is everything before the last marker
// (trimmed); the Question is set only when the decision is "ask"; the Title
// is set only when the decision is "done" and the model included text after
// "done:" (per the MR title convention instruction — see BuildPrompt).
//
// Exported for tests + for future debugging utilities.
func ParseDecision(out string) (decision runner.Decision, summary, question, title string, err error) {
	matches := decisionRE.FindAllStringSubmatchIndex(out, -1)
	if len(matches) == 0 {
		return runner.DecisionUnknown, strings.TrimSpace(out), "", "", ErrNoDecisionMarker
	}
	last := matches[len(matches)-1]
	inner := strings.TrimSpace(out[last[2]:last[3]])
	prefix := strings.TrimSpace(out[:last[0]])

	// Inner can be "<verb>" or "<verb>: <text>"
	verb, rest := splitVerb(inner)
	switch verb {
	case "continue":
		return runner.DecisionContinue, prefix, "", "", nil
	case "done":
		return runner.DecisionDone, prefix, "", rest, nil
	case "ask":
		summary = prefix
		question = rest
		if question == "" {
			question = "(no question text)"
		}
		return runner.DecisionAsk, summary, question, "", nil
	case "fail":
		summary = prefix
		if rest != "" {
			// Surface the reason in Summary too so it ends up in the
			// MR comment + audit even if Question wasn't set.
			summary = strings.TrimSpace(summary + "\n\nReason: " + rest)
		}
		return runner.DecisionFail, summary, "", "", nil
	case "nochange":
		return runner.DecisionNoChange, prefix, "", "", nil
	case "retryci":
		summary = prefix
		if rest != "" {
			summary = strings.TrimSpace(summary + "\n\nReason: " + rest)
		}
		return runner.DecisionRetryCI, summary, "", "", nil
	default:
		return runner.DecisionUnknown, prefix, "", "",
			fmt.Errorf("claude: unrecognised decision verb %q", verb)
	}
}

// splitVerb breaks "<verb>" or "<verb>: <rest>" into (verb, rest). Verb
// is lowercased and trimmed; rest is everything after the first colon
// (also trimmed). Both can be empty.
func splitVerb(inner string) (verb, rest string) {
	idx := strings.IndexByte(inner, ':')
	if idx == -1 {
		return strings.ToLower(strings.TrimSpace(inner)), ""
	}
	return strings.ToLower(strings.TrimSpace(inner[:idx])),
		strings.TrimSpace(inner[idx+1:])
}

// --- title/description update parsing (ADR-0091) ---
//
// These are separate, independent tags from <syntropy-decision>: they can
// appear on any turn (not only on Decision=Done), so the runner can request
// an MR title/description fix on the spot instead of waiting until it
// finishes. Parsing is deliberately kept as its own pair of regexes/
// functions rather than folded into ParseDecision or a shared "generic tag"
// helper — the two protocols have different lifetimes and this keeps each
// one simple to reason about independently.

// titleUpdateRE matches a <syntropy-title-update> tag. Mirrors decisionRE's
// own-line-anchored style (see its comment) so an incidental mention of the
// tag in prose can't hijack it; content is single-line, matching how MR
// titles are used elsewhere (e.g. the "done: <title>" mechanism).
var titleUpdateRE = regexp.MustCompile(`(?m)^[ \t]*<syntropy-title-update>\s*(.*?)\s*</syntropy-title-update>[ \t]*$`)

// descriptionUpdateRE mirrors titleUpdateRE but allows the captured content
// to span multiple lines (an MR description is rarely a single line) while
// still requiring the opening/closing tags themselves to each sit alone on
// their own line.
var descriptionUpdateRE = regexp.MustCompile(`(?ms)^[ \t]*<syntropy-description-update>\s*(.*?)\s*</syntropy-description-update>[ \t]*$`)

// ParseTitleUpdate returns the content of the LAST <syntropy-title-update>
// tag in out, or "" if none is present. Last-match-wins, same rationale as
// ParseDecision: the model may echo the tag while reasoning before the real
// one.
func ParseTitleUpdate(out string) string {
	return lastTagMatch(titleUpdateRE, out)
}

// ParseDescriptionUpdate is ParseTitleUpdate's sibling for
// <syntropy-description-update>.
func ParseDescriptionUpdate(out string) string {
	return lastTagMatch(descriptionUpdateRE, out)
}

// lastTagMatch returns the trimmed capture group of the last match of re in
// out, or "" if re has no match.
func lastTagMatch(re *regexp.Regexp, out string) string {
	matches := re.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return ""
	}
	return strings.TrimSpace(matches[len(matches)-1][1])
}
