# ADR-0119: Bot comments are capped at 60 words, 90 hard, code excluded

**Status**: Accepted
**Date**: 2026-10-05

## Context

Part of the same feedback that produced ADR-0117 and ADR-0118: a user found
that once an MR had accumulated enough syntropy activity, "the threads and
comments" were what became hard to sort out. Batching (ADR-0118) cuts the
*number* of comments. This ADR cuts their *length*.

Nothing bounded a bot comment. Every reply posts the runner's `Summary`
verbatim — `✓ Addressed (address_comment): <summary>` and the handful of
sibling shapes — and the only guidance the runner ever received about that
text was "the text before the tag becomes the recorded Summary". A runner
inclined to narrate produced several paragraphs explaining work the diff
already shows, on every turn, on every thread.

The cost lands on the reviewer, who has to read all of it to find the one
sentence that says whether their comment was addressed.

## Decision

Two layers, because a prompt rule alone will not hold and a clamp alone
produces mangled text.

**The prompt is the mechanism.** `decisionProtocol` — in both
`internal/runner/claude` and its mirror in `internal/runner/openhands`, so
the cap doesn't depend on which runner a Run uses — now states:

- 60 words or fewer; 90 only when a shorter summary would actually mislead.
- Fenced code blocks, diffs and file paths don't count toward the limit.
- Show the change instead of describing it: a short list, a before/after
  pair, or small ASCII art where structure communicates faster than prose.
- No preamble, and no restating the comment being answered.

**The clamp is the backstop.** `clampSummary` truncates to
`maxSummaryWords` (90) words of prose, passing fenced code through whole and
uncounted, and marks the cut with `… _(truncated)_`. It returns the input
unchanged when nothing was cut, so the common case is byte-for-byte what the
runner wrote.

The clamp applies only where a summary reaches a human as a comment. The
Run's own state — `PauseReason`, the plan's remainder note, `Turn` history —
keeps the full text, because truncating there loses information nobody is
reading in a comment thread.

## Alternatives considered

- **Prompt rule only.** Rejected: the summary is posted unconditionally, so
  a runner that ignores the rule (or a model swap that changes how well it
  follows it) goes straight to the MR with no backstop at all.
- **Clamp only, no prompt change.** Rejected: truncating at 90 words
  produces a comment that stops mid-thought. The prompt is what makes a
  60-word comment *good*; the clamp only makes a bad one *short*.
- **A character or byte limit.** Rejected: it would cut code blocks, which
  are the most useful part of a summary and the part a reviewer most wants
  intact.
- **Collapse long comments behind a `<details>` block instead of
  truncating.** Attractive, and it loses nothing. Rejected for now because
  it rewards verbosity — the goal is comments a reviewer can read at a
  glance, not long comments that are easier to skip. Reconsider if
  truncation turns out to cut things people actually wanted.

## Consequences

- A reviewer reads one short comment per batched turn instead of several
  long ones.
- A truncated comment is a signal, not a feature: it means the runner
  ignored the word limit in its own prompt. Worth noticing rather than
  tuning the clamp upward.
- The word budget rewards code over prose, which is the intended bias — a
  diff is checkable, a paragraph claiming the same thing is not.
- Both runner prompts now carry this rule, and a test in each package
  asserts it, so adding a third runner means carrying it there too.
