# ADR-0090: Reconciler's `LastProgress` uses the record's own `UpdatedAt`, not just `History`

**Status**: Accepted
**Date**: 2026-07-29

## Context

Found live, while investigating why a Run (`5ec1a36b`) was believed dead
enough to prompt starting a redundant duplicate Run against the same
project: the daemon's own logs showed the reconciler re-triggering
`5ec1a36b` **five times in 25 minutes**, immediately after it had been
manually resumed from `Failed` back to `Discovering`:

```
12:57:16  reconciler: re-triggered stuck run
12:57:46  reconciler: skipping stuck run in cooldown
...
13:09:46  reconciler: re-triggered stuck run
...
13:12:46  reconciler: re-triggered stuck run
...
13:16:16  reconciler: re-triggered stuck run
...
13:19:16  reconciler: re-triggered stuck run
...
13:22:16  reconciler: skipping stuck run in cooldown
```

— stopping only once real webhook activity (a human comment) landed at
13:22:49. This is exactly what "resurrects, then looks dead again"
zombie behaviour would look like from the outside, even though the Run
was never actually dead during this window — it was doing real,
legitimate (if slow — turns against this codebase routinely take
15-20+ minutes) work the whole time.

Root cause: `reconciler.LastProgress` derived a Run's "last made
progress" timestamp purely from `state.History`'s last Turn — it never
considered the Run record's own `UpdatedAt`, which advances on *every*
store write, including one that doesn't append a Turn at all.
`directResume` (ADR-0086) reviving a `Failed` Run is exactly such a
write: it flips `Status` back to `Discovering` and calls `rs.Store(...)`,
but `History` still points at whatever the Run's last real Turn was —
in this case, days earlier, before the bug that failed it. The instant
the resumed Run re-entered `Discovering`, the reconciler's staleness
check (`now - lastProgress >= threshold`) was already comparing against
that days-old timestamp, immediately exceeding even the 30-minute
threshold (ADR from the `reconcilerStuckThresholdDefault` tuning task)
— so it got re-triggered on the very next sweep tick, and kept getting
re-triggered every `RetriggerCooldown` window (3 minutes) until a fresh
Turn actually completed and finally updated `History`.

## Decision

`LastProgress(state, recordUpdatedAt)` now returns the **later** of
(a) `state.History`'s last Turn's `EndedAt` (or `StartedAt` if still
in-flight) and (b) `recordUpdatedAt`. `Scan`'s call site passes
`rec.UpdatedAt` instead of the previous `rec.CreatedAt` (which never
advances past Run creation and so couldn't have caught this even as a
fallback). If `History` is empty, `recordUpdatedAt` is returned
directly, same as before.

This means any explicit write to a Run's record — not just a new
Turn — correctly resets the reconciler's staleness clock. A resume from
`Failed`/`Cancelled` (`directResume`), or any other future code path
that touches the record without appending a Turn, is now recognised as
"something happened here" instead of being invisible to the staleness
check.

## Alternatives considered

- **Have `directResume` explicitly clear/append a synthetic Turn** so
  `History`'s own last-entry timestamp reflects the resume. Rejected:
  adds a fake Turn to the audit trail (`History` is meant to record real
  runner invocations) purely to satisfy the reconciler's internal
  bookkeeping — using the record's own already-accurate `UpdatedAt`
  achieves the same result without polluting `History` with a
  non-Turn entry.
- **Raise `RetriggerCooldown`** so repeated re-triggers space out further
  even without fixing the root cause. Rejected as the primary fix: this
  incident's Run got re-triggered 5 times in 25 minutes specifically
  *because* it kept looking freshly-stuck on every sweep once the cooldown
  elapsed — a longer cooldown reduces the frequency but doesn't fix the
  underlying wrong staleness calculation, and would also slow down
  legitimate stuck-Run recovery for the case this mechanism exists for
  in the first place (ADR-0033/0053).
- **Skip the reconciler check entirely for some window after a resume**
  (e.g. a grace period). Rejected: requires a new field/timestamp to
  track "last resumed at," which is exactly what `rec.UpdatedAt` already
  is — no new state needed when the existing field already carries this
  information correctly.

## Consequences

- A Run resumed from `Failed`/`Cancelled` no longer gets immediately
  (and repeatedly) re-triggered by the reconciler — its staleness clock
  correctly starts from the resume moment, not from whatever its last
  real Turn happened to be.
- More generally, any write to a Run's record now counts as progress for
  reconciler purposes, not just Turn-appending ones — a strictly more
  correct definition of "is this Run actually stuck" than History alone
  provided.
- Doesn't change behavior for the common case (a Run whose most recent
  write *is* its last Turn) — `recordUpdatedAt` and the Turn timestamp
  are the same moment there, so `LastProgress` returns the identical
  value as before.
