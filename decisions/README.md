# Architecture Decision Records

A log of every meaningful decision made in this repo. The goal is
**agentifiability**: an AI agent should be able to read this directory and
have the same context a senior contributor would.

## Format

Each ADR is a numbered markdown file:

```
NNNN-kebab-case-title.md
```

Number is 4-digit zero-padded, assigned by the next unused integer. Titles
should be a short noun phrase ("Single long-lived daemon," not "We chose…").

### Template

```markdown
# ADR-NNNN: <title>

**Status**: Accepted | Proposed | Superseded by ADR-XXXX
**Date**: YYYY-MM-DD

## Context

What situation prompted this? What constraints were we operating under?
Reference prior ADRs by number and title.

## Decision

What we chose, stated as a single positive sentence first, then detail.

## Alternatives considered

Other options we looked at and why we did not pick them. Be specific —
"X is more complex" is weak; "X requires a long-lived subprocess we'd have
to manage across daemon restarts" is useful.

## Consequences

What this commits us to. What it rules out. What follow-up decisions it
forces or unlocks.
```

## Index

| #    | Title                                              | Status   | Date       |
|------|----------------------------------------------------|----------|------------|
| 0001 | Everflow is an orchestrator, not a Claude replacement | Accepted | 2026-06-15 |
| 0002 | Distribute as a Claude Code Skill + Go binary      | Accepted | 2026-06-15 |
| 0003 | Single long-lived daemon, not cron-driven ticks    | Accepted | 2026-06-15 |
| 0004 | Shell out to `claude -p`, not the Anthropic SDK    | Accepted | 2026-06-15 |
| 0005 | Context lives in the workflow Run, not in the runner | Accepted | 2026-06-15 |
| 0006 | One git worktree per Run                           | Accepted | 2026-06-15 |
| 0007 | Pluggable `Runner` interface, per-Run `--runner` flag | Accepted | 2026-06-15 |
| 0008 | Native structured output per runner                | Accepted | 2026-06-15 |
| 0009 | Collapse the step graph to `Initiated → Iterating → terminals` | Accepted | 2026-06-15 |
| 0010 | Build the scheduled-skill PoC before the interactive loop | Accepted | 2026-06-15 |
| 0011 | Persistent worktree, reset --hard to base each pass | Accepted | 2026-06-15 |
| 0012 | Track all decisions as ADRs                        | Accepted | 2026-06-15 |
| 0013 | Adopt the "Orbit" brand mark and warm-coral palette | Superseded by 0040 | 2026-06-15 |
| 0014 | Bulk-refactor sweeps are the primary use case      | Accepted | 2026-06-16 |
| 0015 | Throttled-sequential MR flow, configurable concurrency | Accepted | 2026-06-16 |
| 0016 | MR comments as everflow's only communication channel | Accepted | 2026-06-16 |
| 0017 | Author-vs-reviewer privilege model + control commands | Accepted | 2026-06-16 |
| 0018 | Starlark filter on every event, with bounded phrase learning | Accepted | 2026-06-16 |
| 0019 | Project layout: `main.go` at root, business logic under `internal/`, `_v0/` archived as own module | Accepted | 2026-06-17 |
| 0020 | GitLab provider — hand-rolled REST client, bare-token webhooks  | Accepted | 2026-06-18 |
| 0021 | GitHub provider — HMAC webhooks, three comment events collapse to one | Accepted | 2026-06-19 |
| 0022 | Sqlite-backed RecordStore + TimeoutStore (pure-Go driver)       | Accepted | 2026-06-19 |
| 0023 | Git operations — shell out to `git`, host-managed auth, local BaseRepo for v1 | Accepted | 2026-06-19 |
| 0024 | Spec as Run; sweep + spec modes coexist on one workflow definition | Accepted | 2026-06-22 |
| 0025 | Planner-driven `discover()` for spec mode                       | Accepted | 2026-06-22 |
| 0026 | Abandonment-with-confirmation (two-tap stop)                    | Accepted | 2026-06-22 |
| 0027 | Claude runner adapter — prompt-marker protocol for Decision     | Accepted | 2026-06-22 |
| 0028 | `everflow start` triggers via a localhost-only HTTP endpoint    | Accepted | 2026-06-22 |
| 0029 | Secret rehydration on daemon startup                            | Accepted | 2026-06-22 |
| 0030 | Starlark filter — embedded default, per-Run override, YAML phrases | Accepted | 2026-06-22 |
| 0031 | Polling as the primary event source; webhooks opt-in            | Accepted | 2026-06-23 |
| 0032 | Commit stages selectively; binary blobs are filtered out        | Accepted | 2026-06-24 |
| 0033 | In-process EventStreamer with cond.Wait, not luno/workflow's memstreamer | Accepted | 2026-06-24 |
| 0034 | Comment-driven loop is the primary interaction; Paused permits self-loop | Accepted | 2026-06-25 |
| 0035 | Self-comment echo suppression via SHA-256 FIFO on AgentState    | Accepted | 2026-07-01 |
| 0036 | Budget enforcement: MaxUnits, MaxTokens, MaxRuntime             | Accepted | 2026-07-02 |
| 0037 | Resume CLI writes directly to the RecordStore                   | Accepted | 2026-07-02 |
| 0038 | Poller auth-backoff via Paused-with-marker                      | Accepted | 2026-07-02 |
| 0039 | Thread planner's per-increment rationale into the runner's Goal | Accepted | 2026-07-03 |
| 0040 | Adopt the "Flow Chevrons" brand mark (supersedes ADR-0013)      | Superseded by 0058 | 2026-07-03 |
| 0041 | Per-stream comment watermarks (fix cross-stream drop)           | Accepted | 2026-07-16 |
| 0042 | Unrecognised `/everflow` verbs become freeform instructions     | Accepted | 2026-07-16 |
| 0043 | Standing scope-discipline instruction on every runner prompt    | Accepted | 2026-07-16 |
| 0044 | Per-spec model selection for the runner                         | Accepted | 2026-07-16 |
| 0045 | Work-phase `DecisionContinue` means "partial slice, remainder follows" | Accepted | 2026-07-16 |
| 0046 | `SyncWithBase` — refresh the feature branch before conflict-resolution turns | Accepted | 2026-07-16 |
| 0047 | `HasWorkBeyondBase` — the "did the runner do anything" check    | Accepted | 2026-07-17 |
| 0048 | File-presence-as-marker for first-run Skill install             | Accepted | 2026-07-17 |
| 0049 | Sqlite-backed EventStreamer event log (supersedes ADR-0033's in-memory-log consequence) | Accepted | 2026-07-17 |
| 0050 | `ReactToNote` — instant emoji acknowledgement of picked-up comments | Accepted | 2026-07-17 |
| 0051 | `everflow setup` picks and persists a default runner + model             | Accepted | 2026-07-17 |
| 0052 | `.everflow.yml` holds a per-repo free-text `title_convention`            | Accepted | 2026-07-17 |
| 0053 | Reconciliation sweep for Runs stuck on a lost event                      | Accepted | 2026-07-17 |
| 0054 | Wire `title_convention` into MR title generation via the runner         | Accepted | 2026-07-18 |
| 0055 | Rename the project from everflow to syntropy                            | Accepted | 2026-07-19 |
| 0056 | Decision marker must be alone on its line                                | Accepted | 2026-07-19 |
| 0057 | Decision marker tag renamed to `<syntropy-decision>`, both names accepted | Accepted | 2026-07-20 |
| 0058 | Adopt the "Syntropy" brand mark — chaos converging into order (supersedes ADR-0040) | Accepted | 2026-07-20 |
| 0059 | Retry-with-backoff on fetch to ride out shared base_repo ref-lock contention | Accepted | 2026-07-20 |
| 0060 | Detect MR merge/close by polling MR state, not by inferring it from comments | Accepted | 2026-07-20 |
| 0061 | Per-Run cooldown after a reconciler re-trigger                          | Accepted | 2026-07-20 |
| 0062 | Configure ErrBackOff and PauseAfterErrCount on every step                | Accepted | 2026-07-21 |
| 0063 | GitLab provider resolves its token fresh per request, not once at startup | Accepted | 2026-07-21 |
| 0064 | Strip nested Claude Code session env vars from the daemon and its subprocesses | Accepted | 2026-07-21 |
| 0065 | Poke `glab` to force its own token refresh before reading its config | Accepted | 2026-07-21 |
| 0066 | `invokeForEvent`'s DecisionContinue commits and pushes, like Done       | Accepted | 2026-07-21 |
| 0067 | GitHub provider resolves its token fresh per request, not once at startup | Accepted | 2026-07-22 |
| 0068 | RetryCI decision for automatic CI-failure triage                        | Accepted | 2026-07-22 |
| 0069 | Wire `DecisionRetryCI` into `invokeForEvent` with a 3-retry cap          | Accepted | 2026-07-22 |
| 0070 | Direct-SQL deletion for terminal-Run retention cleanup                  | Accepted | 2026-07-22 |
| 0071 | Retention sweeper loop wired into the daemon                           | Accepted | 2026-07-23 |
| 0072 | Non-author review comments — auto-fix defects, ask on steering          | Accepted | 2026-07-23 |
| 0073 | Mandatory decision marker on every turn; the runner never pushes or rewrites history | Accepted | 2026-07-27 |
| 0074 | Commit/push stray work on every `invokeForEvent` decision, not just Continue/Done | Accepted | 2026-07-27 |
| 0075 | Feed pre-commit hook rejections back to the runner as `HookFailure`     | Accepted | 2026-07-27 |
| 0076 | Redispatch terminal MR-state events every tick, don't cache "detected" as "delivered" | Accepted | 2026-07-27 |
| 0077 | MR merge/close bypasses the Paused early-return                        | Accepted | 2026-07-27 |
| 0078 | Wire pre-commit hook rejections into `invokeForEvent` with a bounded retry loop | Accepted | 2026-07-27 |
| 0079 | Reactive 401 retry-with-forced-refresh in the provider `do()` methods   | Accepted | 2026-07-27 |
| 0080 | `GetMRState` surfaces merge-conflict status alongside lifecycle state   | Accepted | 2026-07-27 |
| 0081 | Poller dispatches `EventMRConflict` from `MRState.HasConflict`         | Accepted | 2026-07-27 |
| 0082 | `.syntropy.yml` fields distinguish "never asked" from "asked, declined" | Accepted | 2026-07-28 |
| 0083 | `syntropy config check` — pure-code repo-config check, agent only for the gap | Accepted | 2026-07-28 |
| 0084 | `EventMRConflict` invokes the resolve-conflict subagent after the Paused gate, not before | Accepted | 2026-07-28 |
| 0085 | `CreateMR` absorbs a duplicate-branch conflict as idempotent success     | Accepted | 2026-07-28 |
| 0086 | `syntropy resume` detects Failed/Cancelled before trying `/control`     | Accepted | 2026-07-28 |
| 0087 | Abandon confirmation accepts a plain "yes", not just the repeated command | Accepted | 2026-07-28 |
| 0088 | goreleaser release workflow, no Homebrew tap for now                    | Accepted | 2026-07-28 |
| 0089 | `DecisionNoChange` replies to a human comment, stays silent on CI events | Accepted | 2026-07-28 |
| 0090 | Reconciler's `LastProgress` uses the record's own `UpdatedAt`, not just `History` | Accepted | 2026-07-29 |
| 0091 | Dedicated `<syntropy-title-update>`/`<syntropy-description-update>` tags, applied by the harness | Accepted | 2026-07-29 |
| 0092 | Feed unparseable decision markers back to the runner as `ParseFailure` | Accepted | 2026-07-29 |
| 0093 | Poller dispatches Runs concurrently, bounded by a semaphore              | Accepted | 2026-07-29 |
