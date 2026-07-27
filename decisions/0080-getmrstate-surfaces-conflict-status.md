# ADR-0080: `GetMRState` surfaces merge-conflict status alongside lifecycle state

**Status**: Accepted
**Date**: 2026-07-27

## Context

ADR-0060 added polling of MR lifecycle state (`opened`/`closed`/`merged`/
`locked`) so merge/close transitions are detected without waiting for an
incidental webhook or comment. Merge conflicts have no equivalent signal:
today a conflict is only discovered when some unrelated comment/CI event
happens to invoke `SyncWithBase` (ADR-0046) and the sync fails. A conflict
that arises between such events — e.g. the target branch moves under an
otherwise-quiet MR — can sit undetected indefinitely.

Both GitLab's `GET .../merge_requests/:iid` and GitHub's
`GET .../pulls/:number` — the same endpoint `GetMRState` already calls
every poll tick — include mergeability in their response body
(`has_conflicts` on GitLab, `mergeable_state` on GitHub). Detecting
conflicts via the existing poll costs no additional API call and spends no
LLM tokens, matching the spec goal.

## Decision

`provider.Provider.GetMRState` now returns a `provider.MRState` struct
(`State string`, `HasConflict bool`) instead of a bare `(string, error)`.
Both fields come off the single existing request:

- **GitLab**: `HasConflict` is `resp.HasConflicts` (the API's own
  `has_conflicts` boolean).
- **GitHub**: `HasConflict` is `pr.MergeableState == "dirty"`. GitHub
  computes mergeability asynchronously and returns a null/absent
  `mergeable_state` while still computing it; that case falls through to
  `false` (no conflict) rather than guessing, since we'd rather miss a
  conflict for one extra poll tick than report a false positive.

`provider.EventMRConflict` is added to the `EventKind` vocabulary as the
synthetic event this will eventually feed, mirroring how
`EventMRMerged`/`EventMRClosed` are synthesized from `MRState.State` in
`poller.mrStateEvent`.

This increment only extends `GetMRState`'s return shape and updates call
sites to compile (`poller.go` unpacks `mrState.State` into the existing
merge/close logic, unchanged in behavior). Wiring `MRState.HasConflict`
into an actual `EventMRConflict` dispatch, and the runner-side handling
that resolves it, is deliberately left to a follow-up — this is a fresh
spec landing in small increments, not a one-shot change to a wide surface.

## Alternatives considered

- **Add a second return value (`hasConflict bool`) instead of a struct** —
  works for the two fields we need today but doesn't scale: any future
  poll-derived signal (e.g. CI status embedded in the same response) would
  mean another positional bool wedged into every implementation's and
  caller's signature. A struct keeps `GetMRState`'s signature stable as
  the polled snapshot grows.
- **A separate `GetMRConflictStatus` method** — would require a second API
  call per tick per in-flight MR, which is exactly the "no extra API
  calls" constraint the spec goal rules out.
- **Poll `merge_status`/`detailed_merge_status` on GitLab instead of
  `has_conflicts`** — `merge_status` can sit in `"unchecked"` or
  `"cannot_be_merged_recheck"` transiently even absent a real conflict,
  and `detailed_merge_status` isn't available on all self-hosted GitLab
  versions this codebase targets. `has_conflicts` is a plain boolean with
  no transient "recheck" state to reason about.

## Consequences

- `provider.Provider` implementations (and every test double standing in
  for one) must return `provider.MRState{State: ...}` instead of a bare
  string — a breaking signature change, resolved in this commit across
  `internal/poller`, `internal/refactorsweep`, and both provider packages.
- `EventMRConflict` exists but nothing dispatches it yet; it is inert
  until the follow-up increment wires `poller.mrStateEvent` (or an
  equivalent) to emit it from `MRState.HasConflict` and teaches
  `refactorsweep`'s `resume()` how to handle it.
- GitHub's async mergeability computation means a conflict on a
  freshly-pushed commit may not be visible for one or two poll ticks after
  the push — acceptable at current polling intervals.
