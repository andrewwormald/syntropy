# ADR-0078: Wire pre-commit hook rejections into `invokeForEvent` with a bounded retry loop

**Status**: Accepted
**Date**: 2026-07-27

## Context

ADR-0075 threaded `Request.HookFailure` through to `claude.BuildPrompt` but
deliberately left it unpopulated: no caller set it, so the prompt guidance
was inert. `invokeForEvent`'s Done/Continue commit-failure path
(`internal/refactorsweep/workflow.go`) still treated a `git.Commit` error
during `address_comment`/`fix_ci` as one undifferentiated failure and
paused the Run for a human, regardless of whether the target repo's own
pre-commit hook rejected it (fixable by the runner) or something else went
wrong (not fixable by another turn).

## Decision

Give `git.Commit` (`internal/git/git.go`) a way to say *which* kind of
failure this was: `HookRejectionError{Output string}`, returned only from
the final `git commit` invocation once staging has already confirmed there
is something to commit. `Output` is the raw stdout+stderr of that
invocation — captured directly (not via the existing `run`/`runOut`
helpers, whose error path discards stdout) since hook explanations (a
gofmt diff, a lint message) commonly print there rather than to stderr.

In `invokeForEvent`, wrap the runner invocation and its decision handling
in a loop keyed by a per-call `hookRetries` counter. When the Done/Continue
branch's `git.Commit` fails with a `*git.HookRejectionError` and
`hookRetries < maxHookRetries` (2), set `req.HookFailure` to the hook's
output, increment the counter, and loop back to invoke the runner again —
giving it the rejection plus ADR-0075's fixed guidance and one more turn to
self-correct. Any other decision (`NoChange`/`Ask`/`Fail`/`RetryCI`) or a
non-hook commit error still returns immediately exactly as before; only the
hook-rejection path loops.

Exceeding `maxHookRetries` falls back to the pre-existing pause, but with a
message naming the hook specifically (rather than the generic "git Commit
failed") and including its output, so the human doesn't have to go dig
through the worktree to see what the hook complained about.

Unlike `CIRetryCounts` (ADR-0069), the counter here is a plain local
variable, not persisted on `AgentState`. Every retry re-invokes the runner
synchronously within the same `invokeForEvent` call — there is no
daemon-restart or subsequent-event gap to survive, so there's nothing to
gain from threading a new field through the durable object, and doing so
would let a hook-rejection streak leak across unrelated future events for
the same unit.

## Consequences

- A hook rejection during `address_comment`/`fix_ci` now gets the runner
  up to 2 extra turns to fix its own commit before a human is paged,
  instead of pausing on the very first rejection.
- The prompt guidance ADR-0075 added (`hookFailureGuidance`) is exercised
  for the first time by a real caller.
- `HookRejectionError` is specific to the final `git commit` step; other
  `Commit` failures (e.g. `HasChanges`/`add`/`ls-files` errors surfaced
  earlier in the same call) are unaffected and still pause immediately, as
  they aren't something a runner retry can plausibly fix.
