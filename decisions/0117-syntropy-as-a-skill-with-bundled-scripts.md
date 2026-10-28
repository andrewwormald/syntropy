# ADR-0117: syntropy is a skill with bundled scripts; the Go daemon is removed

**Status**: Accepted
**Date**: 2026-10-27

## Context

[ADR-0002](0002-distribute-as-claude-skill.md) shipped syntropy as a Go
binary plus a skill that holds "no executable logic".
[ADR-0003](0003-single-long-lived-daemon.md) made the daemon the only way to
run a Run. [ADR-0014](0014-refactor-sweep-mandate.md) framed the product as a
chain of MRs for bulk refactors.

Three things changed:

- Agent skills are now the normal way to share a tool. A reader scans a skill
  faster than they install and learn a binary. Other teams and other agents
  adopt a skill with a copy.
- The token saving the daemon gave is "do not poll with the LLM". A script
  that the agent runs in the background gives the same saving. The agent wakes
  only when the script exits. ([ADR-0107](0107-wait-command-token-free-poll-for-first-checkpoint.md)
  already used this for `syntropy wait`.)
- The product goal changed. A chain of MRs is one use. A second use is to
  take over an MR that already exists and keep CI green, answer comments, and
  handle reviews.

A prototype was built and tested: offline tests against a fake `glab`, a
fake `curl`, and a local git remote, plus a live GitHub run of the sweep path.

## Decision

syntropy is a skill. The repo ships `skills/syntropy/` (`SKILL.md`, `scripts/`,
`tests/`) and the Go daemon is removed. The last daemon commit is tagged
`daemon-final`.

The agent runs the scripts and does not read them, so code costs no tokens.

**Two modes on one engine:**

- **Sweep** — split a large change into many small MRs, one at a time. MRs
  open ready for review. Drafts are opt-in with `draft_mrs: true`.
- **Adopt** — take over an existing MR/PR. Same wait loop, comment triage, and
  CI handling. `syn-adopt.sh` never closes the MR, never force-pushes, and
  refuses fork MRs.

**Ported from the daemon:** token-free wait, comment triage (noise, bots, echo
suppression, author control verbs), non-author risk screen before the body
reaches the session ([ADR-0106](0106-risk-screen-gates-non-author-comments.md)),
flake retry, conflict sync, binary filter, thread resolve
([ADR-0115](0115-resolve-thread-on-continue-and-surface-resolve-failures.md)),
closed re-verify ([ADR-0096](0096-reverify-mr-closed-before-blacklisting.md)),
API backoff and pause.

**Distribution and update — same process as the binary had:**

- `install.sh` pulls the latest GitHub release archive (or `--ref`), verifies
  its SHA-256, and stores the skill in the agent's skills folder
  (`~/.claude/skills/syntropy` by default, `--dir` for other agents). It writes
  a `VERSION` file.
- `syn-update.sh` checks the latest release at most once per 24 hours, caches
  the result, and prints one best-effort line when a newer release exists
  ([ADR-0102](0102-start-prints-update-notice-best-effort.md)). `syn-start.sh`
  and `syn-adopt.sh` call it. It never installs by itself.
- `syn-update.sh --apply` downloads, verifies, and swaps the skill folder. It
  refuses when the folder is a symlink to a checkout.
- A pushed `vX.Y.Z` tag runs `.github/workflows/release.yml`: tests, package,
  publish `syntropy-skill.tar.gz` and its checksum.

## Alternatives considered

- **Keep the binary and the skill.** Keeps durability, but the install cost
  blocks use by other agents and teams, and two codebases drift.
- **Rewrite as an MCP server.** Needs a process per session and a handshake.
  Scripts need only `git`, `jq`, `curl`, and `glab` or `gh`.
- **Inline the code in SKILL.md.** The agent would read it on each load. Files
  that the agent only runs cost nothing to carry.
- **Auto-update.** Rejected. A skill runs with the user's credentials. A silent
  update is a silent change to what runs. The user applies updates.

## Consequences

- **Lost:** survival after the session ends, webhooks, token and runtime
  budgets, the background planner (`discoverSpec`), Starlark filters, the
  reconciler, retention, the OpenHands runner. State survives on disk, but a
  session must restart the wait.
- **Gained:** no install step beyond a copy, one scannable file, a second use
  case (adopt), one codebase.
- **Supersedes** ADR-0002 and ADR-0003. ADRs 0004–0116 describe the daemon.
  They stay as history. Those that only apply to the daemon (for example the
  shared container sandbox proposal) are moot.
- **Open:** GitLab and adopt mode are tested against fakes only. The risk
  screen needs a live run with a real `claude -p`. `install.sh` and
  `syn-update.sh` are untested against a real release until the first tag.
- `skills/syntropy/tests/run-tests.sh` is the contract. Run it before any change.
- A tag is a release to every user. Tagging needs human approval.
