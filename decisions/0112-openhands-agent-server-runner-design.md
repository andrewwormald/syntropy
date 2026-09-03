# ADR-0112: OpenHands runner — per-Run Agent Server subprocess, reused decision-marker protocol, planning stays Claude-only

**Status**: Accepted
**Date**: 2026-09-03

## Context

[ADR-0007](0007-pluggable-runner-interface.md) already names `openhands` as
a planned `Runner` — a "full agent session" class, distinct from claude's
"single-turn CLI" — but no `internal/runner/openhands/` package exists yet,
and ADR-0007/ADR-0027 both explicitly deferred its adapter design to a
future ADR. The goal for this line of work is to add OpenHands as a second
runner scoped to *execution only* (`work()` / `invokeForEvent()` in
`internal/refactorsweep/workflow.go`) — planning (`discoverSpec()`) stays
Claude-only, structurally, not just by default configuration.

Before writing any `internal/runner/openhands/` code, this ADR records
research into how OpenHands actually exposes a programmatic interface,
since claude's adapter shape (ADR-0027: shell out to `claude -p`, one
process per invocation, parse a decision marker from stdout) does not
obviously transfer.

### What OpenHands exposes

OpenHands' automation surface is the **Agent Server**
(`openhands-agent-server`, part of `OpenHands/software-agent-sdk`) — a
long-lived REST + WebSocket process, not a one-shot CLI like `claude -p`.
Relevant facts from its published OpenAPI schema
(`docs.openhands.dev/openapi/agent-sdk.json`) and docs
(`docs.openhands.dev/sdk/arch/agent-server`), as of 2026-09-03:

- **Lifecycle**: started as `python -m openhands.agent_server --host
  127.0.0.1 --port <p>` (or `uv run` equivalent). Liveness/readiness via
  `GET /alive`, `GET /health`, `GET /ready`. It is a server process that
  outlives any single task, unlike claude's per-call subprocess.
- **Auth**: session keys via `X-Session-API-Key`, configured server-side
  with `OH_SESSION_API_KEYS_*` / `OH_SECRET_KEY` env vars. The docs
  explicitly warn never to expose an unauthenticated instance beyond
  localhost, since it executes arbitrary shell/file tools.
- **Conversation lifecycle**: `POST /api/conversations` creates a
  conversation from an `agent` (LLM config + tool list) and a `workspace`
  (`working_dir`, kind `LocalWorkspace`) plus an `initial_message`
  (freeform text content). `POST /api/conversations/{id}/run` starts
  background execution. `GET /api/conversations/{id}` returns a
  `ConversationInfo` whose `ConversationExecutionStatus` enum is one of
  `running | stopped | finished | error | paused | awaiting_user_input`.
- **Events**: `GET /api/conversations/{id}/events/search` returns a
  discriminated union (`ActionEvent`, `MessageEvent`, `ObservationEvent`,
  `ErrorEvent`, ...) — the agent's final freeform reply is the last
  `MessageEvent`. `POST .../events/respond_to_confirmation` answers a
  paused tool call under a confirmation policy; `POST .../ask_agent`
  queries the agent without changing state.
- There is **no native "Decision" concept**. `ConversationExecutionStatus`
  distinguishes execution states (still running vs. finished vs. errored
  vs. paused for tool confirmation), not domain outcomes like our
  Continue/Ask/Done/Fail/NoChange (ADR-0008). `awaiting_user_input` fires
  from the confirmation-policy mechanism (a human approving a risky tool
  call), which is an orthogonal concept to `DecisionAsk` (pause and ask
  the MR author a question).

This is desk research against the currently-published schema and docs, not
a local spike against a running agent-server — see Consequences.

## Decision

**The adapter manages a per-Run Agent Server subprocess and talks to it
over HTTP, reusing ADR-0027's exact decision-marker text protocol instead
of inventing a new one.**

1. **Subprocess, not shared server.** `Run()` starts
   `openhands-agent-server` bound to a loopback ephemeral port, with
   `workspace.working_dir` set to `req.Worktree` (ADR-0006's worktree
   remains the blast-radius boundary), waits for `/ready`, and tears the
   process down when the invocation ends (success, error, or timeout).
   Never one long-lived server multiplexing conversations for multiple
   Runs.
2. **Same prompt, same marker protocol as claude.** The adapter composes
   the `initial_message` exactly the way `claude.go` composes its prompt
   per ADR-0027 §3/§1: Skill/Unit/Worktree headers, `req.Goal`, and the
   `<syntropy-decision>...</syntropy-decision>` footer. After `POST
   .../run`, the adapter polls `GET /api/conversations/{id}` until
   `execution_status` is `finished` or `error` (bounded by
   `req.Timeout`), reads the last `MessageEvent` text, and runs it through
   ADR-0027's *existing* `ParseDecision` — no new parser, no new protocol.
3. **Confirmation policy is set to auto-approve.** The conversation's
   `confirmation_policy` is configured so tool calls never pause for human
   confirmation (`respond_to_confirmation` is never needed) — same
   reasoning as ADR-0027 §4's unconditional
   `--dangerously-skip-permissions`: inside the worktree, autonomous tool
   use is the accepted risk; `awaiting_user_input` is therefore not a
   status this adapter expects to see in normal operation, and if it does,
   it's treated as a runner-level error, not a `DecisionAsk`.
4. **Planning stays Claude-only via a separate registry, not a runtime
   check.** A future implementation increment splits `Deps.Runners` into
   two `*runner.Registry` instances — one for execution (`work()` /
   `invokeForEvent()`, containing claude + openhands) and one for planning
   (`discoverSpec()`, containing claude only). `discoverSpec()` resolves
   against the planning-only registry regardless of
   `r.Object.RunnerName`, so there's no `if runnerName == "openhands"`
   guard to forget — the openhands adapter is structurally unreachable
   from the planning code path because it's never registered there.

## Alternatives considered

- **Drive the `openhands` CLI instead of the Agent Server API** — the CLI
  is built for an interactive TUI session (human watching output, steering
  turns), not a scripted single invocation with one parseable final
  answer. The Agent Server is OpenHands' documented integration point for
  "another service wants to start conversations and get results without
  embedding the SDK" — the situation this adapter is in.
- **Mirror claude's one-shot subprocess-per-call shape exactly** — rejected
  outright; OpenHands has no one-shot invocation mode. Its unit of work is
  a stateful conversation on a server, not a process that exits when the
  task is done.
- **Signal Decision natively via `ConversationExecutionStatus`** instead of
  a text marker — rejected. The enum has no Ask/Fail/NoChange distinctions,
  and conflates a confirmation-policy pause with "needs a human," which
  isn't what `DecisionAsk` means here. Reusing the marker keeps the
  cross-runner Decision contract (ADR-0008) uniform in one place
  (`ParseDecision`) rather than teaching `workflow.go` a second way to
  read intent per runner.
- **One shared agent-server process for the whole daemon**, conversations
  multiplexed by conversation ID — rejected: breaks worktree-per-Run
  isolation (ADR-0006), and a hung or crashed server takes down every
  in-flight openhands Run instead of just one.
- **Gate planning against openhands with a runtime check on `RunnerName`**
  instead of a separate registry — rejected because the spec calls for a
  structural guarantee: a runtime `if` is a rule someone can weaken by
  editing one line; a registry that never holds the openhands adapter
  makes "planning can't reach openhands" true by construction.

## Consequences

- Implementing `internal/runner/openhands/` needs: a subprocess lifecycle
  manager (spawn, health-poll `/ready`, kill on timeout/completion), a
  small HTTP client for `POST /api/conversations`, `POST .../run`, `GET
  /api/conversations/{id}`, `GET .../events/search`; and a dependency on
  ADR-0027's `ParseDecision` being exported/reusable across runner
  packages rather than private to `internal/runner/claude`.
- New deployment dependency: `openhands-agent-server` (Python package,
  needs a Python/`uv` runtime) must be available wherever the daemon runs
  openhands Runs — unlike claude's single-binary CLI requirement. Not
  addressed here; a follow-up ADR should cover packaging/CI.
- The `Deps.Runners` → `Runners` + `PlanningRunners` split, and the actual
  `internal/runner/openhands/` package, are follow-up implementation
  increments — out of scope for this ADR.
- This ADR is grounded in the published OpenAPI schema and docs, not a
  live spike. Before merging the implementation, a future increment should
  run an actual `openhands-agent-server` locally and confirm the endpoint
  shapes, status transitions, and event ordering assumed here — consistent
  with this repo's existing pattern (ADR-0027) of keeping unit tests
  install-free and treating anything needing a real install as spike-time
  validation. Per this repo's existing local-test-gate practice for
  higher-risk changes, no tagged release should ship the openhands adapter
  until it's been run against a real Agent Server locally, not just
  unit-tested against mocks.
