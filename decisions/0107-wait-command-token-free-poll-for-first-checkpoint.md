# ADR-0107: `syntropy wait` — token-free local poll for a Run's first checkpoint

**Status**: Accepted
**Date**: 2026-08-07

## Context

`syntropy start` (ADR-0028, ADR-0102) triggers a Run against the daemon and
returns immediately, printing a bare Run ID. The calling agent — typically
the Skill (`internal/setup/SKILL.md`) — has nothing concrete to report to
the human beyond "I started something," because a freshly triggered Run
spends its first stretch in `Initiated`/`Discovering` with no unit yet
opened, completed, or blacklisted. Polling for that first real signal by
re-invoking the agent loop would burn tokens on a wait that has nothing to
do with reasoning — it's pure state-machine progress the daemon is already
tracking in its Run record.

`syntropy status` (used by `cmdStatus`) already knows how to resolve a
Run ID (including prefixes) and read a `runStatusResponse` from either the
daemon's HTTP API or, if the daemon is unreachable, a direct sqlite read
via `store.Open` — see `daemonStatusFor`/`tryStoreFallback` in `main.go`.
`wait` reuses that exact resolution path rather than inventing a second one.

## Decision

Add `syntropy wait <run-id>`, implemented as `cmdWait` in `main.go`. It
loops calling `fetchRunStatus` (the same daemon-first/sqlite-fallback
helper `status` uses) on a local `--interval` (default 3s) until
`reachedFirstCheckpoint` reports true, then prints the resolved status and
exits 0. If no checkpoint is reached within `--timeout` (default 10m), it
exits non-zero with an error naming the Run's current status.

A Run has reached its first checkpoint when either:

- a unit has actually landed somewhere — `InFlight > 0`, `Completed > 0`,
  or `Blacklisted > 0` on the status response, or
- the Run itself has moved into a state that needs human attention or is
  otherwise terminal — `AwaitingMerge`, `Paused`, `AwaitingAbandonConfirm`,
  `Completed`, `Failed`, or `Cancelled` (matched by prefix against
  `refactorsweep.Status*.String()`, since some statuses carry a suffix).

Polling is local-only: every tick is a status read (daemon HTTP GET, or a
direct sqlite `Lookup` when the daemon is unreachable), never an agent
invocation. This makes `wait` safe for the Skill to call unconditionally
right after `start` without any budget concern.

## Alternatives considered

- **Have `start` block itself until the first checkpoint.** Rejected:
  `start`'s job is to trigger and return fast (ADR-0102 already treats its
  response path as latency-sensitive for the update-notice check); folding
  in a multi-minute wait changes its contract for every caller, including
  ones that explicitly want fire-and-forget.
- **Webhook/callback from the daemon back to the CLI.** Rejected: the CLI
  process invoking `start` isn't necessarily still listening by the time
  the first checkpoint lands, and building a callback channel is far more
  machinery than a local poll loop for a one-shot CLI command.
- **Treat `Discovering` itself as a checkpoint.** Rejected: `Discovering`
  is exactly the "nothing to show yet" state the Skill needs to wait past —
  reporting it back to the human is no more useful than the bare Run ID
  `start` already prints.

## Consequences

- The Skill can call `syntropy wait <run-id>` immediately after `start` and
  report something concrete (a unit opened, a pause reason, a completion)
  instead of a bare Run ID and a promise to check back later. Wiring that
  into the Skill's documented flow is a follow-up increment.
- `wait`'s exit code is meaningful: 0 means a checkpoint was observed
  (which may itself be `Failed`/`Cancelled` — callers must still inspect
  the printed status), non-zero means the timeout elapsed with the Run
  still stuck pre-checkpoint.
- Because `wait` shares `fetchRunStatus`/`reachedFirstCheckpoint` with
  `status`'s resolution logic, any future change to Run ID prefix
  resolution or the daemon-unreachable fallback benefits both commands
  automatically.
