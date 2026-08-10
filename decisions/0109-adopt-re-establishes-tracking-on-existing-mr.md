# ADR-0109: `syntropy adopt` re-establishes tracking on an existing MR

**Status**: Accepted
**Date**: 2026-08-10

## Context

A Run's live-tracking state (webhook registration, `InFlight`, comment/CI
event handling) lives in a single sqlite record. If that record is lost —
store corruption, a botched migration, a human deleting the wrong row —
the MR/PR itself is untouched: the branch, the commits, the open MR all
still exist upstream. Before this feature, the only recovery was to start
a brand-new Run, which would create a second branch and duplicate work
already done, or push a new commit onto a branch nobody is watching.

`syntropy adopt` (`main.go`, `cmdAdopt`) exists to re-attach live tracking
to that already-open MR without redoing any of it. Getting there took four
increments, landed without a written record as they went:

1. Cross-Run scan (`findRunTrackingMR`) to refuse adopting an MR that's
   already tracked by a live Run — otherwise two Runs would race on the
   same webhook/poll events.
2. `InFlight` pre-populated at trigger time (`AdoptUnitID`/`AdoptMRIID`/
   `AdoptMRBranch`) routes straight to `StatusAwaitingMerge`, skipping
   `Discovering`/`Working` entirely — there's nothing to discover or plan,
   the MR already exists.
3. `setup()`'s adopt branch checks out the MR's *actual* existing branch
   via `CheckoutExistingBranch`, not `EnsureBranch` (which would create a
   new branch off base) — the worktree must reflect the real, already-
   in-progress work.
4. A comment posted on the MR itself announcing the adoption.

This ADR is written now, retroactively, per ADR-0012 (every meaningful
decision gets a written record) — it covers the whole feature, including
the three earlier increments, not just the comment added in this one.

## Decision

`syntropy adopt` re-establishes tracking by: verifying the MR is open and
not already tracked elsewhere, triggering a Run with `InFlight` and
`CurrentUnit` pre-populated, checking out the MR's real branch instead of
creating one, and posting a visible marker comment on the MR:

> 🔗 Adopted by run `<runID>` — live tracking (comment reactions, CI
> events, merge/close detection) resumes from here.

The comment matters because the CLI operator running `syntropy adopt` is
not necessarily the only person watching the MR. Anyone else on the
thread — reviewers, the original author — has no other visibility into
"this MR just came back under automated tracking, its behavior is about
to change again." The comment is posted in `setup()` after the checkout
succeeds and before the Run reaches `StatusAwaitingMerge`; if `PostComment`
fails, `setup()` returns `StatusFailed` (retried like any other setup
error, per `stepErrBackOff`/`stepPauseAfterErrCount`) rather than silently
skipping the notice.

## Alternatives considered

- **No comment, rely on `syntropy status`** — cheaper, but only surfaces
  to whoever thinks to run the CLI. Everyone reading the MR thread stays
  in the dark. Rejected: the whole point of adopt is to restore *visible*
  live tracking, and a silent takeover undercuts that.
- **Post the comment from the `adopt` CLI command itself, before
  triggering** — would fire even if the subsequent trigger/setup fails,
  leaving a stale "adopted" claim with no Run backing it. Posting from
  inside `setup()`, after the checkout succeeds, ties the comment to
  tracking actually being live.

## Consequences

- The adopt path in `setup()` now has three failure points (checkout,
  comment, plus the pre-existing webhook/provider setup already shared
  with the non-adopt path) — all treated uniformly as `StatusFailed` and
  retried by the existing step-level backoff.
- Adopting an MR always leaves a paper trail on the MR itself, not just
  in Everflow's own store — useful for the same "agent picks this up cold"
  reasoning ADR-0012 already applies to the repo.
