# ADR-0089: `DecisionNoChange` replies to a human comment, stays silent on CI events

**Status**: Accepted
**Date**: 2026-07-28

## Context

Found live: a real reviewer comment — `/syntropy why remove windows
support?` — got a full runner turn (the runner read the question,
confirmed Windows had already been dropped in a prior commit, and
produced a real explanatory summary) but no reply ever reached GitHub.
The run's own status showed no error and no pause — everything looked
fine from the operator's side, yet the human who asked got silence.

Root cause: `invokeForEvent`'s `DecisionNoChange` branch
(`internal/refactorsweep/workflow.go`) has always posted nothing,
citing "that would itself trigger a webhook and risk a loop." That
concern was valid when this code was first written, but
`isOwnEcho`/`RecentOutgoingHashes` (ADR-0035, added later) already fully
solves the self-comment-loop problem for every other reply path in this
file — the daemon's own posted comments are hashed and dropped on
re-ingestion before they ever reach the runner again. `DecisionNoChange`
was simply never updated to rely on that mechanism once it existed, so
it kept the older, blunter fix (never reply at all) long after the
better one made it unnecessary.

## Decision

`DecisionNoChange` now checks `ev.Kind`:

- **`EventNoteAdded`** (a real human comment): post the runner's
  `resp.Summary` as an informational reply — the exact same
  `"ℹ️ %s: %s\n\n(No code changes were needed.)"` shape the
  `Done`+`!hasWork` path already uses elsewhere in this file — and
  best-effort resolve the originating discussion thread, matching that
  path too. `commitStrayWork` (ADR-0074) still runs first, unchanged.
- **`EventPipelineFailed`** (fix_ci): stays silent, unchanged. A CI
  failure judged as needing no code change can recur every retry;
  posting "no changes needed" on every occurrence would be exactly the
  kind of repetitive, human-unrequested noise the original blanket
  silence was trying to avoid — that concern is still legitimate for an
  automated, potentially-recurring trigger, just not for a one-off human
  question.

## Alternatives considered

- **Remove the silence entirely, reply on every `DecisionNoChange`
  regardless of `ev.Kind`.** Rejected: fix_ci's repeat-failure case is a
  real, different noise concern than address_comment's one-shot human
  question — collapsing the distinction would trade one bug (silence on
  real questions) for another (spam on recurring CI no-ops).
- **Leave `DecisionNoChange` silent everywhere and instead push the
  runner toward `DecisionAsk`/`Done` for anything resembling a
  question.** Rejected: this pushes a workaround onto every future
  prompt-tuning effort to route around a structural gap in the decision
  handling, rather than fixing the actual bug (a decision that
  legitimately fits "nothing to change" still deserves a reply when a
  human asked something).

## Consequences

- A genuine human question that the runner correctly judges as needing
  no code change now gets an actual answer instead of silent
  non-response — closes the exact gap found live.
- fix_ci's existing no-comment behavior for repeat transient failures is
  unchanged.
- Any future decision-handling path that currently stays silent "to
  avoid a loop" should be checked against the same reasoning this ADR
  applies: is the loop concern still live given ADR-0035's echo
  suppression, or is the silence now costing more (unanswered humans)
  than it protects against?
