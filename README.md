<div align="center">
  <h1>In Active Development</h1>
</div>
<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="logo/wordmark-dark.svg">
    <img src="logo/wordmark.svg" alt="syntropy" width="360" />
  </picture>

  <p><strong>Turn one large change into a chain of small, individually-reviewable MRs — and shepherd each one to merge before opening the next.</strong></p>
  <p><em>syntropy</em> · /ˈsɪn.trə.pi/ (SIN-truh-pee) — the tendency of a system to organize, grow in complexity, and move toward order; the counterpart to entropy, the Second Law of Thermodynamics' pull toward disorder. The name is the mission: agents can generate change faster than any human review process can absorb. Syntropy imposes order on that volume — pacing agent-authored MRs to the rate a human can actually comprehend and gate, not the rate agents can produce them.</p>
</div>

---

## The problem

Sweeping changes — migrate every service off a deprecated package, rename a type everywhere, add a metric to every handler — break down into the same two bad options:

**One giant PR.** 47 services in one branch, `+1,247 / -863`. The reviewer needs an afternoon. They don't have one. The PR sits, conflicts pile up, eventually someone rubber-stamps it to clear the queue. Quality collapses; the diff is too big to engage with.

**You crank one at a time.** Open MR, wait for review, merge, manually do the next, repeat 47 times. You're the bottleneck between every link. You can't go to a meeting; you can't go on holiday; your day is spent waiting and ticking. The discipline is real but the cost is your week.

There's a second cost underneath both: an LLM babysitting a review/CI cycle burns tokens the moment it's *watching*, not just when it's *working*. A subagent that polls a PR thread every few seconds, or re-reads the whole diff to check "did CI pass yet," spends real money doing nothing. A single giant PR is even worse on this axis — one subagent holding the entire 47-service context in its head for the whole review cycle, re-paying that context cost on every CI retry or review round.

Syntropy solves both at once: small MRs a human can actually review in 30-60 seconds, and a daemon that costs zero tokens while a MR sits waiting for review or CI — the LLM only fires when there's something to actually reason about.

## What syntropy does

You hand syntropy a spec — "migrate `internal/legacy` to `internal/v2` across services", "rename `Foo` to `Bar` everywhere", "add a metric to every HTTP handler". A small Go daemon decomposes it into one increment at a time, opens a single Draft MR, waits for a human to actually merge it, and only then opens the next. Concurrency is configurable; default is one MR in flight.

```
     spec.md
        │
        ▼
     ┌─────┐   ┌─────┐   ┌─────┐   ┌─────┐
     │ MR1 │──►│ MR2 │──►│ MR3 │──►│ MR4 │──►  ... until done
     └──┬──┘   └──┬──┘   └──┬──┘   └──┬──┘
        │        │         │         │
        ▼        ▼         ▼         ▼
     review+  review+   review+   review+
     merge    merge     merge     merge
       (one at a time — gated on real review bandwidth)
```

Each MR is small — typically tens of lines, one logical change, scoped to a single unit. The next link in the chain doesn't exist yet when you're reviewing the current one.

The chain self-propels: you act only at the natural human checkpoint — *is this MR good?* — and the daemon handles everything else (opening, pushing, status comments, addressing review comments, retrying flaky CI, picking the next unit, opening the next MR).

```
   ┌──────────────────────────────────────────────────┐
   │  MR opens (Draft, small scope, one unit)         │
   │                                                  │
   │  Human reviews — 30-60 seconds                   │
   │     │                                            │
   │     ├── approve + merge ─────► next MR opens     │
   │     │                                            │
   │     ├── "/syntropy skip" ────► unit blacklisted, │
   │     │                          next MR opens     │
   │     │                                            │
   │     └── request change ──────► runner pushes     │
   │                                a fix,            │
   │                                resolves the      │
   │                                thread,           │
   │                                back to review    │
   └──────────────────────────────────────────────────┘
```

Comments are syntropy's only communication channel. Reply with `/syntropy pause`, `/syntropy resume`, `/syntropy skip [reason]`, `/syntropy retry`, `/syntropy prompt <text>`, `/syntropy status`, `/syntropy stop`, or `/syntropy abandon` (two-tap, 12h confirmation window). A bare `/syntropy` posts the verb list; anything else after `/syntropy` is treated as a freeform instruction and injected straight into the next subagent call, same as `/syntropy prompt`. Bot noise (CI status, formatter comments) is skipped deterministically by a Starlark filter, so the LLM only fires when a comment or a CI failure actually needs reasoning.

### How it does it

- **Durable state machine.** Built on [luno/workflow](https://github.com/luno/workflow); sqlite-backed RecordStore. Survives daemon restart, can sleep idle for days between events at zero LLM cost.
- **Event-driven.** Polls the provider every 30 seconds by default (zero token cost; ADR-0031). Webhook mode available for sub-second latency on hosts with a stable public URL.
- **Per-unit git worktree.** Each MR's runner works in `~/.syntropy/runs/<runID>/worktrees/<unitID>` — no contamination of your main checkout.
- **Auto-resolve on push.** When the runner addresses a reviewer comment and lands the fix, the discussion thread is marked resolved automatically on both GitLab and GitHub (ADR-0034). The reviewer sees their comment close itself.
- **Pluggable runner.** Claude is the only shipping adapter today; Qwen / OpenHands / a local script all fit the `runner.Runner` interface.

Full architecture: [`DESIGN.md`](DESIGN.md). Every meaningful design choice has an ADR in [`decisions/`](decisions/).

## When to use it

Reach for syntropy when:

- **The change spans many files, packages, or services** — "migrate every service off X," "rename Foo to Bar everywhere," "add a metric to every handler." If it decomposes into repeatable, independent units, syntropy is built for it.
- **You want something to babysit a PR/MR** — keep it green overnight, respond to review comments, retry flaky CI — without staying attached to the conversation yourself.
- **The bottleneck is human review bandwidth, not the change itself.** Syntropy doesn't make reviewers faster; it makes sure they're never handed more than they can actually absorb at once.

**What it replaces:** the giant-PR-that-nobody-reviews-properly, and the manual crank-one-at-a-time grind described in [The problem](#the-problem) above.

**What it extends, not replaces:** your existing review process. Syntropy doesn't remove the human checkpoint — every MR still needs an actual approve-and-merge. It just makes sure that checkpoint is the *only* thing gating progress, instead of also gating on someone remembering to kick off the next unit.

**What it's not for:** a single small, one-shot edit. If the change doesn't decompose into multiple independently-mergeable units, just make it directly — spinning up a daemon and a spec for one file is pure overhead.

## Install

You need: Go 1.26+, `git` and `claude` on `$PATH`, a clone of the target repo with an `origin` remote, and provider auth — either an env var (`GITLAB_TOKEN` / `GITHUB_TOKEN`) or an interactive CLI login (`glab auth login` for GitLab, `gh auth login` for GitHub). If both are configured, the env var wins.

The first time you run any command, syntropy best-effort installs the Claude Code Skill bundle into `~/.claude` so Claude Code knows how to invoke it (ADR-0002). Run `./syntropy setup` explicitly to (re)install that bundle and to pick and persist a default runner/model and this repo's PR/MR title convention to `~/.syntropy/config.yaml` (ADR-0051); pass `--force` to overwrite an existing install.

### Installing a prebuilt binary

**Always install from the prebuilt release — do not `git clone` and `go build`.** Building from source skips goreleaser's version/commit stamping, is slower, and pulls in a full Go toolchain for no benefit; every tagged release already ships a binary for your platform. If you're an AI agent asked to "install syntropy," this is the one and only path — go straight to [GitHub Releases](https://github.com/andrewwormald/syntropy/releases), don't reach for `go install` or a source build.

Download the archive matching your OS/arch (`syntropy_<version>_<os>_<arch>.tar.gz`, e.g. `darwin_arm64` or `linux_amd64`), extract it, and put the `syntropy` binary on your `$PATH`:

```bash
tar -xzf syntropy_*_darwin_arm64.tar.gz
chmod +x syntropy
mv syntropy /usr/local/bin/
```

```bash
# Write a spec.
cat > ~/syntropy-specs/migrate.spec.md <<'YAML'
---
goal: Replace internal/legacy with internal/v2 across services
provider: gitlab
project: acme/example
runner: claude
base_branch: main
base_repo: /home/you/dev/your-repo
concurrency: 1
draft_mrs: true
status: ready
---
# Migration plan

For each service still importing `internal/legacy`, switch to
`internal/v2`. Preserve public function signatures.
YAML

# Start the daemon (poll mode; no public URL needed) — using the release binary installed above.
syntropy daemon --commit-author "Your Name" --commit-email "you@example.com" &

# Trigger.
syntropy start --spec ~/syntropy-specs/migrate.spec.md
```

The first MR appears on the target repo within a minute or two. Review it, merge it, and the next opens automatically.

## Features

- **One MR/PR at a time.** Small, reviewable, Draft by default; concurrency is configurable but defaults to one in flight.
- **Zero-token idle.** A MR waiting on review or CI costs nothing — the daemon polls provider state for free and only invokes the LLM when there's a real comment or failure to reason about.
- **Poll or webhook event ingress.** Polling (default) needs no public URL; webhook mode is opt-in for sub-second latency on hosts with a stable address.
- **Author-vs-reviewer privilege model.** `/syntropy <verb>` control commands from the author bypass the LLM entirely and route straight to a state transition; everyone else's comments go through the Starlark filter.
- **Deterministic comment filtering.** Bot noise (CI status pings, formatter comments) is skipped without spending a single token, via a Starlark filter with per-Run override and learned skip-phrases.
- **Auto-resolve on push.** When the runner addresses a reviewer comment and lands the fix, the discussion thread is marked resolved automatically on both GitLab and GitHub.
- **CI-failure triage.** Classifies a pipeline failure as a known flake (retry, no LLM) or a novel failure (subagent diagnose + fix), bounded by a retry cap before pausing for a human.
- **Pausable and resumable, not just pass/fail.** Transient runner or git errors during a unit's initial turn pause the Run for `/syntropy resume` instead of killing it outright — `Failed` is reserved for genuinely unrecoverable configuration problems.
- **Self-healing via reconciliation.** A periodic sweep detects Runs stuck on a lost event and wakes them back up; a merge/close mis-detected mid-propagation gets re-verified before a unit is wrongly blacklisted.
- **Per-unit git worktree.** Each MR's runner works in its own isolated worktree — no contamination of your main checkout, no cross-unit interference.
- **Durable state machine.** Sqlite-backed; survives a daemon restart mid-Run and can sleep idle for days between events.
- **Two-tap abandonment.** `/syntropy abandon` requires a confirming second tap within a 12h window before a Run is actually killed — no accidental stops from a stray comment.
- **Pluggable runner and provider.** Claude ships today; the `Runner` interface (Qwen, OpenHands, a local script) and the `Provider` interface (GitLab, GitHub) are both designed to take more adapters without touching the core state machine.
- **Repo-aware MR conventions.** Reads a per-repo title convention from `.syntropy.yml` and threads it into every MR title the runner generates, instead of a generic `<goal>: <unit-id>` default.
- **Retention cleanup.** Terminal Runs' worktrees, on-disk artifacts, and durable-store rows are automatically cleaned up after a configurable retention window (31 days by default).

## Repository layout

| Path | Purpose |
|---|---|
| [`README.md`](README.md) | This file |
| [`DESIGN.md`](DESIGN.md) | Full architecture and roadmap |
| [`AGENTS.md`](AGENTS.md) | Working rules for AI contributors |
| [`decisions/`](decisions/) | Architecture Decision Records — every meaningful choice |
| [`logo/`](logo/) | Brand assets |
| `main.go` | CLI entrypoint: `daemon`, `start`, `status`, `list`, `abandon`, `resume`, `phrases`, `setup`, `config`, `version` |
| [`internal/refactorsweep/`](internal/refactorsweep/) | State machine + step bodies + control verbs |
| [`internal/provider/{gitlab,github}/`](internal/provider/) | Platform adapters |
| [`internal/runner/claude/`](internal/runner/claude/) | Claude shell-out runner with decision-marker parsing |
| [`internal/git/`](internal/git/) | `git` CLI wrapper with binary-blob filter |
| [`internal/store/`](internal/store/) | Sqlite-backed `workflow.RecordStore` + `TimeoutStore` |
| [`internal/spec/`](internal/spec/) | Spec markdown parser (frontmatter + body) |
| [`internal/filter/`](internal/filter/) | Starlark event filter with per-Run override + phrase learning |
| [`internal/eventstream/`](internal/eventstream/) | In-process `workflow.EventStreamer`, cond.Wait signalling over a sqlite-backed durable log |
| [`internal/poller/`](internal/poller/) | Poll-mode event ingress |
| [`internal/webhook/`](internal/webhook/) | HTTP webhook ingress (opt-in) |
| [`internal/reconciler/`](internal/reconciler/) | Detects Runs stuck on a lost in-memory event and wakes them back up |
| [`internal/config/`](internal/config/) | Reads/writes `~/.syntropy/config.yaml`, the persisted default runner/model from `syntropy setup` |
| [`internal/setup/`](internal/setup/) | Installs the Claude Code Skill bundle and drives the `syntropy setup` interactive flow |
| [`_v0/`](_v0/) | Archived scheduled-skill PoC, separate module |

## Known limitations and troubleshooting

- **Concurrency = 1.** Parallel MRs (concurrency > 1) are on the roadmap but not yet shipped; each Run opens one MR at a time.
- **Provider auth expiry.** If the OAuth token or PAT expires mid-Run, the daemon pauses the Run with a `provider-auth:` reason and backs off polling. Refresh credentials and restart the daemon to resume automatically.
- **`claude` must be on `$PATH`.** There is no fallback runner; if `claude` exits non-zero, the Run parks at Paused so you can retry.
- **Webhook mode requires a stable public URL.** Poll mode (the default) works anywhere; webhook mode needs `--public-base-url`.

See [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md) for diagnosis steps and recovery procedures for every known failure mode.

## License

MIT
