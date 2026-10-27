# ADR-0003: Single long-lived daemon, not cron-driven ticks

**Status**: Superseded by [ADR-0117](0117-syntropy-as-a-skill-with-bundled-scripts.md)
**Date**: 2026-06-15

## Context

Everflow needs to drive workflow loops over hours and days. Two execution
models were considered:

- **Long-lived daemon**: a single process holds the `luno/workflow` runtime
  in memory, calls `wf.Run(ctx)`, and processes events for the lifetime of
  the process.
- **Cron-driven ticks**: a cron entry calls `everflow tick` every N minutes;
  each tick processes pending events and exits. The daemon is the cron job;
  there is no long-lived process.

## Decision

Run a **single long-lived daemon**. The user starts it (`everflow ...` in
the foreground; wrapped in `launchd`/`systemd`/`tmux` for real deployments);
it stays up until a signal stops it.

This is the idiomatic shape for `luno/workflow` — its event consumers,
timeouts, and outbox publishers all assume an in-process runtime. Cron-tick
mode would require reimplementing those as polling loops.

## Alternatives considered

- **Cron-driven ticks** — zero install (any machine with cron works) but:
  the tick interval becomes the minimum reaction time (a 30-minute cron
  job can't react to a webhook within seconds), state has to be flushed/
  rehydrated every tick, and the workflow library's in-memory event bus
  doesn't apply.
- **Hybrid** — daemon as primary, cron tick as fallback if the daemon is
  down. Killed for v1 because it doubles the test surface for a fallback
  most users won't need (you either have a server or you don't).

## Consequences

- The daemon owns the workflow runtime. CLI subcommands like `status`/
  `stop`/`logs` (future) must talk to the daemon via an IPC mechanism
  (Unix socket, or a small HTTP server), not by reading shared state.
- The user is responsible for keeping the daemon running (`systemd`,
  `launchd`, `tmux`, `nohup`). We document the patterns; we don't ship
  a service installer in v1.
- Persistent state (sqlite, or similar) becomes necessary for the daemon
  to survive restarts without losing in-flight Runs. The current PoC uses
  in-memory adapters, which is *fine for the PoC but loses runs on restart*.
  A future ADR will record the sqlite (or other) choice.
