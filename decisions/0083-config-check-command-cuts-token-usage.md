# ADR-0083: `syntropy config check` — pure-code repo-config check, agent only for the gap

**Status**: Accepted
**Date**: 2026-07-28

## Context

The SKILL.md flow that has agents verify `.syntropy.yml` before
triggering a spec (added after the incident where the file silently
never landed in the target repo) originally had the agent open the file
itself and reason through each field's three-state logic (real value /
`BlankSentinel` / absent — ADR-0082) on every single spec trigger, even
when nothing was actually missing.

Direct instruction from the user: cut token usage wherever a deterministic
check can replace an agent's reasoning. Whether `.syntropy.yml` has every
recognised field configured is exactly that kind of check — a file read
plus a few string comparisons, with only one real branch (something is
missing) that ever needs judgment or a conversational ask. Paying for an
LLM turn to re-derive "is this file already fine" on every trigger, when
it almost always is, is waste.

## Decision

Add `syntropy config check [--repo <dir>]`
(`main.go`'s `cmdConfig`/`checkRepoConfig`): reads `.syntropy.yml` via
`setup.ReadRepoConfig`, calls the new `setup.MissingFields(cfg)` (a plain
function returning field names not yet configured — extensible: add a
case there for every new `RepoConfig` field), and prints either
`OK: ...` or `MISSING: ...` plus the field list. Exit code doubles as the
machine-readable signal: `0` = fully configured, `1` = something's
missing.

SKILL.md's setup-check step now tells the agent to run this command and
act on its exit code/output — explicitly **not** to open or reason about
the YAML file itself. Only when the command reports missing fields does
the agent spend a conversational turn, and only on the fields actually
listed.

## Alternatives considered

- **Keep the check entirely inside the agent's own reasoning** (the
  pre-existing approach). Rejected per the user's explicit token-usage
  instruction: this spends a full turn re-deriving a three-line
  conditional's answer on every single spec trigger, the overwhelming
  majority of which end up "nothing to do."
- **Have the daemon itself gate `syntropy start` on this check**, failing
  the trigger outright if config is incomplete, rather than a separate
  `config check` command the agent runs first. Rejected: that would make
  an incomplete config a hard failure with no room for the "ask the user,
  then proceed in the same turn" flow SKILL.md wants — the agent needs to
  see *which* fields are missing before it can usefully ask, and needs to
  be able to write the answer and retry, which a hard gate inside `start`
  doesn't naturally support without becoming a second interactive prompt
  loop.

## Consequences

- Every future `RepoConfig` field must get a corresponding check inside
  `MissingFields` — that's the one place the field list needs to stay
  current, same discipline ADR-0082 already established for
  `EffectiveTitleConvention`/`IsConfigured`.
- This is a genuinely small, mechanical command — most of the actual
  "judgment" work (what should the convention be, does the user want to
  set one) still correctly happens in the agent conversation, only now
  gated on a cheap precondition rather than repeated every time.
- `syntropy config check`'s output is intentionally both human-readable
  and simple enough for an agent to parse without needing a
  machine-structured format (JSON, etc.) — revisit if a future consumer
  needs more than "which fields are missing."
