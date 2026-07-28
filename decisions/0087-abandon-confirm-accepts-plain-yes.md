# ADR-0087: Abandon confirmation accepts a plain "yes", not just the repeated command

**Status**: Accepted
**Date**: 2026-07-28

## Context

ADR-0026's two-tap abandon flow asks the author "Are you sure?" — a
yes/no question — but until now only accepted confirmation by repeating
the exact `/syntropy abandon` command a second time. Direct feedback
from the user: "the confirmation is yes or no. it is psychologically
different than having to repeat the same command to do something." The
prompt's own phrasing sets up a yes/no expectation that the accepted
reply didn't match — a real reviewer replying `yes` to "Are you sure?"
would have that reply silently treated as "any other activity," which
cancels the abandonment instead of confirming it.

## Decision

`internal/refactorsweep/controls.go` adds `isAffirmativeConfirmation`, a
small case-insensitive, punctuation-trimmed check against `yes`, `y`,
`confirm`, `confirmed`. `resume()`'s `StatusAwaitingAbandonConfirm`
branch accepts this alongside the existing "second `/syntropy abandon`"
check — either one now confirms and reacts to the note the same way; the
second `/syntropy abandon` path is left in place unchanged (repeating
the command still works) rather than removed, since some users may
prefer it and it costs nothing to keep both. The confirmation prompt's
own wording is updated to `Reply \`yes\` (or \`/syntropy abandon\`
again)...` so the accepted replies match what's actually asked.

Deliberately narrow matching (exact word after trim, not a substring
search): a reply like "yes I saw this, will look tomorrow" does **not**
confirm — only an unambiguous, standalone affirmative does. This mirrors
the same "don't guess intent from prose" caution already applied
elsewhere in the control-verb system (e.g. ADR-0056's "marker must be
alone on its line").

## Alternatives considered

- **Also add explicit "no"/"cancel" recognition**, rather than relying on
  "any other activity cancels" to implicitly handle a "no" reply.
  Rejected for this fix: the existing generic cancel path already
  produces a clear "abandon cancelled" acknowledgement for literally any
  non-confirming reply, including a plain "no" — there's no broken
  behavior here to fix, just an asymmetry in how explicit the messaging
  is. Not addressed to keep this fix scoped to the reported problem.
- **Fuzzy/substring matching** ("yes" appearing anywhere in the reply).
  Rejected: too easy to false-positive on a reply that happens to
  contain the word "yes" without meaning to confirm (see the
  `TestCmdAbandon_YesWithinLongerSentence_DoesNotConfirm` regression
  test) — a wrong confirmation here is much more costly (closes MRs,
  cancels the Run) than a wrong cancellation (a re-request is trivial).

## Consequences

- The author can now confirm abandonment with a plain `yes` reply, matching
  the psychological framing of the "Are you sure?" prompt.
- `/syntropy abandon` repeated a second time still works identically —
  no behavior removed, only added.
- Any future confirmation-style prompt in this system (if one is added)
  should consider the same "does the accepted reply match what the
  prompt actually asks" check this ADR fixes — a yes/no question should
  accept yes/no, not just a repeated command.
