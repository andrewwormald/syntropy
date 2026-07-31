# ADR-0096: Re-verify `EventMRClosed` against the provider before blacklisting, fall back to the webhook's word if the re-verify read itself fails

**Status**: Accepted
**Date**: 2026-07-31

## Context

`resume()` used to blacklist a unit the instant an `EventMRClosed` webhook
arrived (`markUnitBlacklisted(..., "MR closed without merge")`), trusting
the event kind outright. GitLab and GitHub both transition an MR through a
"closing" state as part of merging it, and webhook delivery isn't ordered
against the provider's own read-your-writes guarantee — a "closed" webhook
can arrive while the platform is still finishing the eventual-consistency
dance of actually recording the MR as merged. Trusting the webhook alone
risked permanently blacklisting a unit that in fact landed, with no
automatic way to reclaim it afterwards (blacklisting is final — see
`discoverSweep`'s dedup and `checkBudget`'s `Completed + Blacklisted`
accounting).

A second question followed directly from the first: what happens when the
re-verify read (`GetMRState`) itself fails? The original re-verify change
returned that as a step error, which retries indefinitely against a
provider that may simply be down — leaving the unit stuck `AwaitingMerge`
forever, never blacklisted, never merged, with no operator visibility
beyond the retry loop.

## Decision

`handleMRClosed` (`internal/refactorsweep/workflow.go`) intercepts every
`EventMRClosed` before it reaches `markUnitBlacklisted`:

- Fresh `GetMRState` read on the same MR. `"merged"` wins over the stale
  webhook payload → `markUnitMerged`, same as a native `EventMRMerged`.
- Neither `"merged"` nor `"closed"` (e.g. reopened) → the webhook is stale;
  leave the unit in-flight and let a later webhook/poll tick settle it.
- `"closed"` confirmed → `markUnitBlacklisted(..., "MR closed without
  merge")`, same outcome as before this change, now provider-confirmed.
- `GetMRState` itself errors → **fall back to the webhook's word and
  blacklist anyway**, `markUnitBlacklisted(..., "MR closed without merge
  (re-verify failed)")`, with a `slog.WarnContext` so the ambiguity is
  visible in logs rather than silently trusted. This intentionally
  reverses the first cut of this change (which returned the error and
  retried indefinitely) — see Alternatives.

## Alternatives considered

- **Retry the step forever on a `GetMRState` error** (the original
  implementation) — correct in spirit ("don't blacklist on an unverified
  read") but leaves the unit stuck in-flight for as long as the provider
  is unreachable, with no forward progress and no operator signal beyond
  an ever-growing retry count. A provider outage of any length turns into
  an indefinite stall for that unit.
- **Pause the Run and ask a human** — more conservative, but a transient
  provider blip (rate limit, brief 5xx) doesn't warrant interrupting an
  unattended sweep; the webhook's own word is a reasonable fallback signal
  for exactly this case.
- **Treat a `GetMRState` error as "still open," leave in-flight** — matches
  the "neither merged nor closed" branch's spirit, but does nothing with
  the *closed* webhook we already received; a provider that stays down
  past the webhook's retry window would leave the unit in-flight forever
  with no fallback, same failure mode as retrying forever.

## Consequences

- A `GetMRState` outage no longer stalls a unit indefinitely — it resolves
  immediately in the direction the last-known signal (the webhook)
  pointed, same tradeoff the rest of the system already makes ("trust the
  freshest signal we have" — c.f. ADR-0060's poll-based state detection).
- **Known false-negative risk, accepted, not corrected here**: if the
  re-verify read fails during the exact eventual-consistency window this
  ADR was written to guard against (webhook says "closed" while the
  provider is still finishing recording a merge), the fallback blacklists
  a unit that actually landed. This is strictly narrower than the
  pre-increment-1 behavior (requires webhook *and* the immediately-
  following read to both misfire), but it is not zero.
- **Re-discovery knock-on, investigated, left as a follow-up**:
  `BlacklistedUnit.Reason` distinguishes a confirmed close ("MR closed
  without merge") from an unverified fallback ("MR closed without merge
  (re-verify failed)"), but `buildPlanningPrompt` only surfaces
  `Plan[].Outcome`/`Rationale` to the spec-mode planner — it never reads
  `Blacklisted[].Reason`. A false-negative blacklist and a confirmed one
  look identical in the next planning turn's history (`- unit-x
  [blacklisted]: <original rationale>`), so the planner has no signal
  that this particular blacklist is unverified and might warrant
  re-discovering the same increment. In sweep mode this can't happen at
  all — `discoverSweep` dedups permanently against `Blacklisted` with no
  re-check path. Surfacing `Reason` to the planner (and/or giving sweep
  mode a bounded re-check for `"(re-verify failed)"` entries) is real
  follow-up work, deliberately not bundled into this ADR or its
  implementation — it's a distinct concern (planner/sweep visibility)
  from the fallback policy this ADR documents.
- Regression coverage: `TestResume_MRClosed_ReVerifiesAgainstProvider_ActuallyMerged`
  covers the merge-wins-over-stale-webhook path;
  `TestResume_MRClosed_ReVerifyErrorFallsBackToBlacklist` covers the
  fallback-on-read-failure path (superseding the now-removed
  `TestResume_MRClosed_ReVerifyErrorLeavesUnitInFlight`, which asserted the
  discarded retry-forever behavior).
