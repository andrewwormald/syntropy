# ADR-0103: `SyncWithBase` discards uncommitted changes instead of refusing

**Status**: Accepted (supersedes the dirty-worktree guard from [ADR-0046](0046-sync-with-base-before-conflict-resolution.md); that ADR's fetch/merge/conflict-swallowing decisions are unaffected. [ADR-0104](0104-discardstraywork-discards-instead-of-committing.md) extends the same discard rule to `commitStrayWork`/`discardStrayWork`, so the "must commit it" line below no longer describes that helper)
**Date**: 2026-08-05

## Context

ADR-0046 gave `SyncWithBase` an upfront guard: if the worktree had
uncommitted changes at call time, it refused to fetch/merge and returned
an error, which `invokeForEvent` (`internal/refactorsweep/workflow.go`)
turns into a paused Run. The guard existed to stop non-overlapping
uncommitted edits from being silently folded into the merge result.

In practice this guard is the exact failure mode ADR-0074 was written to
route around: a runner turn that edits files without reaching
`Done`/`Continue` (or an interrupted invocation) leaves the worktree
dirty, and the very next `SyncWithBase` pauses the Run on an opaque git
message unrelated to whatever event actually triggered it — a human has
to inspect the worktree and clear it by hand to unblock.

The harness owns every commit a unit ships; nothing worth keeping should
ever be sitting uncommitted in a worktree between turns. A prior
increment added `Git.DiscardUncommitted` (resets tracked files to HEAD,
removes untracked files, leaves the branch's own commits alone) as a
standalone method for exactly this kind of cleanup, but left it
unwired — `SyncWithBase` was the last call site still erroring instead
of using it.

## Decision

`SyncWithBase` (`internal/git/git.go`) now calls `DiscardUncommitted` at
the point it used to check `HasChanges` and refuse. Any uncommitted
tracked-file edits or untracked files are thrown away before the fetch;
the fetch/merge then proceeds exactly as if the worktree had been clean
to begin with. The branch's own already-committed history is untouched.

This closes the loop opened by ADR-0074: instead of a human manually
verifying and rescuing a stray uncommitted edit to unblock a paused Run,
`SyncWithBase` simply discards it and moves on. A runner turn that wants
its work to survive must commit it — `Commit`/`commitStrayWork` already
exist for exactly that — uncommitted output was never a durable place to
leave anything, since nothing else in the harness reads a dirty
worktree back out.

## Alternatives considered

- **Keep refusing, but auto-retry via `commitStrayWork` first.** Rejected:
  `commitStrayWork` is scoped to the four `invokeForEvent` decision
  branches that don't otherwise touch git (ADR-0074); teaching
  `SyncWithBase` itself to commit would mean an uncommitted edit
  sometimes gets shipped as a local commit and sometimes discarded,
  depending on which call path hit it first — one consistent rule
  (discard) is simpler to reason about than two competing ones.
- **Leave the guard as an error but improve the pause message.** Rejected:
  still requires a human to intervene on every occurrence instead of the
  harness self-healing, which is exactly the standing operating cost this
  spec goal is meant to remove.

## Consequences

- `TestExecGit_SyncWithBase_RefusesDirtyWorktree` (now
  `TestExecGit_SyncWithBase_DiscardsDirtyWorktree`) asserts the opposite
  of before: the merge proceeds and the uncommitted file is gone
  afterward, rather than asserting the merge is blocked.
- A `SyncWithBase` failure now only ever means a genuine failure (fetch
  error, unknown branch) — never a dirty tree — so the pause message
  callers show for it ("couldn't sync this branch with `<base>`") no
  longer needs a human to consider "was this just leftover work?" as a
  possible cause.
- Any uncommitted edit a runner leaves behind between turns without
  reaching `Done`/`Continue`/`commitStrayWork`'s safety net is now
  silently lost the next time `SyncWithBase` runs, rather than pausing
  for a human to rescue it by hand — an intentional tradeoff per the
  spec goal: the harness owns every commit, so nothing worth keeping
  should ever depend on sitting uncommitted between turns.
