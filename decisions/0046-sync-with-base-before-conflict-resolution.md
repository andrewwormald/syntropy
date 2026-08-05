# ADR-0046: `SyncWithBase` — refresh the feature branch before conflict-resolution turns

**Status**: Accepted
**Date**: 2026-07-16

## Context

`invokeForEvent` (`internal/refactorsweep/workflow.go`) drives the runner
for two conflict-resolution phases on an already-open MR: address-comment
(`EventNoteAdded`) and fix-CI (`EventPipelineFailed`). Neither currently
refreshes the worktree against base before invoking the runner — the
worktree just sits wherever the last invocation (or `EnsureBranch`) left
it, which can be arbitrarily far behind `origin/<baseBranch>` by the time
a reviewer comments or CI fails, especially on long-lived MRs.

That staleness matters specifically for conflict resolution: a runner
asked to "fix CI" or "address this comment" is reasoning about the diff
between its branch and base. If base has moved on and the worktree hasn't,
the runner's view of "what does this look like against main" is stale —
it can resolve a conflict that no longer exists, or miss one that a newer
base commit just introduced.

`internal/git/git.go` already has `HardReset` (ADR-0011), but that's the
wrong tool here: `HardReset` does `fetch && reset --hard origin/<base> &&
clean -fdx`, which discards local commits entirely. It's used for the
planning worktree, which never carries commits of its own between passes.
The address-comment/fix-CI worktrees are different — they hold the
feature branch's own already-pushed commits (the MR's actual content),
which must survive.

## Decision

Add `SyncWithBase(ctx, dir, baseBranch string) error` to the `Git`
interface (`internal/git/git.go`), implemented as:

```
git fetch origin <baseBranch>
git merge --no-edit origin/<baseBranch>
```

Before fetching, `SyncWithBase` checks the worktree is clean (reusing
`HasChanges`) and refuses with an error if it's dirty. Git's own merge
only rejects uncommitted changes that overlap the merge; non-overlapping
ones — e.g. left behind by an interrupted invocation — would be silently
folded into the merge result. The upfront guard makes "dirty worktree at
sync time" an explicit error instead, surfacing the interrupted-invocation
hazard rather than papering over it.

This preserves the branch's own commits and folds base forward via a
merge commit — no rebase, no force-push. Two outcomes:

1. **Clean merge (or already up to date)**: worktree now reflects current
   base; runner proceeds as normal.
2. **Merge conflict**: `SyncWithBase` returns `nil`, not an error. The
   worktree is left with unmerged paths and conflict markers in place.
   This is treated as legitimate output, not failure — the runner
   invocation that follows (`address-comment` / `fix-CI`) is exactly the
   mechanism meant to resolve it. To tell "expected conflict" apart from
   "real infrastructure failure" (bad ref, fetch failure, etc.), the
   implementation checks `git diff --name-only --diff-filter=U` after a
   failed merge: non-empty output means ordinary conflict (swallowed),
   empty output means the error is real and gets propagated.

This increment adds the method and its test coverage only. Wiring it into
`invokeForEvent` (calling `SyncWithBase` before every address-comment /
fix-CI runner invocation) is the next increment — this ADR records the
mechanism; the call site is a small, separate, easily-reviewed diff.

## Alternatives considered

- **Rebase instead of merge.** Rewrites the feature branch's commit SHAs,
  which means the next `Push` would need `--force`, clobbering the MR's
  existing review history/comments-on-commit on providers that track
  that. Merge keeps prior commits intact and pushes cleanly with the
  existing non-force `Push`.
- **Reuse `HardReset`.** Would discard the feature branch's own commits —
  exactly the content the MR exists to carry. Not applicable here; kept
  as a separate method rather than adding a "preserve commits" flag to
  `HardReset`, since the two have different enough contracts (discard vs.
  preserve) that overloading one method with a bool would obscure which
  call sites do which.
- **Treat merge conflicts as an error from `SyncWithBase`.** Would force
  every call site to special-case "conflict" vs "real error" itself.
  Simpler to make the conflict-tolerant behavior the method's own
  contract, once, than to duplicate that judgment at each caller.

## Consequences

- `Git` interface gained one method; `ExecGit` and the `fakeGit` test
  double (`internal/refactorsweep/workflow_test.go`) both implement it.
  `fakeGit.SyncWithBase` records calls (`syncs []string`) and returns
  `syncErr`, following the same pattern as `resets`/`resetErr`.
- No behavior change yet for `invokeForEvent` — this increment is additive
  and inert until wired in.
- Once wired in (next increment), a merge conflict left by `SyncWithBase`
  means the worktree is dirty going into the runner turn for a reason
  other than "runner made changes" — `work()`/`invokeForEvent`'s existing
  `HasChanges` check after `DecisionDone` needs no change, since a runner
  that actually resolves the conflict will have modified the same files
  and the dirty check would trip regardless.

## Tests

`internal/git/git_test.go`:
- `TestExecGit_SyncWithBase_FastForwardsWithoutLosingLocalCommits` —
  conflict-free sync brings base's commit in and keeps the feature
  branch's own commit.
- `TestExecGit_SyncWithBase_LeavesConflictForRunner` — overlapping edits
  produce a conflict; asserts `SyncWithBase` returns `nil` and the
  worktree is left dirty with `<<<<<<<` conflict markers.
- `TestExecGit_SyncWithBase_RefusesDirtyWorktree` — uncommitted changes
  at call time (deliberately non-overlapping with the merge, so git
  itself wouldn't refuse) make `SyncWithBase` error out before fetching
  or merging, leaving both the worktree and the uncommitted change
  untouched.
- `TestExecGit_SyncWithBase_FetchErrorPropagates` — a nonexistent base
  branch returns a real error.

## Superseded in part

The dirty-worktree guard described above (`Decision`, `Consequences`, and
`TestExecGit_SyncWithBase_RefusesDirtyWorktree`) is superseded by
[ADR-0103](0103-syncwithbase-discards-instead-of-refusing.md): `SyncWithBase`
now discards uncommitted changes instead of refusing to sync. The rest of
this ADR — the fetch/merge mechanism and treating merge conflicts as
non-error output — still stands.
