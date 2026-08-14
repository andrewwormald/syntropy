# ADR-0111: Repeated-pause escalation stops re-invoking the runner

**Status**: Accepted
**Date**: 2026-08-14

## Context

The same live incident that motivated ADR-0109/ADR-0110 (hardening
`syntropy adopt`) also surfaced a token-burn problem in the ordinary
comment-driven loop (ADR-0034). A non-Ask pause — a hook rejection, a git
failure, a filter pause — still lets a freeform `EventNoteAdded` reach
`invokeForEvent` (`internal/refactorsweep/workflow.go`'s `resume()`,
"the resulting status is forced back to Paused: only an explicit control
command may clear a non-Ask pause"). That's deliberate: the reply may carry
useful context. But it has no upper bound. If a human keeps replying on a
stuck MR without actually addressing the underlying cause, every reply
re-invokes the runner and lands back on the identical `PauseReason` — a
full subagent turn, burning real tokens, with the Run provably no closer to
resolution than before the call.

`CIRetryCounts` (ADR-0068) and `maxHookRetries`/`maxParseRetries`
(ADR-0075/0092) already cap consecutive *retries within a bounded loop*.
None of them cap consecutive *identical pauses re-entered from outside*,
across arbitrarily many separate events — the gap this ADR closes.

## Decision

`AgentState` gets `IdenticalPauseStreak int`. In `resume()`'s Paused gate,
before invoking the runner on a non-Ask pause, the code compares
`PauseReason` before and after the call:

- Unchanged → `IdenticalPauseStreak++`. On the turn it first reaches
  `maxIdenticalPauseStreak` (3), a one-time bot comment tells the human the
  loop is being cut and to use a `/syntropy` control command.
- Changed → `IdenticalPauseStreak` resets to 0 (the runner made real
  progress or hit a genuinely different problem).

Once `IdenticalPauseStreak >= maxIdenticalPauseStreak`, further freeform
notes on that pause return `StatusPaused` immediately without calling
`invokeForEvent` at all — no more tokens spent until an explicit control
command (`/syntropy resume`, `/syntropy retry`, `/syntropy skip`, ...)
clears `PauseReason` and the streak together (`cmdPause`, `cmdResume`,
`cmdRetry` all reset `IdenticalPauseStreak` alongside `PauseReason`, so a
fresh pause — or a legitimately retried one — always starts counting from
zero).

Ask-pauses (`askPausePrefix`, ADR-0094) are untouched: they're already
gated on getting an actual answer, not a general freeform reply, so the
runaway-reply failure mode this ADR targets doesn't apply there.

## Alternatives considered

- **Time-boxed cooldown between runner invocations on the same pause**
  (mirroring ADR-0061's reconciler cooldown) — caps invocation *rate*, not
  count, so a determined human (or bot) drip-feeding comments every
  cooldown window still burns tokens forever. A count-based cap on
  *identical* outcomes directly targets the actual failure mode: no
  progress, not just fast progress.
- **Hash/normalize `PauseReason` before comparing**, to catch pauses whose
  wording drifts slightly between turns (e.g. a stack trace line number)
  while the underlying cause is the same — more precise, but adds
  complexity for a case not observed in the live incident. Exact-string
  comparison is the simplest thing that closes the observed gap; can be
  revisited if a real report shows near-identical-but-not-exact reasons
  looping.
- **Escalate by transitioning to a terminal state (Failed) instead of
  freezing further invocations** — too aggressive: the MR and Run are
  still perfectly recoverable via a control command, so failing the Run
  outright would force a human to re-trigger from scratch for what's
  often a one-line `/syntropy skip`.

## Consequences

- A Run stuck on a truly unresolvable non-Ask pause now costs at most
  `maxIdenticalPauseStreak` extra subagent turns beyond the original
  pause, not one per human reply, indefinitely.
- The one-time escalation comment is the only new provider-visible
  behavior; everything else is silent internal bookkeeping on
  `AgentState`.
- `cmdStatus`'s existing "Pause reason" line could additionally surface
  `IdenticalPauseStreak` for visibility — left out of this change as
  cosmetic; a natural follow-up if reviewers want it.
