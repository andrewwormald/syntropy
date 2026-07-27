# ADR-0074: Commit/push stray work on every `invokeForEvent` decision, not just Continue/Done

**Status**: Accepted
**Date**: 2026-07-27

## Context

Found live, twice now, in the same shape: a Run pauses on
`SyncWithBase: worktree ... has uncommitted changes; refusing to
fetch/merge` before it can even process the next event. The worktree
still holds real, legitimate edits from a prior turn — in the most
recent incident, a genuine test-coverage extension — that were never
committed.

ADR-0066 fixed exactly this for `DecisionContinue`: previously it was
bundled with `DecisionNoChange` and any work the runner did on a
Continue turn was silently dropped. The fix made `Continue` commit and
push exactly like `Done`, gated on `HasWorkBeyondBase`.

That fix didn't go far enough. `invokeForEvent`'s switch has four more
branches — `DecisionNoChange`, `DecisionAsk`, `DecisionFail`, and
`DecisionRetryCI` — and none of them ever touch git. The assumption
baked into each of those branches is that the runner didn't change
anything: NoChange says so explicitly, Ask/Fail describe a runner that
gave up before finishing, and RetryCI describes a runner that judged
the CI failure as infra noise rather than a code problem. In practice a
runner can still edit files on any of these turns — e.g. start writing
a fix, decide partway through that it needs the author's input, and
stop — without that edit being "the" deliverable the decision is about.
The decision reflects the runner's conclusion about whether the work
is *finished* or *actionable*, not whether the worktree is *clean*.

Once such an edit is left uncommitted, the worktree stays dirty across
every subsequent event for that unit, and the very next `SyncWithBase`
(ADR-0046) refuses to fetch/merge — pausing the Run on an opaque git
message that has nothing to do with whatever the next event actually
was. The only fix available at that point is a human manually
inspecting the worktree, verifying the stray diff is legitimate, and
committing/pushing it by hand to unblock — which is what happened both
times this was hit live, and isn't sustainable as a standing operating
mode.

This directly serves the standing principle "the harness owns git, not
the model" (reaffirmed in ADR-0073): the harness should not depend on
the model choosing to leave a clean tree behind on turns where it
didn't explicitly decide to ship something.

## Decision

Add `(*Deps).commitStrayWork(ctx, worktree, branch, commitMsg)`: checks
`HasWorkBeyondBase`, and if true, commits (tolerating `ErrNoChanges` —
the runner may have self-committed) and pushes. Entirely best-effort —
any error is swallowed rather than surfaced, so it never interferes
with the decision's own pause/reply/messaging logic. Worst case on
failure is exactly the status quo before this fix: a future
`SyncWithBase` catches the same dirty tree and pauses with a message a
human can still act on.

Call it as the first line of each of `DecisionNoChange`, `DecisionAsk`,
`DecisionFail`, and `DecisionRetryCI` in `invokeForEvent`
(`internal/refactorsweep/workflow.go`) — before any of their existing
pause/reply/retry logic runs. `DecisionDone`/`DecisionContinue` are
untouched; they already have their own, more detailed version of this
same check (which also handles discussion-resolution and messaging
based on whether work was found), so adding the generic helper there
too would just double the git calls.

## Alternatives considered

- **Fail the turn instead of silently committing** when a
  NoChange/Ask/Fail/RetryCI turn leaves a dirty tree, forcing a pause
  with a clear "unexpected leftover changes" message. Rejected: the
  edits observed live were legitimate, wanted work (e.g. real test
  coverage) — discarding or blocking on them would throw away good
  output just because the runner's own summary of the turn didn't
  frame it as "the deliverable." Committing it forward costs nothing
  and is strictly better than either dropping it or pausing on it.
- **Only check once, at the very top of `invokeForEvent`, before the
  switch** rather than per-branch. Rejected: `DecisionDone`/`Continue`
  need their own hasWork result to decide the "no changes needed"
  reply vs. the "addressed"/"partial progress" reply — running the
  check again beforehand would either duplicate the git calls or force
  a larger refactor threading the result through every branch. Calling
  it explicitly in just the four branches that don't already have it
  is the smaller, more localized change.

## Consequences

- A runner turn that edits files but doesn't conclude with
  Continue/Done can no longer leave that edit stranded — it's
  committed and pushed regardless of which of the five decisions comes
  back, closing the whole class of bug ADR-0066 only partially closed.
- `DecisionNoChange`'s webhook-loop guard (no top-level comment posted)
  is unaffected — the safety-net commit/push doesn't post anything on
  its own; only the branch's existing reply logic decides whether to
  comment.
- This remains prompt-independent and structural: it doesn't rely on
  the runner behaving any particular way, unlike ADR-0073's git
  guidance which is advisory. If a decision path is ever added to this
  switch in the future, it should call `commitStrayWork` too unless
  there's a specific reason not to (e.g. the unit's worktree is about
  to be removed anyway, as in `work()`'s `DecisionFail`/`NoChange`
  branches, where `RemoveWorktree` makes any leftover dirt moot).
