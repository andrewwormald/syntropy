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
the runner may have self-committed). **It deliberately does not push.**
Entirely best-effort — any error is swallowed rather than surfaced, so
it never interferes with the decision's own pause/reply/messaging
logic. Worst case on failure is exactly the status quo before this
fix: a future `SyncWithBase` catches the same dirty tree and pauses
with a message a human can still act on.

Call it as the first line of each of `DecisionNoChange`, `DecisionAsk`,
`DecisionFail`, and `DecisionRetryCI` in `invokeForEvent`
(`internal/refactorsweep/workflow.go`) — before any of their existing
pause/reply/retry logic runs. `DecisionDone`/`DecisionContinue` are
untouched; they already have their own, more detailed version of this
same check (which also handles discussion-resolution and messaging
based on whether work was found, and does push), so adding the generic
helper there too would just double the git calls.

**Commit only, never push, for these four decisions.** The first draft
of this fix pushed unconditionally, matching Continue/Done. On review,
that's wrong: Continue/Done are the runner explicitly saying "this is
shippable," which is the only signal syntropy has that a change is
safe to expose on the reviewable branch. NoChange/Ask/Fail/RetryCI are
the opposite signal — the runner gave up, is blocked on a question, or
judged the failure as unrelated to code — so any stray edit sitting in
the worktree has no such endorsement and no build/test verification
behind it. Pushing it anyway would trade "worktree stuck dirty" for
"unverified, possibly-broken code lands on the MR and reviewers/CI see
it" — a worse failure mode, not a better one. A local commit already
fully satisfies `SyncWithBase` (ADR-0046), which only checks for a
dirty tree, not push status — so the fix doesn't need to push to work.
Nothing is lost either: the commit persists in the worktree, and the
next Continue/Done turn's own `HasWorkBeyondBase` check will find it
(exactly the "runner self-committed its own work" case that check
already handles) and push it for real once something actually says the
change is shippable.

## Alternatives considered

- **Revert/discard the stray edit** (`git checkout -- .` / `git clean`)
  instead of committing it, on the theory that a decision other than
  Continue/Done means the runner didn't intend to keep it. Rejected:
  the live incident that prompted this ADR was exactly a case where the
  stray edit was real, wanted work (a genuine test-coverage extension)
  that a human manually verified and committed by hand during the
  rescue — discarding it automatically would have thrown that away.
  Committing locally (without pushing) gets the safety of "nothing is
  silently lost" without the risk of "unverified code reaches the
  remote branch."
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
  committed locally regardless of which of the five decisions comes
  back, closing the whole class of bug ADR-0066 only partially closed.
  It isn't pushed until a later Continue/Done turn (or a manual
  `/syntropy retry` that produces one) actually finds it via
  `HasWorkBeyondBase` and ships it.
- `DecisionNoChange`'s webhook-loop guard (no top-level comment posted)
  is unaffected — the safety-net commit doesn't post anything or touch
  the remote on its own; only the branch's existing reply logic decides
  whether to comment.
- A run that gets abandoned/skipped while sitting on one of these
  local-only commits simply drops them along with the rest of the
  worktree — they were never pushed, so nothing needs cleaning up
  upstream.
- This remains prompt-independent and structural: it doesn't rely on
  the runner behaving any particular way, unlike ADR-0073's git
  guidance which is advisory. If a decision path is ever added to this
  switch in the future, it should call `commitStrayWork` too unless
  there's a specific reason not to (e.g. the unit's worktree is about
  to be removed anyway, as in `work()`'s `DecisionFail`/`NoChange`
  branches, where `RemoveWorktree` makes any leftover dirt moot).
