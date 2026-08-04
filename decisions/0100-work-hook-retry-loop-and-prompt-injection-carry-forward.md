# ADR-0100: `work()`'s pre-MR commit step gets its own hook-retry loop, and a fallback pause carries the rejection into `PromptInjection`

**Status**: Accepted
**Date**: 2026-08-04

## Context

ADR-0075/ADR-0078 gave `invokeForEvent` a bounded in-turn retry loop for
pre-commit hook rejections: `req.HookFailure` is set from the hook's
output and the runner gets up to `maxHookRetries` more turns to fix its
own commit before the Run pauses for a human. `work()`'s pre-MR commit
step (`internal/refactorsweep/workflow.go`) had no equivalent — a hook
rejection there paused unconditionally on the very first attempt, and the
pause message discarded the hook's output entirely. Worse, because
`work()`'s pause is resumed by a human's manual `/syntropy` reply rather
than another automatic event, the rejection context wasn't available to
carry forward even if the human retried right away: the next runner turn
started cold and was free to repeat the exact same mistake the hook had
just rejected.

## Decision

Two changes, split across the increments that implemented this unit:

1. Give `work()`'s Done/Continue commit step its own `hookRetries` counter,
   mirroring `invokeForEvent`'s loop exactly: on a `*git.HookRejectionError`
   with `hookRetries < maxHookRetries`, set `req.HookFailure` to the hook's
   output, increment the counter, and loop the runner invocation again
   in-turn instead of pausing immediately.

2. When the retry budget is exhausted and `work()` falls back to
   `pauseWork`, also stash the hook's output in `r.Object.PromptInjection`
   (`internal/refactorsweep/workflow.go:844`) rather than only mentioning it
   in the pause message. `PromptInjection` is the same single-use slot
   `/syntropy prompt <text>` writes to
   (`internal/refactorsweep/controls.go:302`), consumed by the next runner
   call's `req.Goal` prefix at line 722. A human's `/syntropy retry` (or
   any other resume) after this pause therefore carries the last hook
   failure forward into the next turn's goal automatically, instead of
   silently dropping it and risking the identical rejection repeating.

Using `PromptInjection` instead of adding a dedicated field keeps this
consistent with the existing single-use-injection pattern rather than
introducing a second, parallel carry-forward mechanism; it's overwritten
by a subsequent `/syntropy prompt` exactly as any other injection would
be, which is the correct behavior — a human's explicit instruction should
take priority over the stashed hook output.

## Consequences

- A hook rejection in `work()`'s pre-MR phase now gets the same
  up-to-`maxHookRetries` in-turn self-correction chance `invokeForEvent`
  has had since ADR-0078, instead of pausing on the first rejection.
- If the retry budget is exhausted, the human resuming the paused Run gets
  the hook's own complaint fed into the next turn's goal for free — no
  need to paste it back in manually via `/syntropy prompt`.
- `PromptInjection` now has two producers in `work()`'s commit path (the
  hook-rejection fallback here, and `/syntropy prompt` from a human) that
  share the same single-use consume-on-read slot; whichever writes last
  before the next runner call wins, which is the desired precedence.
