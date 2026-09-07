# ADR-0113: OpenHands Agent Server — five schema corrections from a local spike (amends ADR-0112)

**Status**: Accepted
**Date**: 2026-09-07

## Context

[ADR-0112](0112-openhands-agent-server-runner-design.md) designed
`internal/runner/openhands/` from OpenHands' published OpenAPI schema and
docs — desk research, not a live spike — and said as much in its
Consequences: "a future increment should run an actual
`openhands-agent-server` locally and confirm the endpoint shapes, status
transitions, and event ordering assumed here."

That increment happened: a genuine local spike against a running
`openhands-agent-server` v1.44.1 (2026-09-07), not more desk research. It
found five places where the real server disagrees with ADR-0112's
assumptions, all fixed in
[#186](https://github.com/andrewwormald/syntropy/pull/186) and
[#187](https://github.com/andrewwormald/syntropy/pull/187).

## Decision

**ADR-0112's endpoint-shape assumptions are corrected as follows; its
architectural decisions (subprocess-per-Run, reused marker protocol,
NeverConfirm policy, planning-registry split) stand unchanged.**

1. **Console script name.** The installed binary is `agent-server`, not
   `openhands-agent-server` — there is no script literally named
   `openhands-agent-server`. `Runner.Binary` and `spawnServer`'s default
   now both use `agent-server`.
2. **`initial_message` is an object, not a bare string.** The server
   rejects a plain string with "Input should be a valid dictionary or
   object." It expects a `SendMessageRequest` shape: `{"content":
   [{"type": "text", "text": "..."}]}`. `createConversationRequest` now
   sends this structure instead of a raw string.
3. **`confirmation_policy.kind` is `"NeverConfirm"`, not
   `"auto_approve"`.** The server rejects `"auto_approve"` with "Unknown
   kind." `NeverConfirm` is the real `ConfirmationPolicyBase`
   discriminator value.
4. **`ConversationExecutionStatus` real values are `idle`, `running`,
   `paused`, `waiting_for_confirmation`, `finished`, `error`, `stuck`,
   `deleting`.** There is no `stopped` value, and the confirmation-pause
   state is named `waiting_for_confirmation`, not `awaiting_user_input`.
   `pollUntilDone` now treats `finished`, `error`,
   `waiting_for_confirmation`, `stuck`, `paused`, and `deleting` as
   terminal; `converse` still treats `waiting_for_confirmation` as a
   runner-level error rather than `DecisionAsk`, per ADR-0112 §3 — that
   reasoning is unaffected by the name correction.
5. **The agent's final reply is read via `GET
   /api/conversations/{id}/agent_final_response`**, the server's
   purpose-built endpoint for "the final response text, extracted from
   either a FinishAction message or the last agent MessageEvent" — not by
   hand-parsing the discriminated union from `GET .../events/search` as
   ADR-0112 assumed. That union's message payload also lives under
   `llm_message`, not `message`, which the desk research got wrong; using
   `agent_final_response` avoids depending on the union's field names at
   all.

A sixth fix landed in the same line of work but is not a correction to
ADR-0112's desk research: `Runner.APIKey` / `Runner.BaseURL` were missing
from `llmConfig` entirely, so no adapter design in ADR-0112 addressed LLM
credential threading. That gap is closed (`agent.llm.api_key` /
`agent.llm.base_url` populated from `Runner.APIKey`/`Runner.BaseURL` in
`createConversation`), but it is a completion of scope, not a correction
of a wrong assumption.

## Alternatives considered

- **Fold these corrections into ADR-0112 in place** — rejected. ADR-0112
  is a record of what the desk research concluded and why; overwriting it
  would erase the fact that the original assumptions were wrong and lose
  the audit trail this repo's ADR practice depends on ([ADR-0012](0012-track-decisions-as-adrs.md)).
  An amendment ADR is the established pattern for "we later learned X was
  wrong" without rewriting history.
- **No ADR at all, let the code comments speak for themselves** —
  rejected. The code comments in `openhands.go` do document each
  correction inline, but the spec for this line of work explicitly
  requires an ADR amending ADR-0112, and a decision this consequential
  (every wire-format assumption in the original design turned out wrong)
  belongs in the decisions log where a future contributor scanning ADRs
  chronologically will see it, not just in a diff.

## Consequences

- ADR-0112's Consequences section said "no tagged release should ship the
  openhands adapter until it's been run against a real Agent Server
  locally." That gate is now satisfied — the corrections here are the
  result of that local run, not more desk research — but the local-test
  gate historically applied in this repo to security/access-control specs
  applies here by the same logic: verify these corrections hold against a
  real running `agent-server` again if the adapter changes further, don't
  rely on unit tests against mocks alone.
- Future OpenHands Agent Server upgrades may drift these values again
  (new `ConversationExecutionStatus` members, a renamed endpoint). Treat
  any such drift the same way: a local spike first, then an ADR amendment
  recording what changed and why, not a silent code fix.
