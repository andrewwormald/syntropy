# ADR-0075: Provider auth-expiry pauses are internal-only, no MR comment

**Status**: Accepted
**Date**: 2026-07-27

## Context

`handleProviderAuthEvent` (ADR-0038) parks a Run when the poller's local
`gh`/`glab` CLI token has expired (HTTP 401), and previously posted a
comment on the Run's in-flight MR/PR explaining this and telling the
author to refresh credentials and restart the daemon.

Found live: an expired token on the operator's own machine has nothing
to do with the MR's author or reviewer — they can't refresh syntropy's
local CLI credentials, and the pause already resolves itself the moment
the token is refreshed and the next poll succeeds (`EventProviderAuthRestored`
clears it automatically, no human action needed on the MR at all). The
comment was pure operational noise landing on a real, external MR for a
problem that lives entirely on syntropy's own host.

## Decision

`handleProviderAuthEvent` still parks the Run with the same
`providerAuthPausePrefix` reason (used for `syntropy status`/`list`
visibility and to gate the idempotent-restore check), but no longer
calls `postBotComment`. It logs via `slog.Warn`/`slog.Info` instead —
visible to whoever operates the daemon, not to the MR's participants.

## Alternatives considered

- **Route it through a different, operator-only channel** (Slack, email)
  instead of just a log line. Rejected for now: syntropy has no such
  channel today, and `slog` output is already what an operator watching
  the daemon (or its log file) sees; adding a new notification channel
  is a bigger change than this fix warrants. `syntropy list`/`status`
  already surface the pause reason for anyone checking manually.

## Consequences

- Every prior single-MR test that asserted a comment was posted on auth
  failure now asserts the opposite (`TestResume_AuthFailure_ParksRunWithProviderAuthPrefix`).
- If a future need arises for surfacing this to a wider audience (e.g. a
  team channel), that's a new notification mechanism to design, not a
  reason to reintroduce the MR comment — the MR's participants still
  aren't the right audience for "the daemon operator's CLI token expired."
