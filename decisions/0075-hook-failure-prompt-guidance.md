# ADR-0075: Feed pre-commit hook rejections back to the runner as `HookFailure`

**Status**: Accepted
**Date**: 2026-07-27

## Context

When the runner tries to commit during an `address_comment`/`fix_ci` turn
and the target repo's own pre-commit hooks reject the commit (lint,
formatting, secret scanning, file-size caps, etc.), `invokeForEvent`
currently treats that exactly like any other `git.Commit` error: it pauses
the Run and asks a human to intervene (see the `git Commit failed during
%s` branch in `internal/refactorsweep/workflow.go`). That's the wrong
default for this specific failure — a hook rejection is almost always
something the runner itself can see and fix (reformat the file, remove the
offending pattern) if it's told what the hook said. Pausing for a human
on every hook rejection is a worse experience than giving the runner one
more turn to self-correct.

## Decision

Add `Request.HookFailure` (`internal/runner/runner.go`), following the
existing shape of `CIFailure`/`CommentBody`: a plain string, empty unless
this invocation is a retry after a hook rejection, holding the hook's
rejection output.

Wire it into `claude.BuildPrompt` (`internal/runner/claude/claude.go`): a
new `## Commit rejected by pre-commit hook` block, rendered only when
`HookFailure != ""`, containing the raw hook output followed by
`hookFailureGuidance` — a fixed instruction to read the hook's complaint,
fix it, and commit again, explicitly telling the runner not to bypass the
hook with `--no-verify` (which would just trade one problem for a worse
one: an unverified commit landing on the branch).

This increment only threads the field through `Request` and the prompt.
Populating `HookFailure` from an actual hook rejection and retrying the
runner turn (rather than pausing) is deliberately left to a follow-on
increment — see `internal/refactorsweep/workflow.go`'s `invokeForEvent`,
which still pauses unconditionally on any non-`ErrNoChanges` `git.Commit`
error today.

## Consequences

- No behavior changes yet: `HookFailure` is never set by any caller in
  this increment, so `BuildPrompt` output is unchanged for every existing
  invocation.
- The follow-on increment can wire `invokeForEvent` to detect a hook
  rejection specifically (vs. other commit errors), set
  `req.HookFailure`, and retry the runner turn a bounded number of times
  before falling back to the existing pause — mirroring the
  `DecisionRetryCI` cap pattern from ADR-0068/ADR-0069.
