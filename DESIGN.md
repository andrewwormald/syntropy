> **Archived.** This document describes the Go daemon, removed in [ADR-0117](decisions/0117-syntropy-as-a-skill-with-bundled-scripts.md). The last daemon commit is tagged `daemon-final`. Kept for history.

# Syntropy — design

**Status**: Active design. The v1 baseline ships under `internal/` (refactor-sweep workflow, sqlite store, GitLab + GitHub providers, poller, webhook server) and ADRs 0031-0034 have landed on top of it. Every load-bearing decision in this doc links to an ADR under [`decisions/`](decisions/).

**Last updated**: 2026-06-29

## Mandate

Syntropy drives **bulk refactor sweeps over large codebases**. It opens MRs one (or N) at a time, watches them through review and CI, addresses reviewer feedback, ships the merge, and picks up the next unit — until the refactor is done or the user calls a halt.

The point isn't durability for its own sake. The point is **amortising LLM cost**: the workflow handles the repetitive cheap work (queueing, throttling, status reporting, classifying comments, retrying flakes) deterministically; the subagent fires only on the bits that need reasoning. See [ADR-0014](decisions/0014-refactor-sweep-mandate.md).

## Why a workflow library for this

Bulk refactors have an L1/L2/L3 shape that maps cleanly onto `luno/workflow`:

| Refactor concept | `luno/workflow` primitive | ADR |
|---|---|---|
| The refactor goal (one-time setup) | `Trigger(WithInitialValue(&AgentState{Goal, Units, Filter, Skill}))` | [0005](decisions/0005-context-in-workflow-run.md) |
| Throttled sequence (per-unit state machine) | Step graph with a `Working → Awaiting-merge → ...` cycle, semaphore in `AgentState` | [0015](decisions/0015-throttled-sequential-mr-flow.md) |
| Wait for MR events (no polling, no busy loop) | `AddCallback` fired by inbound webhooks | [0014](decisions/0014-refactor-sweep-mandate.md) |
| Resume after restart | `RecordStore`; `Run.Object` rehydrated identically | [0005](decisions/0005-context-in-workflow-run.md) |
| Author intervention ("I'm stuck") | `Pause()` + author posts `/syntropy resume` comment | [0017](decisions/0017-author-privilege-model.md) |
| Concurrency > 1 (v2) | Parent Run + child Runs per in-flight unit, queue + semaphore in parent | [0015](decisions/0015-throttled-sequential-mr-flow.md) |
| Audit trail | Event log + the MR thread itself | [0016](decisions/0016-mr-comments-only-channel.md) |

## Architecture

```
                ┌──────────────────────────────────────────┐
   Author ───►  │   syntropy CLI                           │
   (Claude or   │   start / status / phrases promote / ... │
    direct)     └──────────────────┬───────────────────────┘
                                   │ trigger / inspect
                                   ▼
                ┌──────────────────────────────────────────┐
                │   syntropy daemon (long-lived)           │
                │   ┌─────────────────────────────────┐    │
                │   │ luno/workflow runtime           │    │
                │   │  - RecordStore (sqlite)         │    │
                │   │  - TimeoutStore (sqlite)        │    │
                │   │  - EventStreamer (in-process)   │    │
                │   └────────────────┬────────────────┘    │
                │                    │                     │
                │   ┌────────────────┴────────────────┐    │
                │   │ Per-Run state machine           │    │
                │   │  - Starlark filter eval         │    │
                │   │  - Provider client (glab/gh)    │    │
                │   │  - Runner (claude -p, ...)      │    │
                │   │  - Worktree mgmt                │    │
                │   └─────────────────────────────────┘    │
                │                                          │
                │   ┌─────────────────────────────────┐    │
                │   │ HTTP server :8080               │    │
                │   │  POST /webhook/{provider}/{runID}│   │
                │   │  HMAC-verify → workflow.Callback│    │
                │   └────────────────▲────────────────┘    │
                └────────────────────┼─────────────────────┘
                                     │ webhook POST
                                     │ (via ngrok / tailscale funnel
                                     │  / public DNS on cloud)
                                     │
                ┌────────────────────┴─────────────────────┐
                │  GitLab / GitHub                         │
                │   - MR events: notes, pipelines,         │
                │     merge_request actions                │
                │   - Project-scoped webhook (one per      │
                │     project, dispatched to many Runs)    │
                └──────────────────────────────────────────┘
```

### Daemon model

- **Single long-lived process** (see [ADR-0003](decisions/0003-single-long-lived-daemon.md))
- Hosts the workflow runtime, sqlite store, embedded HTTP server, and per-Run state machines
- Runs under `launchd` / `systemd` / `tmux` — designed to live on a VPS or EC2 instance, not a laptop
- Single workflow definition (`refactor-sweep`); multiple Runs per workflow

### Event ingress: poll by default, webhook opt-in

Per [ADR-0031](decisions/0031-polling-as-primary.md), the default event source is **polling**: the daemon ticks every 30s and asks the provider what's new for each in-flight MR via `GetMRState` + `ListNotesSince`. Synthesised events flow through the same `workflow.Callback` path webhooks use. No public URL required; no webhook permissions needed; no tunnel; no `--public-base-url`. The "Never polls" brand promise was always about **LLM tokens** — polling a provider's REST API costs zero tokens.

Webhook mode remains supported for VPS / cloud deployments that have a stable URL and want sub-second latency. Set `event_source: webhook` in the spec and pass `--public-base-url https://...`. The user provides the tunnel via whatever fits their environment:

| Deployment | Tunnel |
|---|---|
| VPS / EC2 / DigitalOcean | Native public DNS; open the port in the security group |
| Laptop | Tailscale Funnel — free, static `*.ts.net` URL |
| Laptop (alternative) | Cloudflare Tunnel — free with own domain |
| Laptop (last resort) | ngrok paid for stable URL; ngrok free works with the caveat that URLs rotate |

Syntropy does not auto-spawn any tunnel.

## The state machine

Two concentric loops: a Run-level loop that handles discovery and queue management; a per-unit lifecycle that opens and ships one MR.

### Run-level loop (concurrency = 1, v1 baseline)

```
Initiated
   │ setup: capture author, build skill mirror, run discovery
   │        (poll mode: skip webhook step; webhook mode: register + store secret)
   ▼
Discovering ───────── discovery returned 0 ────► Completed (post final comment, exit Run)
   │
   │ found N units, queue them
   ▼
Working                                        ◄─────────────────────────┐
   │ pop next unit from queue                                              │
   │ check filter, run subagent, push branch, open MR                      │
   ▼                                                                      │
Awaiting-merge ── [see per-unit lifecycle below] ─►                        │
                                              merged ────► slot freed ────┤
                                              closed ────► slot freed,    │
                                                           blacklist ─────┘
```

### Per-unit lifecycle (inside Awaiting-merge)

```
Awaiting-merge (sitting indefinitely, zero compute cost)
   │
   ├── event: note_added ──► filter ──► SKIP            (no cost)
   │                                  ──► CONTROL_COMMAND (author only — execute verb)
   │                                  ──► INVOKE_SUBAGENT (subagent reads thread, pushes fix)
   │                                                       │
   │                                                       └─► back to Awaiting-merge
   │
   ├── event: pipeline_failed ──► classify (Starlark)
   │                                  ──► known flake ────► retry job, no subagent
   │                                  ──► novel failure ──► subagent diagnose + fix
   │                                                         │
   │                                                         └─► back to Awaiting-merge
   │                                  ──► subagent stuck (3 attempts, same failure)
   │                                                         │
   │                                                         └─► Paused, post pause comment
   │
   ├── event: merge_request action=merged ──► Done (unit), release slot, return to Discovering
   │
   └── event: merge_request action=closed ──► Failed (unit), release slot, blacklist
```

The `Awaiting-merge` state can hold for *hours or days* with no LLM cost — between events the workflow consumes only the cost of the 30s poll tick (a couple of cheap REST calls), or sub-second wakeups on a webhook delivery. This is what makes the math work for long-running refactors.

Per [ADR-0034](decisions/0034-comment-loop-and-paused-self-loop.md): when the runner addresses a `note_added` event and successfully pushes the change, the originating discussion thread is auto-resolved on the platform — the reviewer sees their comment marked closed without manual action. If the runner replied without producing a code change (e.g. answered a question verbally), the thread is also resolved and the Run stays `Awaiting-merge` (no fatal pause for non-code answers).

### Concurrency > 1

Out of scope for v1, but the design accommodates it. The single Run becomes a *parent* Run holding a queue + semaphore; each in-flight unit gets its own *child* Run with the per-unit lifecycle. The parent's only job is queue management; child Runs handle their own MR. See [ADR-0015](decisions/0015-throttled-sequential-mr-flow.md) for the rationale.

## Communication model

The MR thread is the only channel ([ADR-0016](decisions/0016-mr-comments-only-channel.md)). Syntropy speaks by posting comments; the human speaks by replying.

### Comment classification on inbound

Every `note_added` event runs through:

1. **Author + `/syntropy ...` prefix?** → control command, see [ADR-0017](decisions/0017-author-privilege-model.md)
2. **Bot?** → provider-specific deterministic handler (e.g. Danger title-check → auto-fix title via `glab mr update --title`)
3. **Otherwise** → Starlark filter, see [ADR-0018](decisions/0018-starlark-filter-and-phrase-learning.md). Cheap-skip path for emojis and known phrases; subagent invocation only on substantive content.

### Author privileges

Captured at Trigger via `glab api user` (or `gh api user`), stored on `AgentState.Author`. From there:

- Author's `/syntropy <verb>` comments → bypass the LLM, route to state transitions
- Reviewer comments → go through the filter (substantive ones may invoke the subagent to address them)
- Bot comments → per-source handling

Verbs: `pause`, `resume`, `skip`, `retry`, `prompt <text>`, `status`, `stop`. Full semantics in [ADR-0017](decisions/0017-author-privilege-model.md).

## Workflow inputs at Trigger time

The author hands syntropy five things at `syntropy start`:

| Input | What it is | Example |
|---|---|---|
| **Goal** | One-sentence human description | "Migrate all Go services from logrus to log/slog" |
| **Discovery rule** | How to find units. Either a `--units` static list, or a Starlark `discover()` function, or a shell command | `discover.star` walks `services/*/go.mod` for logrus imports |
| **Skill** | A Claude Code skill the per-unit subagent will run. Lives at `~/.syntropy/runs/<runID>/SKILL.md` (mirror-symlinked into the worktree's `.claude/skills/`) | A skill file with the refactor recipe |
| **Filter** | Starlark function. Runs on every event. Defaults to a sensible one if not specified | `note_added.star`, `pipeline_failed.star` |
| **Provider config** | Which platform, project ID, auth token | `--provider gitlab --project acme/example` |

Plus a few operational flags: `--concurrency 1`, `--public-base-url https://...`, `--max-tokens 1M`, `--max-units 50`.

## Per-Run filesystem layout

```
~/.syntropy/
├── runs/
│   └── <runID>/
│       ├── SKILL.md              # canonical skill, edited by subagent each iteration
│       ├── discover.star         # Starlark discovery rule (if supplied)
│       ├── note_added.star       # Starlark filter for review comments
│       ├── pipeline_failed.star  # Starlark filter for CI events
│       ├── phrases.yaml          # learned skip phrases (per-Run scope)
│       └── worktree/             # the git worktree, mirror-symlinks SKILL.md from above
├── phrases.global.yaml           # cross-Run defaults; human-curated only
├── store.db                      # sqlite RecordStore + TimeoutStore
└── daemon.pid
```

## The Runner interface (unchanged from prior design)

```go
type Runner interface {
    Name() string
    Run(ctx context.Context, req RunRequest) (RunResponse, error)
}

type RunRequest struct {
    Worktree     string
    SkillCommand string         // "/refactor-logrus-to-slog services/payments"
    Goal         string
    UnitContext  string         // bounded — only this unit's scope
    Budget       Budget
}

type RunResponse struct {
    Decision   Decision         // continue | ask | done | fail
    Summary    string
    Question   string           // populated when Decision == ask
    Learnings  Learnings        // { add_phrases: [...], skill_updates: "..." }
    Tokens     int
}
```

Backwards-compatible with the v0 scheduled-skill code; the refactor flow just uses richer fields (Learnings, UnitContext).

## Provider abstraction

```go
type Provider interface {
    Name() string
    AuthenticatedUser(ctx context.Context) (User, error)
    RegisterWebhook(ctx context.Context, projectID string, url string, secret string) (webhookID string, err error)
    DeregisterWebhook(ctx context.Context, projectID string, webhookID string) error
    VerifySignature(headers http.Header, body []byte, secret string) bool
    NormaliseEvent(headers http.Header, body []byte) (Event, error)

    CreateMR(ctx context.Context, branch, title, description string) (MR, error)
    PostComment(ctx context.Context, mr MR, body string) error
    UpdateMRTitle(ctx context.Context, mr MR, title string) error
    RetryPipelineJob(ctx context.Context, jobID string) error
    CloseMR(ctx context.Context, mr MR) error
    IsBot(user User) bool
}
```

Both `gitlab.Provider` and `github.Provider` are shipped. Implementations are ~150 LOC each.

## What's not yet built

Since v1 shipped, ADRs 0031-0034 have landed: polling as the default event source ([ADR-0031](decisions/0031-polling-as-primary.md)), binary-blob filtering in worktree staging ([ADR-0032](decisions/0032-staging-filters-binary-blobs.md)), the `sync.Cond` EventStreamer ([ADR-0033](decisions/0033-replace-memstreamer.md)), and the comment-loop + auto-resolve + `Paused` self-loop ([ADR-0034](decisions/0034-comment-loop-and-paused-self-loop.md)). The provider abstraction, daemon HTTP server, sqlite store, refactor-sweep state machine, Starlark filter integration, per-Run filesystem layout, control-command handler, and GitHub provider adapter are all in. Still open:

- **`syntropy start` CLI (partial)** — flag parsing, validation, trigger, and `syntropy status` are in; `syntropy phrases promote` is still TODO.

## Open questions

These don't block the architecture, but they shape implementation:

1. **Signal that learning is working** — the filter should call the LLM less often over time. What's the measurable signal? A counter of `subagent_invocations / total_events` per Run? Plotted where? Worth thinking through before we ship "the system gets smarter."

2. **Subagent invocation atomicity** — when processing a unit, does one `claude -p` invocation (a) read the skill, (b) make the change, (c) open the MR, (d) update the skill with learnings, in one call? Or are those separate calls? Per-unit-one-call is cheaper; multi-call is more verifiable. Default to one call for v1; reconsider if quality is poor.

3. **What about questions in reviewer comments?** A reviewer asking "why this approach?" isn't a blocking change request. Does the subagent answer it (post a reply comment) or wait for the human? Leaning *answer* — but this lets the subagent author content, not just code. Worth a small UX experiment. *Partially answered by [ADR-0034](decisions/0034-comment-loop-and-paused-self-loop.md): a verbal reply with no code change is now supported — the runner posts an info comment and auto-resolves the thread instead of pausing.*

4. **Dependencies between units** — some refactors have ordering constraints (unit B depends on unit A being merged first). Do we model this? v1 says no — the discovery rule is responsible for enumerating units that *can* run in any order. If ordering matters, the author writes a discovery rule that returns the next unit when the prior is shipped, not all units up front.

5. **Re-discovery cadence and cost** — does discovery re-run after every merge, or on a timer, or both? Cheap for grep-the-filesystem rules; expensive for "call an API to enumerate." Probably configurable.

6. **Skill mutation safety** — the subagent edits `SKILL.md` after each unit. What if it edits it badly and the next unit's subagent now does worse work? Version it (every change is a git commit on the mirror), and add `syntropy skill-history <runID>` so the author can roll back.
