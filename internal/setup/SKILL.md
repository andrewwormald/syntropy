---
name: syntropy
description: Turn a large refactor or sweeping change into a chain of small, reviewable MRs/PRs, shepherded to merge one at a time by the syntropy daemon. Use when the user asks to sweep a change across many files or services, wants a long-running background loop that babysits a review/CI cycle, or mentions "syntropy" by name.
---

# syntropy

`syntropy` is a Go CLI + daemon that decomposes a large change into small
units, opens one draft MR/PR at a time, and only opens the next once the
current one merges. It runs independently of this session — once triggered,
it keeps working even after this conversation ends.

## When to use this skill

- The user wants a sweeping refactor applied across many files, packages, or
  services ("migrate every service off X", "rename Foo to Bar everywhere",
  "add a metric to every handler").
- The user wants something to "babysit" a PR/MR — keep it green overnight,
  respond to review comments, retry flaky CI — without you staying attached.
- The user explicitly mentions `syntropy`.

Don't use it for a single small, one-shot edit — just make the change
directly.

## Prerequisites

- `syntropy` binary on `$PATH` (confirm with `syntropy version`).
- `git` and `claude` on `$PATH`.
- A local clone of the target repo with an `origin` remote.
- Provider auth: `GITLAB_TOKEN` / `GITHUB_TOKEN` env var, or `glab auth
  login` / `gh auth login`.

## Basic flow

1. **Write a spec.** A markdown file with YAML frontmatter describing the
   goal, provider, project, and base repo. See `specs/README.md` in the
   syntropy repo for the anatomy, or ask the user for the details you need.
2. **Check the target repo's `.syntropy.yml` before triggering anything —
   first time in a repo means a short one-off setup conversation.**
   `syntropy setup` writes this file to whatever directory it's run *from*
   (`$(pwd)`, not the spec's `base_repo`) — it's easy for it to have never
   actually landed in the repo a spec targets, and there's no other
   warning if it hasn't. Look for `<base_repo>/.syntropy.yml`:
   - If present, read its `title_convention` and move straight to writing
     the spec — no need to mention any of this.
   - If absent, this is the first time syntropy is being used against this
     repo. Say so plainly (e.g. "First time running syntropy against this
     repo — I need to ask a couple of quick setup questions before I can
     start; this is a one-off, later runs won't need this"), then ask the
     user for this repo's PR/MR title convention. Don't invent one
     yourself — check the repo's own CI/Danger/lint config for an
     enforced title format first and offer that as a starting point, but
     confirm with the user before writing anything. Once confirmed, write
     it yourself: `syntropy setup --title-convention "<their answer>"`,
     run from inside `<base_repo>` (not from wherever this session's cwd
     happens to be) — then continue straight on to writing the spec and
     triggering the run in the same reply, since the user shouldn't have
     to ask twice.
   Without this, MRs/PRs default to a generic `<goal>: <unit-id>` title —
   which can badly violate a repo's real conventions (e.g. an 80-char CI
   title-length check) and isn't obvious until CI already failed on it.
3. **Start the daemon** (if one isn't already running):
   ```bash
   syntropy daemon --commit-author "Name" --commit-email "you@example.com" &
   ```
3. **Trigger the run:**
   ```bash
   syntropy start --spec path/to/your.spec.md
   ```
4. **Check progress** any time with:
   ```bash
   syntropy status <run-id>
   syntropy list
   ```

The daemon opens the first MR/PR within a minute or two. Reviewers interact
with it entirely through MR/PR comments (`/syntropy status`, `/syntropy
pause`, `/syntropy skip`, `/syntropy retry`, `/syntropy prompt …`,
`/syntropy stop`) — you don't need to keep polling on the user's behalf
unless asked.

## Other commands

- `syntropy abandon <run-id>` — request abandonment (two-tap confirmation).
- `syntropy resume <run-id>` — resume a paused run.
- `syntropy phrases` — manage skip-phrase files.

Run `syntropy <command> -h` for full flag reference before constructing a
command — flags evolve independently of this skill file.
