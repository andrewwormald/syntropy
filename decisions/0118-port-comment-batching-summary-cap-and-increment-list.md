# ADR-0118: Port comment batching, the summary cap, and the increment-list rule to the skill

**Status**: Accepted
**Date**: 2026-10-28

## Context

Six daemon MRs were open when [ADR-0117](0117-syntropy-as-a-skill-with-bundled-scripts.md)
removed the Go code. They cannot merge. Each fixed a real problem. Each problem
still exists in the skill:

| Old MR | Problem | Still true in the skill? |
|---|---|---|
| planner follows the increment list | Oversized increments. 30+ update rounds. | Yes. The agent picks units itself. |
| batch review comments | Eight comments gave eight turns, eight pushes. | Yes. `syn-wait.sh` exited on the first new comment. |
| cap posted summaries | Bot comments had no length limit. | Yes. The agent writes every comment. |
| README docs | Described the above. | Moot. README was rewritten. |

Those MRs also claimed ADR numbers 0117–0119. ADR-0117 now has a different meaning.

## Decision

Port the behaviour, not the code.

- **Increment list is binding.** `SKILL.md` tells the agent: next unshipped line
  of **Planned increments** is the next unit. One line, one MR. No bundling. No
  invented scope. A wrong list is fixed in the spec, with the user told.
- **Comments batch.** `syn-wait.sh` holds new comments until `SYN_QUIET` seconds
  (default 180) pass with no new one. `SYN_QUIET_MAX` (default 900) caps the wait
  after the first comment. One event carries all of them. An author `/syntropy`
  command flushes at once. CI failures, conflicts, merge and close are not delayed.
  They carry any waiting comments in `items[]`.
- **Comments are short.** `SKILL.md` asks for 60 words, 90 at most. `syn-say.sh`
  cuts prose past `SYN_MAX_WORDS` (default 90), keeps fenced code whole, and marks the cut.

## Alternatives considered

- **Rebase the six MRs onto the skill.** Each changes deleted Go files. A rebase
  keeps no code. It also keeps the clashing ADR numbers.
- **Only document the rules.** The batching rule needs timing. A script keeps
  time for free. Prose cannot.
- **Hard-fail an over-long comment.** The agent would retry and spend tokens. A
  clamp costs nothing.

## Consequences

- One agent turn per reviewer burst, not one per comment. The agent session
  waits up to 3 minutes longer for a comment fix. Set `SYN_QUIET=0` to turn it off.
- A cut comment can lose meaning. The marker shows the cut. The prompt rule is the
  main control. The clamp is the backstop.
- The 60-word aim is a prompt rule only. Nothing checks it below 90.
