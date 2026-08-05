# ADR-0104: `commitStrayWork` becomes `discardStrayWork` — throws away stray edits instead of committing them

**Status**: Accepted (reverses ADR-0074's "commit, don't discard" choice for these four decision branches; ADR-0074's placement — call it first in `DecisionNoChange`/`Ask`/`Fail`/`RetryCI` — is unaffected)
**Date**: 2026-08-05

## Context

ADR-0074 added `(*Deps).commitStrayWork(ctx, worktree, branch, commitMsg)`,
called at the top of `invokeForEvent`'s `DecisionNoChange`/`Ask`/`Fail`/
`RetryCI` branches (`internal/refactorsweep/workflow.go`). Those four
decisions mean the runner didn't ship anything, but can still have left
edited files behind (a partial fix started before asking a question, for
example). ADR-0074 committed that stray edit locally — never pushed — so
it wouldn't sit uncommitted and trip `SyncWithBase`'s (then-refusing)
dirty-worktree guard on some later turn.

[ADR-0103](0103-syncwithbase-discards-instead-of-refusing.md) then changed
`SyncWithBase` itself to discard uncommitted changes rather than refuse.
That closes the same gap `commitStrayWork` was built for, but via the
opposite mechanism — discard instead of preserve — which left two
inconsistent answers to "what happens to a stray edit nothing declared
shippable?" depending on which call path found it first: `SyncWithBase`
throws it away, `commitStrayWork` preserves it as a local commit.

The spec goal behind ADR-0103 is broader than just `SyncWithBase`: the
harness owns every commit a unit ships, so nothing worth keeping should
ever depend on surviving between turns as an uncommitted (or
locally-committed-but-unpushed) edit. A local commit from
`commitStrayWork` is exactly that kind of thing — it isn't reachable from
`origin`, isn't reviewed, isn't covered by CI, and only survives at all
because some later Continue/Done turn happens to notice it via
`HasWorkBeyondBase`. If no such turn ever comes (the unit gets skipped,
abandoned, or blacklisted first), the commit is silently lost anyway —
the "safety" ADR-0074 committing bought was already conditional, not
durable.

## Decision

Rename `commitStrayWork` to `discardStrayWork` and change its
implementation: instead of checking `HasWorkBeyondBase` and calling
`Commit`, it checks `HasChanges` (is the tree dirty?) and calls
`Git.DiscardUncommitted` when it is. `HasChanges` replaces
`HasWorkBeyondBase` here because the thing being cleaned up is
specifically uncommitted mess — a runner that self-committed its own
work (clean tree, commits ahead of base) has nothing for
`DiscardUncommitted` to do, so gating on `HasChanges` avoids a wasted
git call in that case.

The four call sites (`DecisionNoChange`, `DecisionAsk`, `DecisionFail`,
`DecisionRetryCI` in `invokeForEvent`) are otherwise unchanged — same
position at the top of each branch, same best-effort error swallowing.
`branch`/`commitMsg` arguments are dropped from the signature since
discarding needs neither.

## Alternatives considered

- **Leave `commitStrayWork` as-is.** Rejected: it's now the one remaining
  place in the harness that preserves an edit nothing declared shippable
  by committing it locally, unpushed — an inconsistent exception to the
  discard rule ADR-0103 just established, and one whose "safety" already
  depended on a later turn happening to exist.
- **Keep both a commit path and a discard path, and let the caller pick.**
  Rejected: identical branches (NoChange/Ask/Fail/RetryCI) would need a
  reason to choose one over the other, and there isn't one — all four
  already share the same "runner said this isn't shippable" premise that
  makes discarding the right call in every case.

## Consequences

- `TestResume_NoteAdded_DecisionAsk_CommitsStrayWorkLocallyBeforePausing`,
  `..._DecisionFail_CommitsStrayWorkLocallyBeforePausing`, and
  `..._DecisionNoChange_CommitsStrayWorkLocally` are renamed to
  `..._DiscardsStrayWorkBeforePausing` / `..._DiscardsStrayWork` and now
  assert `g.discards` has one entry and `g.commits` stays empty (previously
  the reverse).
- `TestResume_NoteAdded_DecisionNoChange_NoStrayWork_DoesNotCommit` is
  renamed to `..._DoesNotDiscard` and additionally asserts no discard
  happened, not just no commit/push.
- A runner turn that leaves a partial edit behind on one of these four
  decisions now loses that edit outright the moment the branch runs —
  there is no later turn that can recover it, unlike ADR-0074's
  local-commit approach. This is the intended tradeoff per the same spec
  goal ADR-0103 already accepted: the harness owns every commit, so any
  edit the runner actually wants kept must be shipped via Done/Continue,
  not left for a safety net to preserve.
