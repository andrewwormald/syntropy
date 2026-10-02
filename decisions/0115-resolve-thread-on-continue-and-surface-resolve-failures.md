# ADR-0115: Every "handled it" reply resolves the thread, and a failed resolve is reported

**Status**: Accepted
**Date**: 2026-10-02

## Context

Found live: the operator had to run a *separate* Claude agent over the
repo's PRs to mark review threads resolved before GitHub's auto-merge
would fire. Every thread syntropy had already replied to was sitting open.

Two independent causes, both in `invokeForEvent`
(`internal/refactorsweep/workflow.go`):

1. **`DecisionContinue` deliberately left the thread open.**
   [ADR-0066](0066-invokeforevent-continue-commits.md) gave Continue the
   same commit/push path as Done but gated discussion resolution behind
   `isDone`, reasoning that "resolving it would misrepresent an unfinished
   conversation as settled". The reasoning was sound in isolation and
   wrong in effect: the same ADR notes that `invokeForEvent` "has no
   automatic re-invocation loop", so nothing continues a Continue on its
   own. The open thread therefore bought no automatic follow-up — it only
   blocked auto-merge until a human resolved it by hand, for an MR
   syntropy had already pushed a real, reviewed slice to.

2. **Three paths swallowed the resolve error entirely.** The
   no-code-change paths (Done/Continue with no work, `ErrNoChanges` with
   an empty shortstat, and `DecisionNoChange` on a human comment, the
   last from [ADR-0089](0089-nochange-replies-to-human-comments.md)) all
   called `_ = p.ResolveDiscussion(...)`. Only the pushed-work path
   surfaced a failure. So a resolve that failed for a real reason — a
   403, an expired token, GitHub's GraphQL rate limit exhausted (the
   provider's `ResolveDiscussion` is the only GraphQL caller, and its
   thread lookup costs points on every call) — produced an MR that looked
   answered, stayed unmergeable, and had nothing anywhere in the reply
   thread or the Run's state to say why.

## Decision

A single helper, `resolveOriginatingThread`, is now the only way
`invokeForEvent` resolves a thread. It no-ops on an empty
`DiscussionID`, and on a provider error posts an informational reply
**in that thread** naming the error and stating that an open thread
blocks auto-merge. It never fails the turn or changes the Run's status —
the change is already pushed; manual resolution is a working fallback.

Every path that has just told the reviewer their comment was handled
calls it:

- `DecisionDone` with pushed work — unchanged behaviour, now via the helper.
- **`DecisionContinue` with pushed work — new.** The thread is resolved.
  The remainder stays visible through the existing wording difference
  ("🔄 Partial progress … More work is needed — comment again (or reply
  `/syntropy prompt <text>`) to continue"), which is the part of
  ADR-0066's `isDone` split that was actually doing the work.
- Done/Continue with no work, and `ErrNoChanges` with an empty
  shortstat — unchanged behaviour, failures no longer swallowed.
- `DecisionNoChange` on an `EventNoteAdded` — same.

Paths that end in a pause (`Ask`, `Fail`, `RetryCI`, runner/git errors)
still never resolve: those MRs are not merging anyway, and the open
thread is where the human picks the work back up.

The resolve now runs **after** the summary reply, not before. Both
platforms collapse a resolved thread, so resolving first hid the "here's
what I did" reply behind an expander.

## Alternatives considered

- **Give `invokeForEvent` a real re-invocation loop so Continue reaches
  Done on its own, then keep resolution Done-only.** This is the
  principled fix and ADR-0066 already considered (and rejected) its
  remainder-tracking half. Rejected again here for the same reason: it is
  a much larger change to the event path, and it would not touch cause 2
  at all. Worth revisiting on its own merits — it would make Continue's
  resolution honest rather than pragmatic.
- **Resolve on every decision, including the pauses.** Rejected: a paused
  Run needs a human, and resolving the thread removes the one marker in
  the review UI showing where that human should look.
- **Leave resolution alone and document "resolve threads manually" in
  the README.** Rejected: that is precisely the manual step the operator
  was already automating with a second agent. Babysitting an MR to
  mergeable is what the daemon is for.

## Consequences

- A reviewer thread that syntropy pushed a fix to is resolved whether the
  runner said Done or Continue, so GitHub/GitLab auto-merge is no longer
  blocked by syntropy's own replies. No second agent needed.
- A Continue-resolved thread is collapsed while real work remains. The
  reply says so, but a reviewer skimming resolved threads can miss it.
  This is the deliberate trade accepted above; the fix is the
  re-invocation loop in the first alternative, not re-opening the thread.
- Resolve failures are now visible on the MR itself, which also makes the
  GraphQL rate-limit failure mode diagnosable instead of silent.
- ADR-0066's commit/push decision stands unchanged; only its
  discussion-resolution half is superseded.
