# ADR-0108: Skill-authored prose follows ASD-STE100 (Simplified Technical English)

**Status**: Accepted
**Date**: 2026-08-07

## Context

`internal/setup/SKILL.md` drives an agent through triggering, reviewing,
and reporting on a `syntropy` Run. That agent writes prose on the user's
behalf throughout the flow: the spec itself (Step 3's presentation), unit
descriptions, MR/PR titles and descriptions, and status updates back to
the human. Nothing in the Skill file said how that prose should read.
Left unconstrained, wording drifts across runs — passive voice in one
spec, a noun cluster in another, inconsistent terms for the same concept —
which makes specs harder to skim and harder to review quickly.

A separate skill, `my-writing-style`, already defines ASD-STE100
(Simplified Technical English) rules for exactly this purpose: one idea
per sentence, active voice, present tense, controlled vocabulary, no
ambiguous pronouns, technical literals kept verbatim. That skill is
opt-in — an agent only applies it when a user names it explicitly.

## Decision

Add a `## Writing style` section to `internal/setup/SKILL.md`, placed
between `## Presentation mechanism` and `## Basic flow`. The section
states the same ASD-STE100 rules inline, and applies them unconditionally
to every spec, unit description, MR/PR title and description, and status
update the Skill's agent writes during the flow — not only when a user
asks for this style by name. Code, commands, paths, and identifiers stay
verbatim, per the standard's own carve-out.

The rules are duplicated inline rather than referenced by pointing at
`my-writing-style`, because the Skill file must stand alone: an agent
running this flow should not need a second skill invocation just to know
how to phrase a spec.

## Alternatives considered

- **Reference `my-writing-style` by name instead of inlining the rules.**
  Rejected: it would make Skill behavior depend on a second skill being
  installed and invoked, and this Skill file is meant to be self-contained
  (see `## Presentation mechanism`'s own inline, non-referential style).
- **Leave writing style unspecified and rely on model default phrasing.**
  Rejected: this is exactly the drift the section exists to prevent —
  specs and status updates should read consistently across Runs and
  across different agents driving the same Skill.

## Consequences

- Specs, unit descriptions, MR/PR titles and descriptions, and status
  updates produced by this Skill's flow must follow ASD-STE100: one idea
  per sentence, active voice, present tense, controlled vocabulary, no
  ambiguous pronouns, technical literals verbatim.
- If `my-writing-style`'s rules change, `internal/setup/SKILL.md`'s
  `## Writing style` section needs a matching update, since the two are
  now two independent copies of the same rule set.
