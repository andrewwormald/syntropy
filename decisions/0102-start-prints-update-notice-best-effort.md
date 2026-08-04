# ADR-0102: `syntropy start` prints a best-effort update notice after triggering

**Status**: Accepted
**Date**: 2026-08-04

## Context

Syntropy has no push or auto-update mechanism: it's a CLI an agent invokes,
not a long-running service that could poll for its own updates. Without
something actively surfacing "a newer release exists," users only find out
by manually checking GitHub. The `internal/update` package (added in a
prior increment alongside `Config.UpdateCheckedAt`/`UpdateLatestVersion`)
already implements the check itself — `update.Check` compares the running
binary's version against the latest GitHub release, caching the result in
`~/.syntropy/config.yaml` for `update.CacheTTL` (24h) so repeated calls
don't hit the GitHub API every time. That package was deliberately left
unwired from any command.

`syntropy start` is the command a calling agent runs to kick off a sweep —
it already talks to the daemon and prints a confirmation line, so it's a
natural point to surface an update notice without needing any new command
or background process.

## Decision

`cmdStart` (`main.go`) calls a new `printUpdateNotice` helper immediately
after printing the "Triggered run ..." confirmation, once the daemon POST
has already succeeded. `printUpdateNotice` calls `update.Check` with the
CLI's `version` build variable and, if a newer release is available, prints
one line:

```
A newer syntropy release is available: v0.5.0 (you have v0.4.0)
```

The check is best-effort: any error from `os.UserHomeDir()` or
`update.Check` (offline, GitHub API hiccup, corrupt config, etc.) is
silently swallowed. `start`'s job is to trigger a run; a failed update
check must never turn a successful trigger into a command failure, and
must never block or slow down the trigger itself since it runs after
the response the agent actually cares about.

## Alternatives considered

- **Check before triggering, fail/warn early.** Rejected: couples an
  unrelated concern (release freshness) to the trigger's success path,
  and risks the check itself timing out on a slow network before doing
  what the agent actually asked for.
- **A separate `syntropy update check` command.** Rejected: nothing would
  call it — the whole point (per the spec goal) is that the calling agent
  can surface the notice without needing to remember to ask, or a
  push/auto-update mechanism to remind it.
- **Print the notice via stderr/logger instead of stdout.** Rejected: the
  calling agent reads `start`'s stdout directly to parse the run ID; stdout
  keeps the notice in the same stream the agent already reads, no extra
  plumbing needed for it to notice and relay the message on.

## Consequences

- `start`'s stdout may now include one extra line beyond the "Triggered
  run ..." confirmation; agents/scripts parsing that output should key off
  the "Triggered run" line rather than assuming it's the only output.
- The 24h cache means at most one GitHub API call per day regardless of
  how many times `start` runs, keeping this well within GitHub's
  unauthenticated rate limit.
