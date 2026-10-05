# ADR-0117: The spec's "Planned increments" list is authoritative for planning

**Status**: Accepted
**Date**: 2026-10-05

## Context

Feedback from a user who ran syntropy for five MRs: it works well on small
MRs, but "most of my mrs got bloated to 30+ updates", and past a certain
size the review threads became hard to sort out. Their own conclusion
matches the tool's premise — "where it shines is if you take a plan and
break it up into small tasks and drive it to work from small changes" —
which is exactly what syntropy is for. So the question is why a Run
produced increments big enough to need 30 rounds of review feedback.

Two causes, addressed separately. This ADR covers increment size; comment
batching is its own decision.

`buildPlanningPrompt` (`internal/refactorsweep/workflow.go`) hands the
planner the whole spec body, the plan history, and the instruction "Decide
the next increment toward implementing this spec." There is no sizing rule
of any kind, and — more importantly — no mention of the spec's own
**"Planned increments"** section.

That section is not incidental. The syntropy skill requires every spec to
carry it, one line per planned MR, 18 words maximum per line, and the
operator reviews that breakdown before the Run starts
(`internal/setup/SKILL.md`). It is the one place a human has already made
the "how small is small enough" judgement. The daemon then ignored it: the
list reached the planner only as undifferentiated prose inside `SpecBody`,
with nothing saying it was binding. A planner re-deriving the next
increment from the whole spec each cycle is free to bundle three reviewed
lines into one MR — and nothing in the Run ever notices, because the plan
history records whatever the planner decided as if it were the plan.

## Decision

`buildPlanningPrompt`'s `# Your task` block now states that a "Planned
increments" section, where present, **is** the plan:

- Propose the next line on it that hasn't shipped yet; that line's scope is
  the increment's scope. One line = one MR.
- Do not bundle two lines into one increment.
- Do not invent scope that isn't on the list.
- Re-order lines only where the spec itself states a dependency.
- When every line has shipped and the spec is implemented, return `Done`.

And, for a spec with no such section (hand-written, or predating the skill's
requirement), a fallback sizing rule: one coherent change a reviewer can
read, test and merge on its own — "an increment that would touch unrelated
concerns is two increments."

`applyIncrementScope` already threads the planner's chosen rationale into
the runner's `Goal` for the work turn, so a narrower increment narrows the
runner's scope with no further change.

## Alternatives considered

- **A generic size rule only ("roughly ≤N files"), leaving the list
  advisory.** Rejected: a file count is a proxy for reviewability, and a
  bad one — a 12-file mechanical rename is easier to review than a 2-file
  behaviour change. The list is a human's direct judgement about the right
  slice; a heuristic that overrides it is strictly worse. Kept only as the
  fallback for specs that have no list.
- **A `max_increment_files:` knob in the spec frontmatter.** Rejected for
  now: another dial to tune per Run, with the same proxy problem, and the
  list already encodes the intent. Reconsider if specs without a list turn
  out to be common.
- **Parse the "Planned increments" section in Go and drive the plan from it
  directly, bypassing the planner for scope selection.** Tempting, and it
  would make the rule structural instead of advisory. Rejected for this
  change: it couples the daemon to a markdown heading the skill happens to
  emit, and it removes the planner's ability to adapt when reality diverges
  from the plan (a line turns out to be already done, or blacklisted, or
  split by a `Continue`). Revisit if the prompt rule proves unreliable in
  practice — the failure would be visible as plan entries whose rationale
  doesn't match any line on the list.
- **Post-hoc guard: flag an oversized increment after it lands.** Rejected
  as the primary fix — it reports the problem after the MR exists, which is
  exactly when it's expensive to split. Still worth adding later as a
  detector for the alternative above.

## Consequences

- A reviewed, human-sized increment list is now load-bearing, which raises
  the stakes on writing it well. The skill's 18-words-per-line rule and the
  operator's review of the breakdown are the mechanism for that, and both
  already exist.
- Each line of the list must stand alone as a mergeable MR. A spec whose
  lines are interdependent will now surface that as a stuck Run rather than
  as one silently-bundled large MR.
- Specs with no "Planned increments" section still get a sizing rule, so the
  fix isn't limited to skill-authored specs.
- The planner can still diverge from the list — this is a prompt rule, not a
  parser. Divergence is detectable: a plan entry whose rationale matches no
  line on the list. Worth checking before concluding that the increment-size
  problem is fixed.
