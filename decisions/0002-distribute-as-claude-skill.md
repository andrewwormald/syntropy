# ADR-0002: Distribute as a Claude Code Skill + Go binary

**Status**: Superseded by [ADR-0117](0117-syntropy-as-a-skill-with-bundled-scripts.md)
**Date**: 2026-06-15

## Context

Given [ADR-0001](0001-orchestrator-not-replacement.md) — everflow augments
existing AI coding tools rather than replacing them — we needed a distribution
mechanism that requires zero behavior change from end users.

## Decision

Distribute as two artifacts:

1. **`everflow` Go binary** on the user's `$PATH` (installable via
   `go install github.com/andrewwormald/everflow@latest` or a release archive)
2. **A Claude Code Skill bundle** at `~/.claude/skills/everflow/SKILL.md` that
   tells Claude Code when and how to invoke the binary

Claude Code loads the Skill automatically. When the user asks for something
long-running ("babysit my MRs", "keep this PR green overnight"), Claude reads
the Skill and shells out to `everflow start --skill ... --interval ...`.

The Skill is the *integration*, not the *agent*. It is plain markdown
instructions to Claude Code, plus example commands; it contains no executable
logic of its own.

## Alternatives considered

- **MCP server** — expose everflow as a Model Context Protocol server.
  Heavier integration (long-lived process per Claude Code session,
  authentication handshakes), and MCP is not yet available on every runner
  we want to support. May revisit for v2.
- **Claude Agent SDK binding** — a Go SDK that links into Claude Code's
  runtime directly. Tighter coupling, narrower compatibility (Claude only).
- **No Skill — user invokes `everflow` manually** — works, but loses the
  "Claude knows when to use this" property that makes it actually useful.

## Consequences

- Adding support for a new runner (Qwen, OpenHands, ...) needs *both* a
  Runner adapter ([ADR-0007](0007-pluggable-runner-interface.md)) *and* a
  Skill-equivalent in that runner's distribution format.
- The Skill is part of the project surface; it lives in this repo and is
  versioned alongside the binary.
- Users without the Skill installed can still drive everflow manually; they
  just don't get the "Claude figures out when to invoke it" magic.
