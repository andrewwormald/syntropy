# ADR-0100: `syntropy config check` reports the effective spec tool

**Status**: Accepted
**Date**: 2026-08-04

## Context

[ADR-0051](0051-setup-runner-model-config.md) added a global default
`spec_tool` (`~/.syntropy/config.yaml`), and
[ADR-0099](0099-repo-spec-tool-override-excluded-from-missing-fields.md)
added an optional per-repo override (`.syntropy.yml`'s `spec_tool`) that
falls back to the global default when unset. ADR-0099 explicitly deferred
"wiring the actual fallback ... and threading it into whatever eventually
consumes it" to a later increment — persistence existed, but nothing
resolved the two layers into the one answer an agent actually needs:
which spec tool to route creation/viewing to for *this* repo, right now.

`syntropy config check` (ADR-0083) is already the place an agent
following the syntropy Skill goes before writing a spec, specifically so
it doesn't have to open and reason about `.syntropy.yml` itself. Spec
tool resolution has the same shape as the title-convention check it
already performs: a cheap, deterministic lookup with no need for an LLM
turn to re-derive it every time.

## Decision

`RepoConfig` gains `EffectiveSpecTool(globalDefault string) string`
(`internal/setup/titleconvention.go`): returns the repo override if set,
else `globalDefault`. Unlike `EffectiveTitleConvention`, there's no
`BlankSentinel` to unwrap — ADR-0099 already established that an absent
`spec_tool` is unambiguous (always "use the global default"), so no
three-state logic is needed here.

`checkRepoConfig` (`main.go`) now also loads `~/.syntropy/config.yaml`
via `config.Load` and prints one extra line, independent of the
missing-fields exit code:

- `Spec tool: <value> (repo override)` — `.syntropy.yml` sets `spec_tool`.
- `Spec tool: <value> (global default)` — no repo override, but
  `~/.syntropy/config.yaml` has one.
- `Spec tool: (none set — syntropy's own default spec flow)` — neither is
  set.

SKILL.md's basic-flow step 1 now tells the agent to read this line
straight off `config check`'s output and route accordingly, the same
"don't open the YAML/config yourself" discipline ADR-0083 established for
the missing-fields check.

## Alternatives considered

- **A separate `syntropy config spec-tool` subcommand.** Rejected: the
  agent already runs `config check` unconditionally before every spec
  trigger (ADR-0083); a second command would just be one more thing to
  remember to call, for a lookup cheap enough to always print alongside
  the first.
- **JSON/structured output.** Rejected for the same reason ADR-0083
  rejected it for the missing-fields list: the current human/agent-
  readable text is simple enough for an agent to parse without a schema;
  revisit if a future consumer needs more than "which tool, and why."

## Consequences

- `checkRepoConfig` now depends on `os.UserHomeDir()` and
  `internal/config.Load`, not just the repo-local YAML — tests that call
  it must set `$HOME` to a temp dir to stay hermetic (existing
  `TestCheckRepoConfig_*` tests were updated to do so).
- Actually making a runner *use* `EffectiveSpecTool`'s answer (routing a
  triggered run's spec-creation step through the named external tool
  instead of syntropy's own flow) is still out of scope — this ADR only
  covers surfacing the resolved value to the agent driving the
  conversation, same split ADR-0099 drew between persistence and
  consumption.
