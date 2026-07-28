# ADR-0082: `.syntropy.yml` fields distinguish "never asked" from "asked, declined"

**Status**: Accepted
**Date**: 2026-07-28

## Context

The SKILL.md-driven setup flow (added after a real incident where
`.syntropy.yml` silently never got written for a target repo) has agents
check the target repo's `.syntropy.yml` before triggering a spec, and
walk the user through a one-off setup conversation for any missing
field.

That flow has a gap: if the user is asked for a field (e.g.
`title_convention`) and explicitly says they don't want to set one, there
was no way to record that decision. Writing `""` or simply not writing
the field at all is indistinguishable from "never asked" — which means
either the field gets silently dropped (an agent that checks "is this
non-empty" sees nothing and might ask again next time), or worse, once
syntropy adds a *second* config field in a later version, an existing
repo's `.syntropy.yml` — written when only `title_convention` existed —
would have that new field genuinely absent, and there'd be no way to
tell that apart from "the user considered this field and declined it,"
if declining were ever represented the same way (empty/absent).

Direct instruction from the user: record an explicit "blank" marker when
the user chooses to leave a field unset, specifically so this
distinction survives forward as syntropy adds new fields over time — a
new field should read as "never asked" (prompt the user) on an *existing*
`.syntropy.yml` that predates it, while a field the user was asked about
and declined should read as "already decided" (don't prompt again) even
though its effective value is empty either way.

## Decision

`internal/setup/titleconvention.go` gets a sentinel constant,
`BlankSentinel = "blank"`, plus two methods on `RepoConfig`:

- `IsConfigured() bool` — true iff the raw field is non-empty, i.e. either
  a real value or `BlankSentinel`. False means "never asked" — an agent
  following the syntropy Skill should prompt for it.
- `EffectiveTitleConvention() string` — the value to actually use
  anywhere the convention matters (today: threading into the runner's
  prompt). Returns `""` for both the zero value and `BlankSentinel`, so
  the literal word "blank" never leaks into a prompt as if it were a real
  convention.

`internal/refactorsweep/workflow.go`'s `setup()` now calls
`cfg.EffectiveTitleConvention()` instead of reading `cfg.TitleConvention`
directly when populating `AgentState.TitleConvention`.

SKILL.md's setup-conversation step is rewritten to describe this as a
**general per-field protocol**, not something specific to
`title_convention` — worded so it applies unchanged as new fields are
added later: for each field this version of syntropy recognises, its
value is either (a) a real value — use it, (b) the literal `blank` —
already decided, don't ask again, treat as no value, or (c) absent
entirely — never asked, prompt now, and write either the real answer or
`syntropy setup --title-convention blank` depending on what the user
says. No code change was needed for `WriteRepoConfig`/`syntropy setup`
to support writing the sentinel — passing `blank` as the
`--title-convention` value already works today, since it's a normal
non-empty string as far as that path is concerned; the only place that
needed to treat it specially is the read side.

## Alternatives considered

- **Use `*string` (pointer) fields** to distinguish nil (absent) from a
  pointed-to empty string (explicit blank) at the type level, instead of
  a sentinel string value. Rejected: YAML round-trips a `*string` less
  legibly for a human hand-editing the file (`title_convention: null` vs
  simply omitting the line), and the sentinel approach needs no change to
  `WriteRepoConfig`'s existing signature or the CLI flag's string type —
  a pointer would ripple through more of the existing plumbing for the
  same outcome.
- **A separate `_configured_fields: [...]` list in the YAML** tracking
  which fields have been decided, independent of their values. Rejected:
  more moving parts than necessary — a single sentinel value per field is
  simpler to reason about and to hand-edit, and scales the same way (one
  sentinel check per field) as the fields-list approach would.

## Consequences

- Every future config field added to `RepoConfig` should follow the same
  pattern: a sentinel-aware "effective value" accessor for wherever the
  value is actually used, and `IsConfigured()`-style logic (or an
  equivalent) for whether an agent should still ask. This ADR documents
  the pattern once rather than requiring each new field's own ADR to
  re-derive it.
- SKILL.md's protocol description is intentionally generic ("for each
  field this version of syntropy recognises") rather than enumerating
  `title_convention` specifically, so it doesn't need editing every time
  a new field is added — only the field list itself (which the agent is
  told to check for itself rather than trust the doc to enumerate).
- A `.syntropy.yml` hand-written before this ADR (just
  `title_convention: <value>` or nothing) still works unchanged — this is
  purely additive; no migration needed for existing files.
