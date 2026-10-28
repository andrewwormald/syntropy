# Agent guide

Instructions for AI agents working in this repository.

## What this repo is

An agent skill: `skills/syntropy/` (`SKILL.md` + `scripts/` + `tests/`), an installer (`install.sh`), and a decisions log (`decisions/`). There is no Go code. The old Go daemon is in git history at tag `daemon-final`.

Read first: [README.md](README.md), then [ADR-0117](decisions/0117-syntropy-as-a-skill-with-bundled-scripts.md).

## Working rules

### Decisions get ADRs

A decision gets an ADR under `decisions/` if it would surprise a reader, locks out alternatives, or would be re-litigated. Use the template in [`decisions/README.md`](decisions/README.md). Number it as the next unused 4-digit integer. Update the index. If it supersedes an ADR, mark the old one `Superseded by ADR-NNNN`.

ADRs 0001–0116 describe the daemon. They stay as history. Do not rewrite them.

### Code rules

- Scripts are bash with `set -euo pipefail`. Match the style in `scripts/lib.sh`.
- Run before committing:
  - `bash skills/syntropy/tests/run-tests.sh`
  - `shellcheck -S warning -e SC2034,SC2164 skills/syntropy/scripts/*.sh install.sh`
- Add a test in `tests/run-tests.sh` for every new behavior. Use the fake `glab` / `curl` stubs. No test may touch the network.
- Keep `SKILL.md` short. The agent reads it on every load. Put code in scripts, not in `SKILL.md`.
- Do not add dependencies beyond `git`, `jq`, `curl`, `tar`, `shasum`, and `gh`/`glab`.
- Single-line comments unless the *why* needs more.
- No Makefile. Put commands in this file.

### Do not do these without explicit human approval

- Force-push to `main`.
- Make a script write or delete outside `~/.syntropy` and the run's worktree.
- Let a script run text from an MR comment.
- Modify `LICENSE`.

### Commit hygiene

- One logical change per commit.
- The subject says intent (`feat(adopt): refuse fork MRs`), not just the diff.
- Co-author trailer when an AI agent contributed.
- Do not name real people in ADRs, docs, or commits. Use roles.

## Releasing

Push a tag `vX.Y.Z`. `.github/workflows/release.yml` tests, packages, and publishes the skill archive with its checksum. The in-skill update check reads the latest release, so a tag is a release to every user. Get approval before tagging.

## Live testing

See `skills/syntropy/TESTING.md`. Use a private scratch repo. Never a real or work repo.
