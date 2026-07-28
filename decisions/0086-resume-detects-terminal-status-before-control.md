# ADR-0086: `syntropy resume` detects Failed/Cancelled before trying `/control`

**Status**: Accepted
**Date**: 2026-07-28

## Context

Found live (same incident as ADR-0085): after run `5ec1a36b` terminated
in `Failed`, `syntropy resume 5ec1a36b` reported `✓ resume sent` but had
no effect — the run's status and `updated_at` were unchanged 20+ seconds
later.

`cmdResume` (`main.go`) already has a `directResume` path that correctly
revives `Failed`/`Cancelled`/`Paused` runs to `StatusDiscovering` via a
direct store write, and its own comment says as much: "the direct path
is needed for Cancelled/Failed → Discovering revive." But that path was
only reachable in two cases: the daemon being unreachable, or the Run
being auto-paused by the `PauseAfterErrCount` circuit breaker (ADR-0062,
detected via the daemon's `/status` `auto_paused` flag). A **reachable**
daemon with a **genuinely terminal, non-auto-paused** Failed/Cancelled
Run fell through to `sendControl(..., "resume", "")` — a POST to
`/control` that the daemon accepts and returns `HTTP 200` for
unconditionally, because writing the control event to the outbox
succeeds regardless of whether anything downstream is actually listening
for it. There's no `wf.Callback` consumer registered for a terminal
status, so the write is accepted but never acted on — `cmdResume` saw no
HTTP error and reported success for an operation that silently did
nothing.

## Decision

`cmdResume` now calls `daemonStatusFor` once up front and checks it for
two conditions before ever attempting `sendControl`:

1. `AutoPaused` (existing behavior, unchanged) → `directResume`.
2. `Status` is `Failed` or `Cancelled` (new) → `directResume`, the same
   path, regardless of whether the daemon itself is reachable.

Only if neither condition matches does `cmdResume` fall through to the
`sendControl`-first / direct-store-write-as-fallback path that already
existed — which remains correct for the case it's actually meant for:
reviving a `Paused` Run, where the daemon's `/control` → `wf.Callback`
dispatch **does** have a registered consumer and genuinely works.

## Alternatives considered

- **Make `sendControl`/`/control` itself report an error when there's no
  registered callback for the target status.** Rejected: that's a
  change to the daemon's outbox/callback semantics for a problem that's
  really about the CLI picking the wrong path — `/control`'s "accept the
  write, no error" behavior is fine for the cases it's meant to serve
  (Paused → resume). The bug is `cmdResume` not checking Status before
  choosing a path, not `/control` being too lenient.
- **Always try `directResume` first, `sendControl` second, for every
  status.** Rejected: `directResume` explicitly rejects statuses other
  than `Cancelled`/`Failed`/`Paused` ("only Cancelled, Failed, or Paused
  runs can be resumed via direct store write") — routing every resume
  through it first would mean an extra failed attempt (and a
  daemon-restart-required warning in its output) on the common case of
  resuming a healthy `Paused` Run through a live daemon, which is the
  one case that already worked correctly before this fix.

## Consequences

- `syntropy resume <run-id>` on a genuinely `Failed`/`Cancelled` Run now
  actually revives it to `Discovering` via the daemon-reachable path,
  matching what its own comments already claimed the tool did.
- Same caveat `directResume` already carries applies here too: reviving
  via direct store write requires the daemon to pick up the resulting
  outbox event on its next poll — a `Failed`/`Cancelled` Run's step
  consumer may need the daemon restarted to notice, same as the existing
  daemon-unreachable fallback path's own printed warning.
- No change to `Paused` Run resumption — that still goes through
  `sendControl` first exactly as before.
