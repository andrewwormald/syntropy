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

## What it is

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

## Why it helps

The two existing options for sweeping refactors are bad:

**One giant PR.** 47 services in one branch, `+1,247 / -863`. The reviewer needs an afternoon. They don't have one. The PR sits, conflicts pile up, eventually someone rubber-stamps it to clear the queue. Quality collapses; the diff is too big to engage with.

**You crank one at a time.** Open MR, wait for review, merge, manually do the next, repeat 47 times. You're the bottleneck between every link. You can't go to a meeting; you can't go on holiday; your day is spent waiting and ticking. The discipline is real but the cost is your week.

Syntropy puts a daemon in the loop instead of you. The chain self-propels: you act only at the natural human checkpoint — *is this MR good?* — and the daemon handles everything else (opening, pushing, status comments, addressing review comments, retrying flaky CI, picking the next unit, opening the next MR).

**You get the benefits of disciplined small-MR practice without the discipline being your problem.** Each MR is small because syntropy split the work; each MR ships fast because reviewers can actually engage with it; the sweep completes because there's never a stalled mega-PR clogging the queue.

## Inside each MR

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

## How it works (briefly)

- **Durable state machine.** Built on [luno/workflow](https://github.com/luno/workflow); sqlite-backed RecordStore. Survives daemon restart, can sleep idle for days between events at zero LLM cost.
- **Event-driven.** Polls the provider every 30 seconds by default (zero token cost; ADR-0031). Webhook mode available for sub-second latency on hosts with a stable public URL.
- **Per-unit git worktree.** Each MR's runner works in `~/.syntropy/runs/<runID>/worktrees/<unitID>` — no contamination of your main checkout.
- **Auto-resolve on push.** When the runner addresses a reviewer comment and lands the fix, the discussion thread is marked resolved automatically on both GitLab and GitHub (ADR-0034). The reviewer sees their comment close itself.
- **Pluggable runner.** Claude is the only shipping adapter today; Qwen / OpenHands / a local script all fit the `runner.Runner` interface.

Full architecture: [`DESIGN.md`](DESIGN.md). Every meaningful design choice has an ADR in [`decisions/`](decisions/).

## Quick start

You need: Go 1.26+, `git` and `claude` on `$PATH`, a clone of the target repo with an `origin` remote, and provider auth — either an env var (`GITLAB_TOKEN` / `GITHUB_TOKEN`) or an interactive CLI login (`glab auth login` for GitLab, `gh auth login` for GitHub). If both are configured, the env var wins.

The first time you run any command, syntropy best-effort installs the Claude Code Skill bundle into `~/.claude` so Claude Code knows how to invoke it (ADR-0002). Run `./syntropy setup` explicitly to (re)install that bundle and to pick and persist a default runner/model and this repo's PR/MR title convention to `~/.syntropy/config.yaml` (ADR-0051); pass `--force` to overwrite an existing install.

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

# Build + start the daemon (poll mode; no public URL needed).
go build -o syntropy .
./syntropy daemon --commit-author "Your Name" --commit-email "you@example.com" &

# Trigger.
./syntropy start --spec ~/syntropy-specs/migrate.spec.md
```

The first MR appears on the target repo within a minute or two. Review it, merge it, and the next opens automatically.

## Repository layout

| Path | Purpose |
|---|---|
| [`README.md`](README.md) | This file |
| [`DESIGN.md`](DESIGN.md) | Full architecture and roadmap |
| [`AGENTS.md`](AGENTS.md) | Working rules for AI contributors |
| [`decisions/`](decisions/) | Architecture Decision Records — every meaningful choice |
| [`logo/`](logo/) | Brand assets |
| `main.go` | CLI entrypoint: `daemon`, `start`, `status`, `list`, `abandon`, `resume`, `phrases`, `setup`, `version` |
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
