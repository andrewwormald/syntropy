# ADR-0093: Poller dispatches Runs concurrently, bounded by a semaphore

**Status**: Accepted
**Date**: 2026-07-29

## Context

Investigated live: does working on syntropy's own repo (`andrewwormald/
syntropy`) genuinely slow down progress on Runs against `core`? Proved
true by inspecting the vendored `github.com/luno/workflow@v0.5.0`
library source directly.

Two distinct bottlenecks exist, only one of which this ADR addresses:

1. `AddStep`-registered statuses (`Discovering`, `Working`) share one
   single-threaded consumer each in the library itself, since
   `ParallelCount` is never configured in `Build()`
   (`internal/refactorsweep/workflow.go`) and defaults to 0/unset —
   confirmed via `workflow.go`'s `if parallelCount < 2 { consumeStepEvents(w,
   currentStatus, config, 1, 1) }`. This is a real limitation but is NOT
   what this ADR fixes — deferred, see Consequences.

2. `AddCallback`-registered statuses (`AwaitingMerge`, `Paused`,
   `AwaitingAbandonConfirm`) have **no** library-level bottleneck at
   all: `builder.go`'s `AddCallback` registers no consumer/topic/shard,
   and `callback.go`'s `Workflow.Callback` runs the callback function
   synchronously inline, in whatever goroutine calls it.

The actual bottleneck for (2) turned out to be syntropy's own
`internal/poller/poller.go`: `pollOnce`'s dispatch loop
(`for _, r := range runs { l.pollRun(ctx, r) }`) was a plain sequential
loop with no goroutines. A single slow `rn.Run()` call (the `claude -p`
subprocess invocation, observed live to take 15-20+ minutes) blocked
the entire poller — all Runs, across all repos — from making any
progress until it returned. This is the concrete, provable mechanism by
which syntropy work on its own repo delays `core` Runs and vice versa.

## Decision

`pollOnce` now dispatches each Run's `pollRun` call in its own
goroutine, bounded by a semaphore (`Loop.Concurrency`, default
`defaultPollConcurrency = 8`), with a per-RunID in-flight guard so a
still-running `pollRun` from a previous tick is never given a second,
overlapping `pollRun` for the same Run.

The semaphore acquire happens *inside* each spawned goroutine, not in
the dispatch loop itself — `pollOnce` spawns a goroutine per unclaimed
Run and returns immediately, regardless of how many Runs are pending
relative to `Concurrency`. Only the actual `pollRun` work is
gated on semaphore capacity. This matters: acquiring the semaphore in
the loop (blocking on `sem <- struct{}{}` before spawning) would make
`pollOnce` itself block once `Concurrency` slots filled — reintroducing
a bounded version of the exact problem being fixed, since the daemon's
ticker (`Loop.Run`) calls `pollOnce` synchronously and a blocked
`pollOnce` means no new tick can process either.

## Alternatives considered

- **Configure `ParallelCount` on the `AddStep` calls.** Fixes bottleneck
  (1) but not (2) — `AwaitingMerge` is where most live Runs actually
  sit (waiting on CI/review), and its callback path has zero
  library-level sharding to configure. Deferred as lower priority; see
  Consequences.
- **One goroutine per Run with no bound.** Simpler, but an unbounded
  number of concurrent `claude -p` subprocesses (each consuming
  significant CPU/memory/API-rate-budget) is an availability risk with
  many active Runs. A bounded semaphore caps worst-case resource use.
- **Claim/lease-based competing-consumers model in `luno/workflow`
  itself**, replacing static shard/`ParallelCount` partitioning with
  dynamic, datastore-backed lease claiming (raised separately: the user
  is `luno/workflow`'s maintainer and wants to eventually fix this at
  the library level, not just in syntropy's poller). This is the
  "correct" long-term fix for bottleneck (1) and for scaling across
  multiple daemon instances, but is a substantially bigger design
  effort explicitly deferred — not started as part of this change.

## Consequences

- Fixes the concrete, measured cross-repo/cross-run blocking for
  `AwaitingMerge`/`Paused`/`AwaitingAbandonConfirm` Runs: one slow
  runner invocation no longer stalls the poller's ability to check any
  other Run for new activity.
- `Concurrency` is a static, process-local cap — it does not
  dynamically scale with load, and running multiple daemon instances
  would each apply their own independent cap rather than sharing one
  global budget. Acceptable for the single-daemon-per-repo-set topology
  in use today; revisit if/when multi-instance daemons are introduced
  (tracked separately, task #26 in this session's working list).
- Does not address bottleneck (1) (`Discovering`/`Working` sharing one
  unparallelized consumer each in `luno/workflow`) — identified as
  lower priority since fewer Runs sit in those statuses at any time
  compared to `AwaitingMerge`. Left for a future `ParallelCount`
  change or, more likely, the library-level lease redesign discussed
  above.
