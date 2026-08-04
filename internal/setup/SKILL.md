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

## Presentation mechanism

Whenever this skill needs to show something to the user — a spec for
review, or an optional supporting visual — use the best rendering
capability your environment actually has, and never silently fall back to
a one-line summary:

- If you have an artifact/canvas/preview capability (e.g. Claude Code's
  Artifact tool), use it to render a clean, standalone document — not a
  wall of raw markdown dumped into the conversation.
- If your environment has no such capability, the fallback is to print
  the full content directly in the conversation — the complete thing, not
  a summary of it.

Two places in this flow rely on this principle:

- **Step 3** (spec review) uses it to render the spec itself before
  triggering a run — this is required, not optional, every time a new
  spec is written.
- **Step 1** (spec-tool routing) uses it for an optional nice-to-have: when
  no spec tool is configured, you may additionally offer a lightweight
  architecture visual (e.g. a simple component/flow diagram) alongside the
  plain markdown spec. This only applies when no spec tool is set — if a
  repo has a spec tool configured, that tool's own flow already owns
  presenting and reviewing the spec, so don't duplicate it here.

## Basic flow

1. **Run `syntropy config check --repo <base_repo>` before writing
   anything — a pure, cheap, no-tokens code check, not something to
   reason about yourself.** `syntropy setup` writes `.syntropy.yml` to
   whatever directory it's run *from* (`$(pwd)`, not the spec's
   `base_repo`) — it's easy for it to have never actually landed in the
   repo a spec targets, and there's no other warning if it hasn't. Don't
   open or parse `.syntropy.yml` yourself; that's exactly the token spend
   this command exists to avoid. Just run it and act on its exit code:
   - **Exit 0 ("OK: ... has every recognised field configured")** — move
     straight to writing the spec, no need to mention any of this to the
     user.
   - **Exit 1 ("MISSING: ...")** — it lists exactly which field(s) need
     asking about (new syntropy versions may add more over time; the
     command always reflects the current list, so don't hardcode field
     names yourself). *Only now* is a conversational turn worth spending:
     ask the user about each listed field. Say so plainly (e.g. "This
     repo's `.syntropy.yml` is missing a title convention — I need to ask
     before I can start; this is a one-off, later runs won't need this").
     Don't invent an answer yourself — check the repo's own CI/Danger/lint
     config for an enforced format first and offer that as a starting
     point, but confirm with the user before writing anything.
     - If they give you a real answer:
       `syntropy setup --title-convention "<their answer>"`, run from
       inside `<base_repo>` (not wherever this session's cwd happens to
       be).
     - If they explicitly decline to set one:
       `syntropy setup --title-convention blank` — write the literal
       sentinel, don't just skip writing anything, so `config check`
       correctly reports it as already-decided next time instead of
       flagging it as missing again.
     - Either way, continue straight on to writing the spec and
       triggering the run in the same reply — the user shouldn't have to
       ask twice.

   The point of running the check first is to spend a conversational
   turn (and its tokens) only on fields that are *actually* missing —
   never re-ask about something `config check` already reports as
   configured, and never read the YAML yourself to double-check its
   answer, and never do the work of writing a spec only to discover
   afterward that you needed to pause for a missing field.

   The same command's output also has a `Spec tool: ...` line —
   independent of the exit code, printed either way — telling you where
   to route spec creation/viewing for this repo: a named tool (e.g.
   `Spec tool: spec-kit (repo override)` or `(global default)`) means use
   that tool's own flow instead of the plain markdown-file approach in
   step 2 below; `Spec tool: (none set — syntropy's own default spec
   flow)` means proceed as described here, and you may optionally offer
   the lightweight architecture-visual nice-to-have described in
   **Presentation mechanism** above. Read this line, don't ask the
   user which spec tool to use or read `.syntropy.yml`/
   `~/.syntropy/config.yaml` yourself to figure it out.

   Without this, MRs/PRs default to a generic `<goal>: <unit-id>` title —
   which can badly violate a repo's real conventions (e.g. an 80-char CI
   title-length check) and isn't obvious until CI already failed on it.
2. **Write a spec.** A markdown file with YAML frontmatter (`goal`,
   `provider`, `project`, `base_branch`, `base_repo`, `concurrency`,
   `draft_mrs`, `status`) plus a markdown body expanding the goal — see
   the README's Quick Start section in the syntropy repo for a full
   example, or ask the user for the details you need. Default to writing
   it to `~/syntropy-specs/<name>.spec.md` unless the user's told you
   otherwise — a plain sibling directory, deliberately outside
   `~/.syntropy/`. Syntropy itself never reads, writes, indexes, or
   cleans up anything in there; it only ever reads whatever single path
   you hand it via `--spec` at trigger time. Syntropy owns execution,
   not the spec-authoring process leading up to it — this is a
   suggested convention for where *you* keep specs, not something the
   daemon manages.
3. **Render the spec for review before triggering it — every time you
   write a new one.** A spec is a real decision (scope, constraints, what
   gets built) made on the user's behalf; it shouldn't sit invisibly on
   disk until it's already running. Once the spec file is written,
   present its actual content — goal, provider/project, full body — using
   the **Presentation mechanism** described above, before calling
   `syntropy start`. Keep the treatment plain and legible (this is a spec
   for review, not a pitch); a compact frontmatter summary plus the
   rendered body plus the exact `syntropy start` command it'll run is
   enough. Wait for the user's go-ahead before triggering unless they've
   already told you (this session or standing instructions) to proceed
   automatically once a spec's ready — don't ask again if they have.
4. **Start the daemon** (if one isn't already running):
   ```bash
   syntropy daemon --commit-author "Name" --commit-email "you@example.com" &
   ```
5. **Trigger the run:**
   ```bash
   syntropy start --spec path/to/your.spec.md
   ```
6. **Check progress** any time with:
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

## Reporting a bug in syntropy itself

File an issue only for a genuine defect in syntropy's own behavior — a
crash, a wrong state transition, a misclassified event, the daemon doing
something the docs/ADRs say it shouldn't. Not for problems with the user's
own repo, CI, or business logic; those aren't syntropy bugs.

```bash
gh issue create --repo andrewwormald/syntropy --title "..." --body "..."
```

`andrewwormald/syntropy` is a public repo. The user's own repo, project
structure, feature names, and spec content are never yours to share there —
strip them before filing, every time:

- **Include:** `syntropy version` output, OS/arch, provider *type* only
  (`gitlab` or `github`, never the project path), the Run's `status` (goal
  text generalized — "a bulk rename across ~12 units", not the literal
  spec), the exact error/log line with any org/repo/project/file names
  replaced by placeholders (`<org>/<repo>`, `<path>`), and steps to
  reproduce described in terms of syntropy's own behavior (which command,
  which state transition, what happened vs. what you expected).
- **Never include:** the real org/repo/project name or URL, the literal
  spec goal or body, real file paths or diffs from the swept repo, MR/PR
  comment thread content, usernames/emails, or anything that reveals what
  the user's codebase does or how it's structured. If a log line or error
  message contains any of this, redact it before pasting — don't just
  trim the obviously-identifying part and assume the rest is safe.

If you're not sure whether something is safe to include, leave it out and
describe the shape of the problem in the abstract instead.
