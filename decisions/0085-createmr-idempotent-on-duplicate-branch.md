# ADR-0085: `CreateMR` absorbs a duplicate-branch conflict as idempotent success

**Status**: Accepted
**Date**: 2026-07-28

## Context

Found live on a real run (`5ec1a36b`, spec mode, concurrency=1,
9-service worklist): increment-2's `work` phase pushed its branch,
called `CreateMR`, and GitLab returned !82143 successfully. Sometime
afterward, `CreateMR` was called *again* for the exact same branch —
plausibly a duplicate `work()` invocation racing the first one to
completion (concurrency=1 bounds how many units are in flight
simultaneously, not whether the same unit's step ever gets invoked
twice, e.g. by a reconciler re-trigger firing while a genuinely
long-running turn was still in progress). GitLab correctly rejected the
second create:

```
gitlab API /api/v4/projects/.../merge_requests: status=409
body={"message":["Another open merge request already exists for this
source branch: !82143"]}
```

`work()` treated any `CreateMR` error as fatal
(`internal/refactorsweep/workflow.go`: `r.Object.LastError = ...; return
StatusFailed, nil`), so the Run terminated in `Failed` — even though
!82143 (the actual migration work) went on to merge cleanly and visibly
on GitLab. The Run's own terminal status was simply wrong: the work was
done and correct, but syntropy reported failure.

## Decision

Both `gitlab.Provider.CreateMR` and `github.Provider.CreateMR` now treat
this specific duplicate-branch conflict as idempotent success instead of
an error:

- **GitLab**: on a 409 response, look up the branch's existing open MR
  (`GET /merge_requests?source_branch=<branch>&state=opened`) and return
  it as if creation had succeeded.
- **GitHub**: on a 422 response whose body mentions "A pull request
  already exists" (GitHub reuses 422 for other validation failures too,
  e.g. no commits between head/base — this check exists specifically so
  those don't get masked), look up the existing PR
  (`GET /pulls?head=<owner>:<branch>&state=open`) the same way.

Both fall back to surfacing the original error unchanged if the status
doesn't match, or if the lookup itself fails or finds nothing — this
must never mask a genuinely different conflict as success.

This fixes the failure mode at its actual source (the provider boundary,
where "does an MR already exist for this exact branch" is a clean,
answerable question) rather than trying to prevent the duplicate
`work()` invocation from ever happening in the first place — the latter
would require solving distributed at-most-once execution for a
long-running external side effect, which is a much larger problem than
this incident calls for.

## Alternatives considered

- **Prevent the duplicate invocation itself** (e.g. a per-unit lock,
  or having the reconciler check more carefully before re-triggering a
  Run whose current step might still be legitimately in flight).
  Rejected for this fix: harder to get right (a lock needs its own
  failure-recovery story — what happens if the daemon crashes while
  holding it), and doesn't fully close the gap anyway — a daemon crash
  and restart between "CreateMR succeeded" and "the Run's AgentState
  persisted the new MR" would produce the identical duplicate-create
  race even with perfect duplicate-invocation prevention, since the
  first invocation's own side effect already landed. Making the
  provider idempotent at the one point that actually has enough
  information to resolve the ambiguity (GitLab/GitHub itself) closes the
  bug regardless of *why* a second `CreateMR` call happened.
- **Retry-with-backoff at the `work()` call site** instead of fixing
  `CreateMr` itself. Rejected: a retry doesn't help here — the *second*
  `CreateMR` call is not a transient failure to retry past, it's a
  structurally guaranteed 409/422 for as long as the branch's MR stays
  open. The fix has to recognize and resolve the conflict, not retry
  through it.

## Consequences

- A duplicate `work()` invocation for a unit that already has an open
  MR now converges correctly on `r.Object.InFlight[unitID] = mr` (the
  existing MR) and proceeds to `StatusAwaitingMerge`, instead of hard-
  failing the whole Run.
- This doesn't change `CreateMR`'s behavior for the common case (no
  existing MR) at all — the extra lookup only runs on the specific
  error shape each provider uses for "duplicate branch."
- If a *different* class of duplicate-invocation bug surfaces later
  (e.g. duplicate `Commit`+`Push` racing on the same branch, not just
  duplicate `CreateMR`), it needs its own idempotency fix at whatever
  provider/git boundary can resolve it — this ADR only covers MR
  creation.
