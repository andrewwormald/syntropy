# ADR-0114: `syntropy config check` preflights OpenHands' undeclared runtime dependencies

**Status**: Accepted
**Date**: 2026-09-14

## Context

README's "Enabling OpenHands (opt-in)" section (added alongside
[ADR-0112](0112-openhands-agent-server-runner-design.md)/[ADR-0113](0113-openhands-agent-server-schema-corrections.md))
documents that a plain `uv tool install openhands-agent-server` is not
enough: `agent-server` also needs the `libtmux` and `openhands-tools`
Python packages and the system `tmux` binary, none of which a plain
install pulls in. Before this change, nothing in `syntropy` checked for
any of these — a repo that opted into `runner: openhands` without having
followed that install step would only find out via a cryptic subprocess
failure buried in `agent-server`'s own stderr, partway through a real Run.

`syntropy config check` (`checkRepoConfig` in `main.go`, ADR-0083) already
reports the openhands runner's registration, default model, and whether
LLM credentials are configured — the natural place to also report these
three dependencies, since it's the command a spec-following agent already
runs before triggering work.

## Decision

**`Runner.MissingDependencies()` (`internal/runner/openhands/preflight.go`)
checks tmux, libtmux, and openhands-tools, and `checkRepoConfig` prints the
result whenever the openhands runner is registered.**

- `tmux` is checked via `exec.LookPath("tmux")` — it's a system binary, not
  a Python package, so PATH resolution is the correct and only check.
- `libtmux` and `openhands-tools` are Python packages that live inside
  whichever interpreter `agent-server`'s console-script actually runs
  under — not necessarily the system `python3`, since `uv tool install`
  creates an isolated venv per tool. `MissingDependencies` resolves
  `r.Binary` on PATH, reads its `#!` shebang line to find that
  interpreter (handling both a direct interpreter path and an
  `#!/usr/bin/env python3`-style indirection), and shells out to it with
  `python -c "import <module>"` per package. A failing import means the
  package is missing; the printed remediation is the same `uv tool
  install ... --with libtmux --with openhands-tools` command the README
  already gives.
- If `r.Binary` itself isn't resolvable on PATH at all, `MissingDependencies`
  returns an error rather than a missing-item string — `checkRepoConfig`
  reports this as "could not check" rather than folding it into the
  missing list, since "can't confirm" and "confirmed missing" are
  different findings worth telling apart.
- This is report-only: unlike `.syntropy.yml`'s missing-fields list, a
  missing OpenHands dependency does not affect `config check`'s exit
  code. Nothing about picking `runner: openhands` in a spec required
  these dependencies to already be installed before this check existed,
  and `config check`'s exit code is consumed as a machine-readable
  "spec fields need asking about" signal (ADR-0083) — overloading it with
  an unrelated environment-readiness signal would break that contract.

## Alternatives considered

- **Just try to import the packages with the system `python3`** —
  rejected. `agent-server` runs inside its own `uv tool install` venv;
  the system interpreter has no relationship to what packages that venv
  actually has installed. Checking the wrong interpreter would produce
  false negatives (reports missing when the venv has it) or false
  positives (reports present when the venv doesn't) depending on what
  happens to be on the host's system `python3`.
- **Actually spawn `agent-server` and see if it fails on startup** —
  rejected. This is what already happens today during a real Run, and is
  exactly the cryptic-failure experience this preflight exists to avoid;
  it would also leave a subprocess and port bound for a config check
  that's supposed to be a fast, side-effect-free read.
- **Fail `config check`'s exit code (and add to the missing-fields list)
  when a dependency is absent** — rejected for now. That list and exit
  code are specifically about `.syntropy.yml` fields an agent should ask
  a human about (ADR-0083); an environment/PATH problem is a different
  kind of gap with a different remediation (run an install command, not
  ask the user a question). Keeping them separate keeps both signals
  unambiguous.

## Consequences

- `config check` now shells out to a Python interpreter (when the
  openhands runner is registered and its binary resolves) purely to test
  two imports. This is a real subprocess spawn on every `config check`
  invocation with OpenHands enabled, but it's a `python -c "import x"`,
  not a real Agent Server boot, so the cost is negligible.
- `MissingDependencies`' PATH/exec calls are injected via unexported
  `Runner.lookPath`/`Runner.checkPythonImport` fields (nil in production,
  overridden in tests) — the same pattern `Runner.HTTPClient` already
  uses for the Agent Server HTTP calls — so its logic is unit-testable
  without a real `tmux`/`agent-server`/venv on the test host.
- If a future OpenHands release changes its packaging (e.g. drops the
  `uv tool install --with` pattern for a bundled wheel that vendors
  `libtmux`/`openhands-tools`), this preflight's package list — and
  possibly the shebang-interpreter-resolution approach itself — will need
  revisiting; it's coupled to the exact install method README documents
  today.
