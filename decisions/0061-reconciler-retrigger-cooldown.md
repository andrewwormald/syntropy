# ADR-0061: Per-Run cooldown after a reconciler re-trigger

**Status**: Accepted
**Date**: 2026-07-20

## Context

ADR-0053 added `internal/reconciler`'s sweep, which every `Interval`
(default 30s) re-triggers any Run whose `LastProgress` is older than
`Threshold` (default 20 minutes). ADR-0053's idempotency argument —
that a re-trigger is a harmless no-op once the record's version has
moved on — assumes the Run either wakes up and advances, or is a
genuinely wedged step that a retrigger can't fix either way. It didn't
account for the gap between those two: a Run that's still legitimately
stuck (agent turn genuinely running long, or truly wedged) stays flagged
by `Scan` on every subsequent tick until it either finishes or exceeds
`Threshold` again from a new baseline. With a 30s `Interval` and a Run
stuck well past `Threshold`, that means a re-trigger event on the same
topic every 30 seconds for as long as it stays stuck — spamming the
event log and logger for a condition a repeat send cannot resolve any
faster.

## Decision

`Sweeper` tracks a per-`RunID` cooldown and skips re-triggering a Run
still inside it. Stated as a single sentence: **a Run that's just been
re-triggered is left alone for `RetriggerCooldown` (default 3 minutes)
unless it's made fresh progress since**, even if `Scan` still flags it
as stuck.

- `Sweeper.cooldowns` is an in-memory `map[string]cooldownEntry` keyed
  by `RunID`, read and written only from `sweepOnce`, which `Run` calls
  serially from a single goroutine — no locking needed.
- Each `cooldownEntry` records `retriggeredAt` and the `lastProgress`
  value observed at that retrigger. On a later sweep, a stuck `RunID`
  is skipped only if both hold: `lastProgress` hasn't advanced past the
  recorded entry's `lastProgress`, and `now - retriggeredAt <
  RetriggerCooldown`. If `lastProgress` has advanced — the Run made a
  new turn since the last retrigger and then got stuck again — the
  cooldown is bypassed immediately regardless of elapsed time, because
  that's a new stuck episode, not the same one repeating.
- The zero value of `RetriggerCooldown` disables the cooldown entirely
  (every stuck Run is re-triggered on every sweep), preserving
  ADR-0053's original per-tick behaviour for any caller that doesn't
  set it.
- Wired in `main.go` via `--reconciler-retrigger-cooldown`, default
  `3 * time.Minute` (`reconcilerRetriggerCooldownDefault`), the same
  pattern as `--reconciler-stuck-threshold` from ADR-0053: an
  operational knob, not hardcoded, because the right value depends on
  operational experience.

**3-minute cooldown vs. 20-minute stuck threshold**: the two knobs
answer different questions and are deliberately not the same value.
`Threshold` (20 minutes) asks "how long can a legitimately slow agent
turn run before we suspect it's actually stuck?" — set generously to
avoid false positives against real work. `RetriggerCooldown` (3
minutes) asks "once we've already acted on a stuck Run, how long
before acting again could plausibly help?" — set shorter than
`Threshold` because it's not guarding against false positives, it's
rate-limiting a repeat action whose second attempt within seconds of
the first can't possibly do anything the first attempt didn't. 3
minutes is long enough that the sweep isn't re-sending on every 30s
tick for a still-stuck Run, short enough that a Run which does recover
mid-cooldown isn't made to wait anywhere near as long as the original
20-minute detection threshold before a further stuck episode gets
picked up again.

## Alternatives considered

- **Cooldown = Threshold (reuse the existing knob).** Simpler — one
  fewer flag. Rejected: conflates "how long before we suspect a Run is
  stuck" with "how long before repeating an action we already took,"
  which answer different questions (see Decision) and have no reason
  to move together; an operator raising `Threshold` to tolerate slower
  turns would also silently make repeat-retrigger spam worse.
- **Track cooldown state in the RecordStore instead of in-memory.**
  Would survive daemon restarts, matching ADR-0049's rationale for
  moving the event log to sqlite. Rejected for this increment: a
  restart naturally clears the in-memory cooldown map, which just
  means the first sweep after restart may re-trigger a Run that was
  mid-cooldown — a harmless extra no-op per ADR-0053's idempotency
  guarantee, not a correctness issue. Not worth new schema for a
  purely rate-limiting concern.
- **Skip re-triggering a stuck Run a fixed number of times instead of a
  time window.** A count doesn't map cleanly to "enough time for a
  woken-up receiver to actually make progress," which is what the
  cooldown is protecting; a duration does.
- **No fresh-progress bypass — cooldown always runs the full
  duration.** Simpler state (drop `lastProgress` from `cooldownEntry`,
  key purely on `retriggeredAt`). Rejected: without it, a Run that
  recovers, makes a turn, and gets stuck again shortly after would sit
  silently for up to the full cooldown before being noticed again,
  which is exactly the "stuck Run is silent" failure ADR-0053 exists to
  prevent.

## Consequences

- A repeatedly-stuck Run now generates at most one re-trigger event per
  `RetriggerCooldown` window instead of one per `Interval` tick,
  cutting event-log and log-line volume for the pathological case from
  every-30s to every-3-minutes without changing detection latency for
  the common case (a Run stuck for the first time is still caught and
  retriggered at the same `Threshold`-bounded point as before).
- `cooldowns` is unbounded for the lifetime of the daemon process
  except for entries that get overwritten on a later retrigger — a
  RunID that's retriggered once and then finishes normally leaves a
  stale entry in the map forever. Not evicted because Run volumes at
  everflow's current scale make this negligible (per ADR-0053's
  Consequences making the same call about `Scan`'s O(n) sweep); revisit
  together if either becomes a real memory concern.
- Two related operator-facing knobs now exist
  (`--reconciler-stuck-threshold`, `--reconciler-retrigger-cooldown`)
  that must be reasoned about together, not independently — see the
  Decision section's comparison. Documentation and future changes to
  either should call out the other.

