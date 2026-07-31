# ADR-0095: The Paused gate lets `askPausePrefix` resume, forces every other pause to stay put

**Status**: Accepted
**Date**: 2026-07-31

## Context

ADR-0094 introduced `askPausePrefix` but deliberately didn't change
`resume()`'s Paused gate — it still dropped every non-control event
outright, `DecisionAsk` pauses included. That left two problems:

1. Replying in plain language to the planner's or runner's clarifying
   question did nothing at all: the reply was noted and discarded, so
   the author had to already know to prefix control commands, and there
   was no way to actually answer the question without also typing
   `/syntropy resume` first (which clears `PauseReason` before the
   answer is ever seen by the runner).
2. `cmdResume` (and `cmdRetry`) always returned `StatusAwaitingMerge`.
   That's a lie when the pause came from `discoverSpec`'s `DecisionAsk`:
   `markUnitMerged`/`markUnitBlacklisted` empty `InFlight` before
   handing off to `StatusDiscovering`, so a Run paused there has no
   in-flight MR for `StatusAwaitingMerge` to describe. The workflow
   graph already lists `StatusDiscovering` as a valid `Paused` callback
   destination (see `Build` in `workflow.go`), but nothing returned it.

## Decision

**Paused gate** (`resume()`, `internal/refactorsweep/workflow.go`): a
freeform `EventNoteAdded` reply on an in-flight unit's MR is now passed
to `invokeForEvent` in both cases:

- `strings.HasPrefix(r.Object.PauseReason, askPausePrefix)` — the
  decision `invokeForEvent` returns is used as-is. This needed no new
  branch: `invokeForEvent`'s existing decision switch (`DecisionDone` →
  `StatusAwaitingMerge`, `DecisionAsk` → stays `Paused` with a new
  question, etc.) already does the right thing once it's reachable.
- Any other `PauseReason` — `invokeForEvent` still runs (the reply may
  be useful context for the subagent), but the returned status is
  discarded and `StatusPaused` is returned regardless, so only an
  explicit control command can clear a non-Ask pause.

Every other case (no in-flight unit for the event's MR, or a non-`NoteAdded`
event such as `EventMRConflict`) is unchanged: noted, no transition.

**`cmdResume`** (`internal/refactorsweep/controls.go`): now returns
`StatusDiscovering` when `len(r.Object.InFlight) == 0` (the discoverSpec
pause case) and `StatusAwaitingMerge` otherwise, with the acknowledgement
comment worded to match.

## Alternatives considered

- **Route resume-on-Ask through a dedicated code path instead of
  `invokeForEvent`.** Rejected: that's exactly the "accidental resume via
  a decision branch" failure mode the spec calls out avoiding — the
  point is that a `DecisionAsk` pause resumes via the *same* decision
  switch every other event uses, gated only by checking why the Run is
  paused before calling it, not by a special-cased transition.
- **Track "came from Discovering" with a dedicated bool field instead of
  inferring from `InFlight`.** Rejected: `InFlight` is already
  authoritative for "is a unit's MR open right now", which is exactly
  the condition `StatusAwaitingMerge` vs `StatusDiscovering` turns on;
  a redundant field would need to be kept in sync with it for no benefit.
- **Also fix `cmdRetry`'s equivalent `StatusAwaitingMerge` return.** Out
  of scope for this increment — `cmdRetry` isn't a `DecisionAsk`
  resume path (it's for clearing failure-mode pauses like a git or hook
  error, both of which only occur with an in-flight unit), so its
  honesty gap, if any, is narrower and left for a separate change if it
  turns out to matter.

## Consequences

- A plain-language reply now answers a `DecisionAsk` pause directly,
  without a separate `/syntropy resume`.
- A reply during any other pause still reaches the runner as context but
  can never silently un-pause the Run — matches the spec's "should not
  silently un-pause the Run at all" for non-Ask pauses.
- `cmdResume` on a Run paused before any unit existed now correctly
  re-enters `StatusDiscovering` instead of falsely claiming
  `StatusAwaitingMerge`.

## Tests

- `TestResume_PausedRun_NonAskFreeform_StaysPausedButRunnerStillSeesIt` —
  a non-Ask `PauseReason` invokes the runner but stays `Paused`
  regardless of `DecisionDone`.
- `TestResume_PausedRun_AskPause_FreeformReplyCanResume` — an
  `askPausePrefix` pause lets `DecisionDone` resume to
  `StatusAwaitingMerge` via the existing `invokeForEvent` switch.
- `TestCmdResume_NoInFlightUnits_ReturnsDiscovering` — `cmdResume` with
  empty `InFlight` returns `StatusDiscovering`, not `StatusAwaitingMerge`.
- `TestStatusGraph_PausedAllowsSelfLoop`'s `integrationDeps` now wires a
  non-nil (empty) `runner.Registry`, since the Paused-self-loop path it
  exercises now reaches `invokeForEvent`.
