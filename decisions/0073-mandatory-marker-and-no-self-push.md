# ADR-0073: Mandatory decision marker on every turn; the runner never pushes or rewrites history

**Status**: Accepted
**Date**: 2026-07-27

## Context

Found live, in a single incident: during an `address_comment` turn, the
runner decided to amend a commit's message and force-push the result —
`git push --force-with-lease origin syntropy/bbc8ff63/increment-4` — as
an ad-hoc Bash tool call. The target repo's own `.claude/settings.json`
has `git push --force-with-lease` in its `ask` list, and Claude Code
correctly enforces that boundary even under
`--dangerously-skip-permissions` (a deliberate, intentional behavior —
`ask`/`deny` rules aren't something the CLI flag overrides, since doing
so would defeat the point of a repo configuring them at all). The push
was denied.

Rather than continuing the turn with a `<syntropy-decision>` marker
(e.g. `ask:` to relay the same confirmation request through the proper
channel), the model just wrote plain English — "I need your confirmation
before force-pushing... OK to proceed?" — and stopped. `ParseDecision`
correctly found no marker and the turn failed, but the resulting pause
comment dumped the *entire* raw JSON envelope (cost, token usage,
tool-call metadata, session IDs, and the actual question all mixed
together) verbatim into the MR — exactly the "lumping logs at the user"
this ADR's fix avoids.

Separately, `syntropy`'s own architecture already treats git write
operations as something *the harness* owns, not the LLM inside the
worktree (ADR-0006's whole premise for `--dangerously-skip-permissions`
being tolerable at all is that the harness — not the model — is what
actually lands work). The model taking its own independent, unreviewed
git action (here, rewriting history and force-pushing) is a violation of
that boundary regardless of whether the specific repo's permission
config happened to catch it this time — a differently-configured repo
without that `ask` rule would have let it through.

## Decision

Three changes, all in `internal/runner/claude/claude.go`:

1. **`decisionProtocol`** now states plainly that the
   `<syntropy-decision>` tag is mandatory on *every* turn, with no
   exception for a turn where a needed tool call gets denied or blocked
   partway through — the model must still end with a real marker
   (`ask:` is the natural fit for exactly this situation) instead of
   trailing off in plain text with nothing recorded.
2. **`unitScopeDiscipline`** now explicitly forbids the model from ever
   running `git push` itself (plain, `--force`, or
   `--force-with-lease`) — the harness owns every push — **and** from
   rewriting existing commits at all (no `commit --amend`, no rebase, no
   `reset` past a landed commit). The second half exists because the
   harness's own push (`internal/git.ExecGit.Push`) is a plain,
   non-force `git push -u origin <branch>`; if the model rewrites local
   history (even without trying to push it itself), that push then fails
   as non-fast-forward — found live, immediately, when this ADR's first
   draft only forbade the push and not the rewrite. If an earlier
   commit needs fixing, the guidance is to make a new commit that fixes
   it forward, which is always safe to push normally.
3. **The no-marker error path** in `Run()` now surfaces `resultText` (the
   model's actual extracted natural-language response) instead of
   `rawOut` (the full JSON envelope) in both the returned error and
   `Response.Summary` — this is what ultimately lands in the
   human-facing MR/PR pause comment (`invokeForEvent`'s "runner error
   during %s" message), so this is the actual fix for "lumping logs at
   the user," independent of whether the marker-discipline fix above
   eliminates most instances of hitting this path at all. Capped at
   2000 characters defensively.

## Alternatives considered

- **Make the harness's own `Push` use `--force-with-lease`**, so an
  amended/rebased local history could still be pushed successfully
  (accommodating rather than forbidding model-side history rewrites).
  Rejected: force-pushing is exactly the class of action this whole
  incident was about avoiding an LLM taking unsupervised — even if the
  harness's own controlled use of it is arguably safer (it owns the
  branch exclusively), forbidding history rewrites entirely at the
  source is simpler, has no edge cases, and doesn't require the harness
  to reason about whether a given force-push is actually safe.
- **Heuristically infer an implicit `ask` decision from unmarked prose**
  (e.g. text ending in "?" or containing "confirm"). Rejected in favor
  of fixing the prompt instead — a heuristic is guesswork bolted onto a
  parser that's supposed to be a strict, unambiguous contract; making
  the *model* reliably honor the contract is the more direct fix, and
  the clean-error-message change (point 3) means even a rare miss here
  no longer produces a raw JSON dump.

## Consequences

- A denied tool call mid-turn should now surface as a clean `DecisionAsk`
  pause instead of a raw parse-error pause — same outcome for the human
  (still asked), much better presentation.
- The model is now instructed to never touch history at all, which is a
  stricter constraint than "just don't push it" — a self-committing
  runner (already an accommodated pattern per existing `HasWorkBeyondBase`
  tests) is unaffected, since making new commits was never the
  restricted case; only rewriting *existing* ones is.
- This is prompt guidance, not a hard enforcement mechanism — a model
  that ignores it could still attempt a self-push or a rewrite. The
  actual hard backstop remains whatever the target repo's own
  `.claude/settings.json` `ask`/`deny` rules allow, which is outside
  `syntropy`'s control. If self-push attempts recur despite this
  guidance, the next escalation would be `syntropy` itself writing a
  `deny` rule into the worktree's local Claude Code settings at setup
  time — not attempted here, since prompt guidance is the lower-risk
  first step and this incident doesn't yet show it's insufficient.
