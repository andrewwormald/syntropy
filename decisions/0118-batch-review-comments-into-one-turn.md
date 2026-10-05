# ADR-0118: Review comments are batched into one turn per quiet window

**Status**: Accepted
**Date**: 2026-10-05

## Context

Feedback from a user who ran syntropy for five MRs: "most of my mrs got
bloated to 30+ updates", and past a certain size the threads and comments
became hard to sort out. Thirty update rounds on one MR is not a planning
problem — it is one update per comment.

That is exactly what the code did. The poller lists every new note since
its watermark and dispatches **one `EventNoteAdded` per note**
(`internal/poller/poller.go`); webhook delivery is one event per comment by
definition. `resume` then handed each event straight to `invokeForEvent`,
which runs a full runner turn, commits and pushes. So a reviewer working
down a diff and leaving eight comments got eight runner turns, eight
commits and up to eight pushes — eight force-refreshes of the MR for anyone
watching it, and eight separate "✓ Addressed" replies to read.

Nothing was wrong with any individual turn. The problem is that review
feedback arrives in bursts and was being processed one item at a time.

ADR-0117 covers the other half of the complaint (increments too big in the
first place). This ADR covers the update volume.

## Decision

A review comment on an in-flight MR is **queued**, not acted on. The queue
drains into one runner turn once it has been quiet for a while.

- `AgentState.PendingNotes` holds the queue per unit, durable in the record
  store, so a daemon restart mid-window loses no comment.
- `commentBatchWindow` = **3 minutes** of quiet after the *last* comment.
  Chosen to cover a normal review pass (comments seconds to a couple of
  minutes apart) while still answering a lone drive-by comment promptly.
- `maxCommentBatchDelay` = **15 minutes** after the *first* queued comment,
  bounding the slide. Without it a reviewer commenting every two minutes
  could defer the turn indefinitely while the earliest commenter waits.
- A `workflow` timeout on `StatusAwaitingMerge` drives the drain. With an
  empty queue the TimerFunc returns the zero time, so a Run nobody is
  commenting on carries no timeout row at all.
- `invokeForEvent` takes the whole batch: one prompt listing every comment,
  one commit, one push, then a reply **and** a resolve on each originating
  thread (ADR-0115).

Deliberately **not** batched:

- **CI failures and conflicts.** Already one event per occurrence, with no
  human waiting on a reply.
- **Comments during a pause.** The Run needs the answer now, and the pause
  already serialises the work.
- **`/syntropy` control verbs.** Handled before the filter, unchanged.
- **The "eyes" reaction.** Posted when the comment is *queued*, not when the
  batch runs — the whole point of it is that the commenter sees their
  comment was picked up, and a silent three-minute gap defeats that.

Escape hatches, because three minutes is a long time when you are watching:
`/syntropy retry` and `/syntropy prompt <text>` drain the queue immediately,
and `/syntropy status` reports how many comments are queued and when the
turn is due.

Supporting details:

- Re-delivery is deduplicated by note ID — the poller and a webhook can both
  surface the same comment.
- `CommenterIsAuthor` is all-or-nothing across the batch: one outside
  reviewer makes the whole turn reviewer feedback, so ADR-0072 still routes
  solution-steering suggestions to `Ask`.
- The risk screen runs over every comment in the batch, and a flagged
  comment's pause names which one tripped it.
- A unit that merges or is blacklisted mid-window has its queue dropped —
  there is no MR left to push to.
- The timeout store does not deduplicate and the TimerFunc re-runs on every
  store while in `AwaitingMerge`, so a unit collecting comments accumulates
  several timeout rows with near-identical deadlines. The drain is
  idempotent: the first row to fire does the work, the rest find an empty
  queue and no-op.

## Alternatives considered

- **Cap the number of update rounds per MR and warn past it.** Rejected: it
  treats the symptom (too many updates) and leaves the cause (one update per
  comment) in place. A reviewer leaving ten legitimate comments deserves ten
  answers, just not ten pushes.
- **Batch in the poller — one event per tick instead of one per note.**
  Tempting, and it would fix the common path with a smaller diff. Rejected:
  webhook delivery would still be one event per comment, so the fix would
  only apply to poll-driven Runs and would leave the two event sources
  behaving differently. Batching in the workflow covers both.
- **A fixed window from the first comment rather than a sliding quiet
  period.** Rejected: a reviewer two minutes into a ten-minute pass would
  get the first half of their feedback answered while still writing the
  second half, producing exactly the extra update round this change exists
  to remove. The sliding window with a hard cap gets both properties.
- **Process comments one at a time but only push once, at merge time.**
  Rejected: the push is what triggers CI and what a reviewer looks at, so
  deferring it hides whether the fix works until the end.

## Consequences

- A review pass now costs one update round instead of one per comment. This
  is the change the feedback asked for.
- An answer takes up to three minutes to appear. For a single comment on a
  quiet MR this is strictly slower than before, and it is the main cost of
  this decision; the reaction, the status line and the two drain verbs exist
  to make the wait legible rather than to remove it.
- Comments are durable state now. A Run holding queued comments has work
  pending that is visible nowhere in its status fields except
  `PendingNotes` — `/syntropy status` reports it for exactly that reason.
- `resume` no longer invokes the runner for a note on an in-flight MR, so
  any future code reading "a comment arrives → a turn runs" has to go
  through the drain. Tests use a `resumeAndDrain` helper to keep that path
  explicit rather than mocking the timer.
