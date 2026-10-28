<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="logo/wordmark-dark.svg">
    <img src="logo/wordmark.svg" alt="syntropy" width="360" />
  </picture>

  <p><strong>An agent skill that helps with merge requests. Break a large change into many small MRs. Or take over an MR that exists and keep it moving.</strong></p>
</div>

---

## What it does

Syntropy is a skill for Claude Code and other coding agents. It is a `SKILL.md` plus shell scripts. There is no binary and no daemon.

Two modes:

- **Sweep** — split a big change into small MRs/PRs. The agent opens one, waits for review and merge, then opens the next. MRs open ready for review. Drafts are opt-in (`draft_mrs: true`).
- **Adopt** — take over an existing MR/PR. The agent keeps CI green, answers comments and reviews, and resolves conflicts. It never closes your MR and never force-pushes.

## Why scripts

Waiting for review costs tokens if the agent polls. The scripts poll instead. The agent runs `syn-wait.sh` in the background and wakes only when something needs it: a merge, a failed CI job, a review comment. The agent runs the scripts and does not read them, so the code costs no tokens.

Scripts also filter comments before the agent sees them: thanks/lgtm/emoji, bots, its own comments, known CI flakes. Comments from people other than you go through a risk screen first. A flagged comment never reaches the agent.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/andrewwormald/syntropy/main/install.sh | bash
```

This installs the latest release to `~/.claude/skills/syntropy`. Options, after `bash -s --`:

- `--dir <path>` — another agent's skills folder
- `--ref <tag|branch>` — a specific version instead of the latest release

Prefer to read it first? Download `install.sh`, read it, then run it. Or copy `skills/syntropy/` into your agent's skills folder yourself.

Needs `git`, `jq`, `curl`, and `gh` or `glab` (logged in).

## Use

Tell your agent what you want:

- "Use syntropy to migrate every service from logrus to slog, one MR per service."
- "Use syntropy to take over !1234 and fix CI and the review comments."

## Update

The skill checks GitHub for a newer release at most once every 24 hours, when you start a sweep or adopt an MR. It prints one line if a newer release exists. It never installs anything by itself.

To update: run `syn-update.sh --apply`, or run `install.sh` again. Both check the release checksum first. If the skill folder is a symlink to a checkout, use `git pull`.

Turn the check off with `SYN_NO_UPDATE_CHECK=1`.

## Limits

- The loop lives in the agent session. If the session ends, start the wait again. Run state is on disk in `~/.syntropy`.
- No webhooks and no token budget. A sweep stops at `SYN_MAX_UNITS` (default 50).
- GitHub sweep tested live. GitLab and adopt mode tested against fakes only.

## Develop

```bash
bash skills/syntropy/tests/run-tests.sh   # offline, ~25s, needs git and jq
shellcheck -S warning -e SC2034,SC2164 skills/syntropy/scripts/*.sh install.sh
```

Release: push a tag `vX.Y.Z`. The workflow tests, packages `skills/syntropy`, and attaches `syntropy-skill.tar.gz` and its checksum. The update check reads that release.

See [`decisions/`](decisions/) for why. [ADR-0117](decisions/0117-syntropy-as-a-skill-with-bundled-scripts.md) records the move from a Go daemon to this skill. The last daemon commit is tagged `daemon-final`.

## License

See [LICENSE](LICENSE).
