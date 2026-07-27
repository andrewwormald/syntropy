# ADR-0076: Redispatch terminal MR-state events every tick, don't cache "detected" as "delivered"

**Status**: Accepted
**Date**: 2026-07-27

## Context

Found live: a real run's MR merged on GitLab, but the run sat believing
the unit was still in-flight for over an hour afterward, with no
automatic path back to a correct state — not even a future reconciliation
sweep (ADR-0047/0053) would have caught it, since the run kept receiving
and processing *other* events (comments), so it never looked "stale" by
any inactivity heuristic.

Root cause, in `internal/poller/poller.go`'s `pollRun`: for each in-flight
unit, `GetMRState` is polled every tick and compared against
`mrStates[mr.IID]` (persisted as `AgentState.LastMRStates`). On a change,
`mrStateEvent` builds an `EventMRMerged`/`EventMRClosed`, `Dispatcher` is
called, and — critically — `mrStates[mr.IID]` is updated to the new state
**regardless of whether the dispatch was actually processed**, only
whether the `Dispatcher` call itself returned an error. But `Dispatcher`
returning `nil` doesn't mean the workflow *applied* the transition: the
merge event happened to arrive while the run was `Paused`, and
`invokeForEvent`'s "while Paused, ignore all events" guard
(`internal/refactorsweep/workflow.go`) drops it silently, no error
involved. The poller had no way to see that drop — it just remembered
"already told them", so every subsequent tick's `state != prev` came back
false, and the event was never regenerated. Permanently stuck.

## Decision

For terminal MR-state events specifically (`mrStateEvent` returning a
non-empty `Kind`, i.e. "merged" or "closed"), dispatch **every poll tick**
that the unit is still present in `ActiveRun.InFlight` — not gated on
`state != prev`. `mrStates[mr.IID]` is still updated (kept for
observability/the persisted snapshot), but it no longer suppresses
redispatch.

This is safe and self-terminating: `ActiveRuns` is re-read fresh from the
store every tick, and the only thing that removes a unit from `InFlight`
is the workflow actually processing the merge/close
(`markUnitMerged`/`markUnitBlacklisted`). So once the transition genuinely
lands, the unit simply isn't in the next tick's `InFlight` at all — no
special "already handled" bookkeeping is needed to stop redispatching.
Until then, redispatching every ~30s is a cheap, idempotent retry: a
`resume()` call that's already processed a unit (cross-talk on an MR it
no longer tracks) just no-ops via `unitForMR` returning "" — see
`internal/refactorsweep/workflow.go`'s early-return for exactly this case.

## Alternatives considered

- **Only skip the cache update if `Dispatcher` returns an error.** Doesn't
  fix the actual bug — the drop here happened with a `nil` error (Paused
  is a normal, successful early-return, not a failure), so this
  wouldn't have caught the real incident.
- **Fix it at the workflow layer instead** — e.g. don't let `Paused` drop
  `EventMRMerged`/`EventMRClosed` specifically, since a merge is true
  regardless of pause state. Rejected as the sole fix: it would patch this
  one drop cause, but the poller's "cache on detect, not on deliver"
  design is the actual structural bug — any other reason `resume()`
  ever fails to apply a dispatched terminal event (a transient store
  write error, a future code path, a bug not yet found) would hit the
  same permanent-stuck failure mode. Making redispatch retry-until-gone
  closes the whole class, not just this one instance of it.
- **Add a reconciliation-sweep check specifically for this** (poll GitLab
  independently of the per-Run poller, on some sweep). Rejected: this
  would duplicate the exact polling logic pollRun already does, just on
  a second schedule — better to fix the one mechanism that's supposed to
  handle it than add a second, redundant one that could itself develop
  the same bug.

## Consequences

- A `Paused` run's in-flight MR that merges/closes while paused now gets
  correctly (re-)notified as soon as it leaves `Paused` and a poll tick
  runs — no manual intervention needed, unlike the incident that
  prompted this ADR (which required a direct store edit to unblock,
  since the cached state made the daemon's own recovery impossible).
- Slightly more `Dispatcher` calls than before during any window where a
  unit sits merged/closed but not yet actually processed — bounded by
  the poll interval and however long that window lasts; harmless given
  `resume()`'s existing idempotent no-op path for units no longer
  tracked.
- If a future terminal MR state is added to `mrStateEvent` beyond
  merged/closed, it inherits this retry-until-gone behavior
  automatically — no per-state bookkeeping to remember to add.
