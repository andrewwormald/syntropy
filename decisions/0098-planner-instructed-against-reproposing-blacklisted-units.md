# ADR-0098: Explicitly instruct the planner not to re-propose blacklisted units

**Status**: Accepted
**Date**: 2026-08-04

## Context

ADR-adjacent prior work (`fix(refactorsweep): surface blacklist reasons in
the planning prompt`) fixed `buildPlanningPrompt` so each `BlacklistedUnit`'s
actual `Reason` is rendered in the "Blacklisted units" section, instead of
collapsing the whole list to a bare count. That gave the planner the
*information* needed to tell an explicit human rejection (e.g. via
`/syntropy skip: this breaks the auth flow`) apart from a routine "no
changes needed" blacklist.

Information alone doesn't constrain behavior. The "Your task" section —
the part of the prompt that actually tells the planner what to decide —
said nothing about the blacklist at all; it only described the
Decision=Continue/Done/Ask/Fail contract. Nothing stopped the planner from
reading a blacklisted unit's reason, disregarding it, and proposing the
same increment again under a new `unit-N` ID: `discoverSpec` mints a fresh
ID per Continue decision (`workflow.go`, `discoverSpec`), so a re-proposed
duplicate looks like brand-new work with no structural signal tying it back
to the rejection.

## Decision

`buildPlanningPrompt` (`internal/refactorsweep/workflow.go`) adds an
explicit instruction to the "Your task" section:

> Do not propose an increment that duplicates a blacklisted unit above
> unless its Reason no longer applies (e.g. it was blacklisted for a cause
> that's since been fixed) — an explicit human rejection must not be
> silently re-proposed as a "new" increment.

This sits between the task framing and the Decision contract, so it reads
as a constraint on *how* to decide rather than a fifth decision type. The
"unless its Reason no longer applies" carve-out is intentional: a unit
blacklisted for "no changes needed" (the sweep-mode default when a runner
returns Done with a clean worktree) can legitimately become actionable
again if upstream code changes; the instruction targets silent duplication
of *rejected* work, not permanent exclusion of the unit ID.

## Alternatives considered

- **Rely on the surfaced Reason alone, no explicit instruction** — the
  status quo prior to this ADR. Leaves the planner to infer "don't
  duplicate this" from context; an instruction-following model doesn't
  reliably treat descriptive history as a behavioral constraint unless
  told to.
- **Enforce it in code** (reject/filter a Decision=Continue whose
  rationale closely matches a blacklisted unit) — more robust in
  principle, but rationale-similarity matching is fuzzy (paraphrased
  re-proposals wouldn't match a string/substring check) and risks
  false-positive rejections of legitimately-changed follow-up work. A
  prompt-level instruction, backed by the Reason already being visible, is
  the cheaper first line of defense; a code-level guard is a candidate
  follow-up if this proves insufficient in practice.
- **Drop the blacklisted unit's history entirely once "rejected"** — would
  prevent duplication by omission, but destroys the Reason needed for the
  "unless no longer applies" carve-out and any post-hoc audit of why a
  unit was skipped.

## Consequences

- The planner now sees both the *signal* (per-unit `Reason`) and the
  *rule* (don't duplicate without cause) in the same prompt, closing the
  gap the prior increment left open.
- This is prompt-level guidance, not an enforced constraint — a
  non-compliant planner run can still re-propose a blacklisted unit; there
  is no code path that rejects it. Acceptable for now given the
  planner is a single Claude-driven decision point already trusted for the
  Decision=Continue/Done/Ask/Fail contract itself.
- No new test infra: `TestBuildPlanningPrompt_InstructsAgainstReproposingBlacklisted`
  asserts the instruction text is present in the rendered prompt, alongside
  the existing `TestBuildPlanningPrompt_SurfacesBlacklistReason`.
