package openhands

import (
	"fmt"
	"strings"

	"github.com/andrewwormald/syntropy/internal/runner"
)

// BuildPrompt composes the prompt sent to the openhands-agent-server,
// mirroring claude.BuildPrompt's section structure (ADR-0027) so both
// runners share the same decision-marker protocol and event-specific
// blocks. Kept as its own package-local copy rather than calling
// claude.BuildPrompt directly: OpenHands' default agent may have broader
// tool access than Claude Code's --dangerously-skip-permissions sandbox
// assumes, so this adds an explicit instruction — never push, never call
// git or the provider (GitHub/GitLab) API directly — that can't be assumed
// to transfer silently from the Claude Code prompt.
//
// Layout:
//  1. Header lines (Skill / Unit / Worktree, if set)
//  2. The body — req.Goal, taken verbatim
//  3. Event-specific blocks (comment to address — plus the non-author
//     triage guidance when the commenter isn't the Run's author, CI
//     failure to fix, a hook-rejected commit to fix, and an unparseable
//     prior response to fix)
//  4. The scope-discipline reminder, plus the OpenHands-specific
//     never-push/never-call-provider-API instruction
//  5. The decision-marker protocol instructions
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

	if req.UnitID == "" {
		b.WriteString(planningScopeDiscipline)
	} else {
		b.WriteString(unitScopeDiscipline)
		b.WriteString(noSelfPushOrProviderAPIGuidance)
		if req.TitleConvention != "" {
			fmt.Fprintf(&b, "## MR title convention\n\n%s\n\nWhen you finish with Decision=done, phrase the MR title per this convention and put it after \"done: \" in the decision marker, e.g. `<syntropy-decision>done: <title></syntropy-decision>`.\n\n", req.TitleConvention)
		}
	}
	b.WriteString(decisionProtocol)
	return b.String()
}

// nonAuthorCommentGuidance mirrors claude.nonAuthorCommentGuidance
// (ADR-0072): a reviewer can get an objective defect fixed directly, but a
// change of solution or direction needs the author's sign-off.
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

// hookFailureGuidance mirrors claude.hookFailureGuidance (ADR-0075).
const hookFailureGuidance = `Your last commit attempt in this worktree was rejected by the repo's own
pre-commit hooks — the output above is what the hook reported. Read it,
fix whatever it's complaining about (formatting, lint, secrets, file
size, etc.), and commit again. Do not use ` + "`--no-verify`" + ` or any other
way of bypassing the hook.

`

// parseFailureGuidance mirrors claude.parseFailureGuidance (ADR-0092).
const parseFailureGuidance = `Your previous response could not be parsed — the error above is what
syntropy reported. Your response must end with exactly one
<syntropy-decision>...</syntropy-decision> tag, alone on its own line, using
one of the documented forms (continue, done, ask: <question>, fail: <reason>,
nochange, retryci: <reason>). Finish your work and emit a single well-formed
tag.

`

// planningScopeDiscipline mirrors claude.planningScopeDiscipline (ADR-0045).
const planningScopeDiscipline = `## Scope discipline

Bias toward more, smaller increments rather than fewer, larger ones. Each
increment should cover a single concern that can land as its own small MR.
If a candidate increment would touch more than one concern, split it into
separate increments instead of bundling them together.

`

// unitScopeDiscipline mirrors claude.unitScopeDiscipline (ADR-0045).
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

// noSelfPushOrProviderAPIGuidance is OpenHands-specific: it does not exist
// in claude.BuildPrompt because Claude Code's --dangerously-skip-permissions
// sandbox (ADR-0006) is the boundary syntropy already relies on there. The
// Agent Server's confirmation policy is forced to auto-approve (ADR-0112
// §3), which grants whatever tool access the underlying agent config allows
// — a superset that isn't guaranteed to match Claude Code's, so this spells
// out the same "never push" boundary in terms broad enough to also cover
// calling the provider (GitHub/GitLab) API directly, e.g. merging or
// approving its own MR/PR — an action a shell-out to `git push` wouldn't
// perform but a direct API call could.
const noSelfPushOrProviderAPIGuidance = `## Never push, never call git or the provider API directly

Beyond the git restrictions above: never call the GitHub/GitLab API (or any
other provider API) directly — not to push, merge, approve, or otherwise
mutate the MR/PR or the remote repository. The harness owns every
interaction with the provider and with the remote; your job ends at leaving
committed changes in the local worktree.

`

// decisionProtocol mirrors claude.decisionProtocol.
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
