# ADR-0099: Per-repo `spec_tool` override is excluded from `MissingFields`

**Status**: Accepted
**Date**: 2026-08-04

## Context

A prior increment added a global default `spec_tool` to
`~/.syntropy/config.yaml` (`internal/config.Config.SpecTool`,
`setup.ResolveSpecTool`), mirroring [ADR-0051](0051-setup-runner-model-config.md)'s
`Model` resolution pattern closely enough that it didn't need its own
ADR — so an agent knows whether to route spec creation/viewing to the
user's own tool (e.g. spec-kit) or syntropy's own default flow. The full
feature also calls for an optional per-repo override, living in
`.syntropy.yml` alongside `title_convention`
([ADR-0052](0052-everflow-yml-title-convention.md)).

`title_convention` is a field every repo is expected to have a decided
answer for: `MissingFields` ([ADR-0083](0083-config-check-command-cuts-token-usage.md))
reports it absent so `syntropy config check` (and an agent following the
syntropy Skill) prompts the user for it once, and
[ADR-0082](0082-repo-config-blank-sentinel.md)'s `BlankSentinel` lets the
user explicitly decline without being re-asked forever.

A per-repo `spec_tool` override is a different kind of field: it's
optional by design, with a global default (ADR-0051) as its fallback. A
repo that never sets it isn't in an unconfigured state needing a
follow-up conversation — it's simply using the default, same as most
users won't bother with a repo-specific override at all. Applying
ADR-0082's "never asked → ask now" protocol here would mean every repo's
setup conversation nags the user about a field almost nobody needs to
answer.

## Decision

`RepoConfig` gains `SpecTool string \`yaml:"spec_tool,omitempty"\`` and a
`--repo-spec-tool` flag on `syntropy setup`
(`setup.ResolveRepoSpecTool` mirrors `ResolveTitleConvention`'s
flag-then-prompt-then-empty precedence; `setup.WriteRepoConfig` now takes
both `convention` and `specTool` and is a no-op only when both are
empty). But `MissingFields` gets no `spec_tool` case — it is deliberately
excluded from the "ask the user" protocol ADR-0082 and ADR-0083 built for
`title_convention`. An absent `spec_tool` in `.syntropy.yml` means
exactly one thing: fall back to the global default. There's no
"never asked" vs. "asked, declined" distinction to preserve, so no
sentinel value is needed either.

## Alternatives considered

- **Treat `spec_tool` like `title_convention`** (add it to `MissingFields`,
  give it a blank-sentinel-aware effective-value accessor). Rejected:
  ADR-0082's protocol exists to solve "was this field ever decided,"
  which matters when a field's absence is ambiguous with intent. Here
  absence always means "use the global default" — there's nothing
  ambiguous to disambiguate, and forcing every repo through a setup
  conversation for a field with a working global fallback creates
  friction ADR-0083 was written to avoid.

## Consequences

- Wiring the actual fallback (an `EffectiveSpecTool()`-style accessor
  that resolves repo override → global default, and threading it into
  whatever eventually consumes it) is deferred to a later increment —
  this ADR only covers persistence, not consumption.
- Future config fields should default to ADR-0082's ask/sentinel pattern
  unless, like this one, they have a clear non-repo-level fallback that
  makes "never configured" unambiguous — that's the test for whether a
  field belongs in `MissingFields`.
