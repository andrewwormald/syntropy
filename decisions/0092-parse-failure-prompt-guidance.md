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

`internal/refactorsweep/workflow.go`'s `work()` and `invokeForEvent` both
wrap the runner invocation in a bounded retry loop: when `rn.Run` returns
an error wrapping `claude.ErrNoDecisionMarker`, the loop sets
`req.ParseFailure` to the error text and retries the turn, up to
`maxParseRetries` (2) times, mirroring the `maxHookRetries` cap pattern
from ADR-0075/ADR-0076. Exceeding the cap falls back to the existing
failure/pause behaviour unchanged.

## Consequences

- A runner turn that emits a malformed or missing decision marker now
  gets up to `maxParseRetries` extra turns to self-correct before
  `work()` fails the unit or `invokeForEvent` pauses the Run for a
  human — the same UX improvement ADR-0075 gave hook rejections.
- Only `ErrNoDecisionMarker` triggers a retry; other `ParseDecision`
  errors (e.g. an unrecognised decision verb) still fail/pause on the
  first occurrence, since those aren't the class of failure this ADR
  targets.
