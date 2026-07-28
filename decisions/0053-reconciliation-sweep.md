# ADR-0053: Reconciliation sweep for Runs stuck on a lost event

**Status**: Accepted
**Date**: 2026-07-17

## Context

ADR-0033 replaced `memstreamer` with `internal/eventstream`, a
`sync.Cond`-based `workflow.EventStreamer`, and accepted at the time that
the event log itself could stay in-memory because durability came from
the RecordStore's transactional outbox (ADR-0022) and everflow is
single-process. ADR-0049 later closed the specific restart-loss gap that
reasoning missed by moving the log and per-receiver cursors into sqlite.

That still doesn't make a lost in-memory event impossible in every case.
A `Send` reaches every parked receiver via `Broadcast`, but nothing
guarantees a given receiver goroutine is alive and parked to catch it —
an operator's runner process exiting mid-step, a receiver goroutine that
panics and isn't restarted, or any other loss between a `Store` call and
delivery leaves the record's `AgentState` sitting in `StatusWorking` or
`StatusDiscovering` with no event left to wake it. There's no timeout or
liveness check anywhere else in the daemon to notice this. A stuck Run is
silent: no error, no comment, no MR update — just a Run that stopped
advancing.

## Decision

`internal/reconciler` runs a periodic, non-busy-loop sweep
(`Sweeper.Run`, ticking every `Interval`, default 30s — matching
`internal/poller`'s cadence) that finds Runs stuck on a lost event and
re-triggers them:

- `Scan` pages through `workflow.RecordStore.List` for
  `RunStateRunning` records (deliberately excluding `RunStatePaused` —
  a pause is an intentional stop, not a lost event) and flags any whose
  status is `StatusWorking` or `StatusDiscovering` — the only two
  statuses that depend on an in-memory event to advance — and whose
  `LastProgress` (last `Turn.EndedAt`, or `StartedAt` if the turn is
  still in-flight, or the record's `CreatedAt` if there's no history
  yet) is older than `Threshold`.
- `Retrigger` re-sends the record's current-status event on the same
  topic (`workflow.Topic(WorkflowName, Status)`) that a normal
  transition would use, via `streamer.NewSender` — the identical
  event-topic mechanism `main.go`'s initial `Trigger` call uses to start
  a Run, not a side channel.
- Idempotency is not reimplemented — it's inherited. The re-sent event
  carries `HeaderRecordVersion` from the record's current
  `Meta.Version`; the vendored library's `stepConsumer` already skips
  any event whose version header doesn't match the record's live
  version. So retriggering a Run that's already moved on, or
  retriggering the same stuck Run twice, is a no-op rather than a
  double-process. Reconciler code does no version bookkeeping of its
  own.
- `main.go` wires this in as `buildSweeper`, run alongside the daemon's
  other background loops, with the threshold exposed as
  `--reconciler-stuck-threshold` (default 30 minutes) rather than
  hardcoded, since the right value depends on how long a legitimate
  agent turn can take and that's expected to change as usage grows.

## Alternatives considered

- **Make the EventStreamer itself durable/retrying** (e.g. redeliver
  unacked events after a lease timeout, like an at-least-once queue).
  Closes the gap at the source rather than papering over symptoms, and
  is the more "correct" long-term fix. Rejected for this increment:
  it changes the streamer's delivery semantics for every consumer, not
  just this one failure mode, and risks introducing duplicate-delivery
  bugs of its own (now every step needs to tolerate redelivery, not
  just the rare stuck case). ADR-0049 already moved the log to sqlite
  for the restart case specifically because it's the higher-value,
  lower-risk fix; a full redelivery/lease model is a larger change
  better scoped on its own if a future increment finds sweep latency
  unacceptable.
- **Do nothing beyond ADR-0049's sqlite log.** ADR-0049 closes the
  restart-loss case but not the live-process-lost-receiver case
  described above in Context. Rejected — a stuck Run with no restart
  involved would sit forever with nothing to notice it.
- **Detect stuck Runs some other way (e.g. a separate liveness table,
  heartbeats per receiver).** Would need new schema and a new failure
  mode to reason about (what if the heartbeat writer itself stalls).
  Rejected in favour of reusing `RecordStore.List` + the existing
  `AgentState.History` timestamps, which the workflow already
  maintains for other reasons.

## Consequences

- A stuck Run now self-heals within `Threshold` + `Interval` instead of
  sitting forever. The cost is that latency window — a real Run
  legitimately mid-turn for longer than `Threshold` gets re-triggered
  unnecessarily, but `Retrigger`'s version-guarded idempotency (see
  Decision) makes that a harmless no-op, not a duplicate process.
- The reconciler is a backstop, not the durability fix — ADR-0049's
  sqlite-backed log remains the primary defence against the
  daemon-restart case; this ADR's sweep covers what ADR-0049 doesn't
  (in-process receiver loss without a restart). Both are needed.
- `--reconciler-stuck-threshold` is one more operational knob: setting
  it too low re-triggers legitimately slow turns more often than
  useful (still harmless per the idempotency guard, but noisy in logs);
  too high leaves a genuinely stuck Run silent for longer. 30 minutes
  is a starting guess, not a measured value — revisit if production
  usage shows agent turns routinely exceeding it.
- `Scan` pages the full `RunStateRunning` set every tick via
  `RecordStore.List`; fine at everflow's current Run volumes, but if
  the number of concurrently running Runs grows large this becomes an
  O(n) full-table sweep every 30s and may need an index or a
  narrower query (e.g. filter by status server-side) as a follow-up.
