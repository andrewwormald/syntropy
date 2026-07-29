# ADR-0091: Dedicated `<syntropy-title-update>`/`<syntropy-description-update>` tags, applied by the harness

**Status**: Accepted
**Date**: 2026-07-29

## Context

The runner already has one mechanism for changing an MR's title: the
`done: <title>` suffix on the `<syntropy-decision>` marker (ADR-0052/
ADR-0054). That only fires once, on the final Decision=Done turn, and it
covers only the title — there's no equivalent for the description, and no
way to fix either mid-flight on a Continue/Ask turn. Today, if the runner
notices the MR title or description is stale or wrong partway through a
unit, its only recourse is to ask a human to hand-edit MR metadata it's
already authorized to fix itself.

## Decision

Add two new tags, independent of the decision-marker protocol, that the
runner can emit on any turn: `<syntropy-title-update>` and
`<syntropy-description-update>`. Each is parsed by its own regex/function
(`ParseTitleUpdate`/`ParseDescriptionUpdate` in
`internal/runner/claude/claude.go`), mirroring `decisionRE`'s own-line-
anchored, last-match-wins style so an incidental mention in prose can't
hijack them. The description tag's content is allowed to span multiple
lines (an MR description is rarely one line); the title tag's is not.

The parsed values thread into two new `runner.Response` fields,
`TitleUpdate`/`DescriptionUpdate`, populated on every `Run()` return path
regardless of `Decision` — these are a separate protocol from
`ParseDecision`, not a variant of it. The existing `Title`/`done:` mechanism
is untouched.

This increment is parsing-only: it does not teach the prompt to mention the
new tags, and nothing yet reads `TitleUpdate`/`DescriptionUpdate` to
actually call the MR provider. Applying the update remains the harness's
job (never the runner's, consistent with ADR-0073's "harness owns every
push" precedent) — that wiring is deliberately deferred to a follow-up
increment so this one stays a single, reviewable concern.

## Alternatives considered

- **Fold into the decision marker** (e.g. `<syntropy-decision>continue;
  title: ...; description: ...</syntropy-decision>`): rejected — the
  decision marker's grammar is verb + optional single-line `rest`; cramming
  a multi-line description into that would either break the "must be alone
  on its line" constraint (ADR-0056) or require a much more complex
  sub-grammar. Separate tags keep both protocols simple.
- **A generic/parameterized tag-extraction helper** (`parseTag(name,
  out)`): considered, since a second and third tag now exist. Deferred —
  with three tags total and different content shapes (single-line verb+rest
  vs. free-text single-line vs. free-text multi-line), a generic abstraction
  would need parameters for most of what it's trying to save; two small
  sibling functions following the established `decisionRE` pattern stay
  easier to read.

## Consequences

- The runner can now (once prompt wiring lands) request MR metadata fixes
  without pausing for a human, on any turn.
- `runner.Response` gains two more fields that every `Runner` implementation
  should populate (today, only the `claude` adapter does; other runners
  default to zero values, same as any other Response field).
- Follow-up work: teach `BuildPrompt`/`decisionProtocol` about the new tags,
  and wire `TitleUpdate`/`DescriptionUpdate` into the workflow step bodies
  that call the MR provider's update APIs.
