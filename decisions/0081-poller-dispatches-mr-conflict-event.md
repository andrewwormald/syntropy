# ADR-0081: Poller dispatches `EventMRConflict` from `MRState.HasConflict`

**Status**: Accepted
**Date**: 2026-07-27

## Context

ADR-0080 extended `GetMRState` to return `HasConflict` alongside lifecycle
`State`, and added `provider.EventMRConflict` to the event vocabulary, but
deliberately left it inert — nothing dispatched it yet.

## Decision

`poller.pollRun` now dispatches `provider.EventMRConflict` for an in-flight
MR whenever `GetMRState` reports `HasConflict == true`, using the same
response the existing merge/close check already fetches (no extra API
call, no LLM tokens).

The dispatch is unconditional on every tick the conflict persists — there
is no "already told them" cache, mirroring the terminal-state
(`EventMRMerged`/`EventMRClosed`) redispatch established in ADR-0076: a
cache keyed on the poller's own view of "did I already dispatch this"
previously caused a live incident where a dropped event (e.g. a Paused
Run) silently and permanently suppressed all future redispatch. Since
`resume()` doesn't yet have conflict-handling logic to no-op on
(left to a follow-up increment per ADR-0080), redispatch stops only once
the underlying conflict is actually resolved and `HasConflict` reports
`false` on a later poll.

## Consequences

- `refactorsweep.resume()` has no case for `provider.EventMRConflict` yet;
  it falls through to the generic filter-eval path today. Teaching
  `resume()` how to resolve the conflict (e.g. triggering `SyncWithBase`
  proactively) is the next follow-up increment.
- No new persisted state was added to `AgentState`/`ActiveRun` — conflict
  detection is stateless per tick, same as the terminal-state check.
