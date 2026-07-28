# ADR-0082: `EventMRConflict` invokes the resolve-conflict subagent after the Paused gate, not before

**Status**: Accepted
**Date**: 2026-07-28

## Context

ADR-0081 wired the poller to dispatch `provider.EventMRConflict` on every
tick a conflict persists, but left `resume()` with no case for it — the
event fell through to the generic filter-eval path, which isn't correct
for a synthetic, deterministic event with no comment/CI payload to
classify. This increment teaches `resume()` how to actually resolve it.

`resume()` already has two existing precedents for how a Kind gets
routed, and this event doesn't match either cleanly:

- `EventMRMerged`/`EventMRClosed` (ADR-0077) bypass `StatusPaused`
  entirely — they're lifecycle *truth*, so a paused Run still applies
  them.
- `EventNoteAdded`/`EventPipelineFailed` go through the cheap filter
  (`filter.Eval`) to decide Skip/Pause/InvokeSubagent, and respect the
  Paused gate — a paused Run ignores them.

## Decision

`EventMRConflict` is handled in `resume()`
(`internal/refactorsweep/workflow.go`) as a new branch placed **after**
the `StatusPaused` early-return, going straight to `invokeForEvent` (no
filter step) once an in-flight unit matches:

- **Not lifecycle truth, so it doesn't bypass Paused like merge/close.**
  A conflict doesn't mean the MR's existence or terminal state changed —
  it means the diff no longer applies cleanly, which is exactly the kind
  of "more activity" a human-driven pause (an unanswered `DecisionAsk`,
  a hook rejection) is meant to hold steady through. Resolving a conflict
  the runner will just re-produce a diff for, while the Run is paused
  waiting on an unrelated question, isn't useful — a paused Run stays
  paused and picks up the conflict via ADR-0081's persistent redispatch
  once it resumes.
- **Not routed through the filter, unlike NoteAdded/PipelineFailed.**
  The filter's job is classifying *unstructured, human-authored* input
  (a comment's intent, a CI log) into Skip/Pause/InvokeSubagent.
  `EventMRConflict` carries no such ambiguity — the poller already
  established the fact via `GetMRState.HasConflict` (ADR-0080), so
  there's nothing for a `.star` filter to decide. This mirrors why
  `EventMRMerged`/`EventMRClosed` also skip the filter, just on the other
  side of the Paused gate.
- `invokeForEvent` gains a `PhaseResolveConflict` phase
  (`internal/refactorsweep/types.go`), builds `req.ConflictFiles` from
  `d.Git.ConflictedFiles` (populated by the `SyncWithBase` call
  `invokeForEvent` already makes for every event kind), and sets
  `req.SkillCommand` to `/syntropy-resolve-conflict <unitID>` — the same
  Request/Turn/Decision machinery `PhaseAddressComment`/`PhaseFixCI`
  already use, so `Done`/`Ask`/`Fail`/`Continue`/`RetryCI` handling is
  inherited for free.

## Alternatives considered

- **Bypass Paused like merge/close.** Rejected per the reasoning above —
  a conflict is a re-derivable condition (SyncWithBase will surface it
  again on the next unpaused tick), not a one-time fact that goes stale
  if not applied immediately. Bypassing Paused here would mean the
  runner burns a turn resolving a conflict while a Run is still stuck
  on, say, an unanswered `DecisionAsk` — the resolved diff could go stale
  again before anyone answers the question anyway.
- **Route through the cheap filter like NoteAdded/PipelineFailed.**
  Rejected: the filter has nothing to classify here — the event is
  already a definite "invoke the runner" fact, and forcing it through
  `.star` eval would just be a no-op hop (or worse, a foot-gun if some
  repo's filter happens to `Skip` on unrecognized Kinds).

## Consequences

- A Run paused for any reason no longer picks up conflict resolution
  until it resumes — verified by
  `TestResume_MRConflict_WhilePaused_StaysPaused`.
- An unpaused Run with a matching in-flight unit invokes the runner
  directly on `EventMRConflict` — verified by
  `TestResume_MRConflict_InvokesSubagent`.
- Completes the ADR-0080/0081/0082 chain: detection → dispatch →
  resolution, closing out the full spec goal of catching conflicts via
  the existing poll instead of waiting for an incidental comment/CI event
  to surface them through `SyncWithBase`.
