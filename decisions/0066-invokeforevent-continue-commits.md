# ADR-0066: `invokeForEvent`'s DecisionContinue commits and pushes, like Done

**Status**: Accepted (its Done-only discussion resolution is superseded by [ADR-0115](0115-resolve-thread-on-continue-and-surface-resolve-failures.md) — Continue now resolves the thread too; the commit/push decision and the reply wording split still stand)
**Date**: 2026-07-21

## Context

ADR-0045 redefined `DecisionContinue` to mean "a real partial slice was
shipped, more work remains" for the planned-work loop (`work()`), and made
that loop treat it like `Done` for shipping purposes (commit/push if
dirty, blacklist if clean, record a `RemainderNote`). `invokeForEvent` —
the separate path that handles ad-hoc events on an already-in-flight unit
(`address_comment`, `fix_ci`) — was never updated to match. Its switch
statement bundled `DecisionContinue` with `DecisionNoChange`:

```go
case DecisionContinue, DecisionNoChange:
    // Runner decided nothing actionable. Don't post a comment — that
    // would itself trigger a webhook and risk a loop.
    return StatusAwaitingMerge, nil
```

Found live: a reviewer commented `/syntropy I would like tests to cover
this from the beginning` on an in-flight MR. The runner wrote a real,
substantial, passing test file (`impl_test.go`, 282 lines) but — because
adding full test coverage is naturally a multi-turn task — correctly
returned `DecisionContinue` to signal more work was needed. `invokeForEvent`
treated this identically to "nothing actionable happened," never committed
or pushed the file, and moved on. The file sat permanently untracked in
the worktree until, turns later, it tripped `SyncWithBase`'s
uncommitted-changes guard on an unrelated event, parking the Run with a
confusing `refusing to fetch/merge` pause that gave no hint the real
problem was a silently-dropped `Continue` from several turns earlier.

## Decision

`invokeForEvent`'s switch now handles `DecisionDone` and `DecisionContinue`
together (`case DecisionDone, DecisionContinue:`), running the exact same
`HasWorkBeyondBase` → `Commit` → `Push` sequence Done already had, gated by
a local `isDone := resp.Decision == DecisionDone` for the two places the
two decisions should still differ:

- **Discussion resolution**: only `Done` resolves the originating
  discussion thread. `Continue` means the reviewer's feedback isn't fully
  addressed yet, so the thread stays open for whatever event continues it
  — resolving it would misrepresent an unfinished conversation as settled.
  *(Superseded by [ADR-0115](0115-resolve-thread-on-continue-and-surface-resolve-failures.md):
  since there is no automatic re-invocation, the open thread bought no
  follow-up and only blocked auto-merge, so `Continue` now resolves too
  and relies on the reply wording below to show work remains.)*
- **Reply wording**: `Done` posts `✓ Addressed (...)`; `Continue` posts
  `🔄 Partial progress (...)` plus a pointer that commenting again (or
  `/syntropy prompt <text>`) continues the work — `invokeForEvent` has no
  automatic re-invocation loop the way the planned-work loop does, so this
  sets the reviewer's expectation correctly: another event is what moves
  it forward, not a background retry.

`DecisionNoChange` keeps its original, narrower case (the "truly nothing
happened" path, still deliberately silent to avoid a webhook-loop risk) —
only `DecisionContinue` was miscategorized.

## Alternatives considered

- **Give `invokeForEvent` its own remainder-tracking mechanism**, mirroring
  `work()`'s `RemainderNote` on Plan entries. Rejected: `RemainderNote` is
  specifically a planner-facing artifact (rendered into the next planning
  prompt via `buildPlanningPrompt`) for the sweep/spec discovery loop —
  `invokeForEvent` operates on an already-in-flight unit outside that loop
  entirely, so there's no planner turn for a remainder note to feed. The
  reviewer's own follow-up comment already serves that steering role.
- **Immediately re-invoke the runner again within the same `invokeForEvent`
  call when it returns Continue**, looping until Done/Ask/Fail. Rejected:
  changes this function from event-driven to a bounded-but-unbounded
  retry loop with its own new failure modes (when to give up, how to
  avoid runaway turns) for a problem the existing event-driven model
  already solves adequately — the reviewer commenting again, or the
  original CI event recurring, is a perfectly good "continue" trigger and
  matches how every other multi-turn interaction in this codebase already
  works.

## Consequences

- A `Continue` decision from `address_comment`/`fix_ci` no longer loses
  the runner's work — this specific failure mode (silently-dropped file,
  later surfacing as a confusing unrelated `SyncWithBase` pause) cannot
  recur.
- Reviewers now see an explicit "partial progress, more needed" comment
  instead of silence when a comment-triggered turn doesn't fully finish —
  a behavior change or bystanders/tests that only ever exercised Done
  from this path.
