# ADR-0092: Feed unparseable decision markers back to the runner as `ParseFailure`

**Status**: Accepted
**Date**: 2026-07-29

## Context

When a runner turn's response has no valid decision marker — malformed,
missing, or otherwise unparseable by `claude.ParseDecision`
(`internal/runner/claude/claude.go`, returns `ErrNoDecisionMarker`) —
`invokeForEvent` currently has no way to tell the runner what went wrong
and ask it to fix its own output. The only path today is to pause the Run
and ask a human to intervene. That's the wrong default for this specific
failure: an unparseable response is almost always something the runner
itself can see and fix (emit the tag, put it alone on its own line, use a
recognised verb) if it's told what the parser rejected. This mirrors
ADR-0075's `HookFailure`, which made the same call for pre-commit hook
rejections — a bounded number of self-correction retries beats pausing on
every occurrence.

## Decision

Add `Request.ParseFailure` (`internal/runner/runner.go`), following the
existing shape of `HookFailure`/`CIFailure`: a plain string, empty unless
this invocation is a retry after a parse failure, holding the parse error.

Wire it into `claude.BuildPrompt` (`internal/runner/claude/claude.go`): a
new `## Previous response could not be parsed` block, rendered only when
`ParseFailure != ""`, containing the raw parse error followed by
`parseFailureGuidance` — a fixed instruction to end the response with
exactly one well-formed `<syntropy-decision>` tag, alone on its own line,
using one of the documented verbs.

This increment only threads the field through `Request` and the prompt.
Populating `ParseFailure` from an actual `ErrNoDecisionMarker` and
retrying the runner turn (rather than pausing) is deliberately left to a
follow-on increment — see `internal/refactorsweep/workflow.go`'s
`invokeForEvent` and `work()`, which still pause unconditionally on a
`ParseDecision` error today.

## Consequences

- No behavior changes yet: `ParseFailure` is never set by any caller in
  this increment, so `BuildPrompt` output is unchanged for every existing
  invocation.
- The follow-on increment can wire the retry loops in
  `internal/refactorsweep/workflow.go` to detect `ErrNoDecisionMarker`
  specifically, set `req.ParseFailure`, and retry the runner turn a
  bounded number of times before falling back to the existing pause —
  mirroring the `maxHookRetries` cap pattern from ADR-0075/ADR-0078.
