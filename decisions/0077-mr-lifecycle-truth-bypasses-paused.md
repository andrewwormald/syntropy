# ADR-0077: MR merge/close bypasses the Paused early-return

**Status**: Accepted
**Date**: 2026-07-27

## Context

ADR-0076 fixed the poller half of a real stuck-run incident: it used to
cache "detected merged" the instant a state change was seen, regardless
of whether the workflow actually processed it, so a merge event dropped
even once was lost forever.

That fix alone isn't sufficient. `invokeForEvent`'s `resume()` has a
blanket rule (`internal/refactorsweep/workflow.go`): while a Run is
`Paused`, every inbound event except a `/syntropy` control command is
silently ignored — "noted (`EventsSeen`) but produce no transition." That
rule exists to keep a paused Run inert while waiting on a human (an
unanswered `DecisionAsk` question, a `DecisionFail`, a hook rejection —
see ADR-0075/0076's own incident context). But an `EventMRMerged`/
`EventMRClosed` isn't "more activity to wait out" — it's the provider
telling syntropy an unambiguous fact: the MR this unit was about no
longer exists in the state the Run thinks it's in. Whatever the Run was
paused about is moot the instant its MR merges or closes.

Direct instruction from the user, asked as "should we not look at
resuming the workflow if we get an update so that syntropy can try align
itself with the MR?" — yes: syntropy should reconcile itself with reality
as soon as it learns the truth, not sit paused until a human notices and
manually resumes.

## Decision

`resume()` now checks `EventMRMerged`/`EventMRClosed` **before** the
`StatusPaused` early-return, matching the existing precedent
`EventProviderAuthRestored` already set (ADR-0038): look up the unit via
`unitForMR`, and if it matches an in-flight unit, apply
`markUnitMerged`/`markUnitBlacklisted` immediately regardless of current
status. If the event doesn't match any in-flight unit (cross-talk from
another MR on the shared project webhook), it falls through to the
normal Paused/AwaitingMerge handling unchanged — this bypass is
specifically for the Run's own tracked MR, not a blanket "any merge event
anywhere clears any pause."

## Alternatives considered

- **Auto-resolve the pause too** (clear `PauseReason`, post a comment
  explaining the question is now moot) instead of just applying the
  merge. Rejected for this increment: `markUnitMerged` already fully
  completes the unit (removes it from `InFlight`, adds it to
  `Completed`, moves the Run to `Discovering` to plan the next unit) —
  there's no lingering pause state left to explain away once the unit is
  gone. If a future case exists where a bypassed lifecycle event
  *doesn't* fully resolve the pause on its own, that would need its own
  design, not assumed here.
- **Only bypass Paused for EventMRMerged, not EventMRClosed.** Considered
  narrower scope since the live incident was specifically a merge.
  Rejected: the same "ground truth beats a stale pause" reasoning applies
  identically to a close — an author closing their own MR while syntropy
  sits paused waiting on an unrelated question shouldn't leave the unit
  stranded either.

## Consequences

- A Run paused for any reason (a question, a hook failure, an auth
  issue) now correctly completes/blacklists its unit and moves on the
  moment its MR merges/closes — combined with ADR-0076's redispatch fix,
  this closes the full incident: neither half alone was sufficient (the
  poller could keep redelivering forever and the workflow would still
  drop it; the workflow could apply it but only if the poller ever
  redelivered it after the first drop).
- Cross-talk safety is preserved by explicit test
  (`TestResume_MRMerged_CrossTalk_StaysPausedWhenNoMatchingUnit`) — an
  unrelated MR's merge event on the same shared webhook must not
  spuriously clear an unconnected pause.
