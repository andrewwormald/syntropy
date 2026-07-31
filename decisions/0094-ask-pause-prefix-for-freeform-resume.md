# ADR-0094: `askPausePrefix` marks `DecisionAsk` pauses for freeform resume

**Status**: Accepted
**Date**: 2026-07-31

## Context

`resume()`'s Paused gate (`internal/refactorsweep/workflow.go`) is
deliberately blunt: while `r.Status == StatusPaused`, only control
commands (`/syntropy resume`, `/syntropy retry`, ...) and MR lifecycle
truth progress the Run; every other inbound event — including an
ordinary reply comment — is noted and dropped. That blanket rule is
correct for most pause reasons: a git push failure, a hook rejection, a
filter pause. None of those are *questions*, so there is nothing for a
plain-language reply to answer, and letting arbitrary conversation
un-pause the Run would be an accident waiting to happen.

`DecisionAsk` is different. Both of its call sites — `discoverSpec`
(the planner asking a clarifying question before planning the next
increment) and `invokeForEvent` (the runner asking the author to
approve or decline reviewer steering, ADR-0072) — park the Run with a
question in `PauseReason` and tell the author to reply. The reply *is*
the answer to that specific question. Requiring the author to first
type `/syntropy resume` and then repeat themselves is one more hoop
than the interaction needs, and worse: if a future change makes
freeform replies resume the Run generically (e.g. by having some
decision branch downstream unconditionally set status away from
`Paused`), that would resume *every* kind of pause on an unrelated
freeform comment, not just `DecisionAsk` — silently un-pausing a Run
that's actually stuck on a git failure or a hook rejection just because
someone left a comment.

The fix has to be: a freeform reply should resume the Run **only**
when the Run is paused specifically because of `DecisionAsk`, decided
deliberately by checking what the Run is paused about — never as a side
effect of a decision branch that overwrites status regardless of why it
was set. That requires being able to tell a `DecisionAsk` pause apart
from every other `PauseReason` at the point the Paused gate makes its
call.

`providerAuthPausePrefix` (ADR-0038) already establishes the pattern
for exactly this need: prefix `PauseReason` with a fixed marker when
parking for a specific reason, then `strings.HasPrefix` on it later to
recognise that pause type regardless of the (human-readable, free-text)
remainder of the string.

## Decision

Introduce `askPausePrefix = "ask: "` alongside `providerAuthPausePrefix`
in `internal/refactorsweep/workflow.go`, and prefix `PauseReason` with
it at both `DecisionAsk` call sites:

- `discoverSpec`: `r.Object.PauseReason = askPausePrefix + resp.Question`
- `invokeForEvent`: `r.Object.PauseReason = askPausePrefix + resp.Question`

This increment only introduces the marker and applies it consistently
at both sites — it does not yet change `resume()`'s Paused-gate
behavior. That follow-on work (letting a freeform `EventNoteAdded`
reply resume the Run when `strings.HasPrefix(PauseReason,
askPausePrefix)` holds, while leaving every other `PauseReason`
untouched) is scoped separately.

## Alternatives considered

- **A new field (e.g. `AskPaused bool`) instead of a string prefix.**
  Rejected: `PauseReason` is already the single place pause state and
  its cause live together (see `providerAuthPausePrefix`), and
  `cmdStatus` already renders it verbatim to the author — a prefix
  keeps that rendering intact (`"ask: <question>"` still reads
  naturally) while adding a machine-checkable marker, with no new state
  to keep in sync.
- **Distinguish by which code path set `PauseReason` rather than its
  content.** Rejected: `resume()`'s Paused gate only has `r.Object`
  to inspect at the point it decides whether to let an event through;
  it doesn't know which prior call produced the current pause. A
  content marker is the only way to make that decision at the gate
  without threading extra state through the Run object.

## Consequences

- Both `DecisionAsk` pauses now read `"ask: <question>"` in
  `PauseReason` (previously `"planner asks: <question>"` from
  `discoverSpec` and a bare `<question>` from `invokeForEvent` — two
  different, inconsistent shapes). `cmdStatus`'s rendering of
  `PauseReason` is unaffected in kind, only in the exact wording of
  these two cases.
- No behavior change yet: `resume()`'s Paused gate still requires a
  control command to progress a paused Run. The marker is inert until
  a follow-on increment reads it.
- Any future pause reason that should behave like `DecisionAsk` (freeform-
  resumable) should reuse `askPausePrefix` rather than inventing another
  marker; anything that should behave like every other pause (control-
  command-only) needs no prefix at all, matching today's default.

## Tests

No behavior changes in this increment; existing
`internal/refactorsweep/workflow_test.go` coverage of `discoverSpec`'s
and `invokeForEvent`'s `DecisionAsk` branches continues to pass
unchanged against the new `PauseReason` text.
