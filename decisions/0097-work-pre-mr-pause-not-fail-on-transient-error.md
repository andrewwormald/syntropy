# ADR-0097: `work()` pauses (not fails) on a transient runner/git error before the MR exists, and resumes back into `StatusWorking`

**Status**: Accepted
**Date**: 2026-08-03

## Context

`work()` (the `StatusWorking` step) previously treated every error the same
way, regardless of cause: capture it in `AgentState.LastError` and return
`StatusFailed`, a terminal state. That's right for unrecoverable config (an
unknown provider name, no `Runners`/`Git` configured) or a runner's own
deliberate refusal (`DecisionFail`) — those won't resolve themselves, and
the honest thing is to stop and let a human start a fresh Run. It's wrong
for the git/runner calls in between: `EnsureBranch`, the `claude` exec
itself, `HasWorkBeyondBase`, `Commit`, `DiffShortstat`, `Push`, and the
worktree isolation check. A dropped SSH connection, a rate-limited API
call, or a `claude exec` crash are exactly the kind of blip the workflow
library's own `PauseAfterErrCount` circuit breaker (ADR-0062) already
treats as recoverable for `setup()`/`discover()` — but `work()`'s
capture-into-LastError-then-return-nil pattern (deliberately chosen in an
earlier fix to dodge the library's *unbounded* pre-ADR-0062 retry loop, see
the 2026-06-24 pre-commit-hook incident) bypasses that circuit breaker
entirely, so every one of these errors previously ended the Run for good.

Increment-1 of this spec (see `AddStep(StatusWorking, ...)`'s destination
list) already added `StatusPaused` as a valid graph destination from
`StatusWorking`, anticipating this change but deferring the actual
reclassification and the harder problem it exposes: **how does a Run
paused before any MR exists ever get resumed?** Every other `StatusPaused`
site in this package assumes at least one in-flight MR to comment
"paused"/"resumed" on and for the poller to watch for a reply. A pre-MR
pause has neither — the poller (`internal/poller`) only walks
`AgentState.InFlight`, and a Run with `CurrentUnit` set but no MR yet has
nothing in there. Without a fix, a Run parked this way would sit forever;
nothing would ever synthesize the event needed to invoke `resume()`.

## Decision

**1. Reclassify `work()`'s error sites (`pauseWork` helper, `workflow.go`).**
Transient runner/git failures — `EnsureBranch`, `verifyIsolatedWorktree`,
`runner.Run` (after `maxParseRetries` is exhausted), `HasWorkBeyondBase`,
`Commit` (non-`ErrNoChanges`), `DiffShortstat`, `Push` — now go through
`pauseWork`, which sets both `LastError` (diagnostic detail) and
`PauseReason` (human-facing) and returns `StatusPaused`. Everything else —
missing `CurrentUnit`/provider/`Runners`/`Git`, an unknown runner name,
`DecisionFail` (the runner's own refusal), and an unexpected `Decision`
value — stays `StatusFailed`. `CreateMR` failures are deliberately **left
as `StatusFailed`**: a provider API error is neither a "runner" error nor a
"git" error per the spec's own framing, and CreateMR is the boundary where
the pre-MR phase ends, not a step inside it. Reclassifying it is left to a
future ADR if it turns out to matter in practice (see Alternatives).

**2. Route `StatusPaused → StatusWorking` through `cmdResume`/`cmdRetry`
(`controls.go`), keyed on `CurrentUnit != "" && len(InFlight) == 0`.** That
combination is unambiguous and already distinct from the two existing
cases each command handles:

| `CurrentUnit` | `InFlight` | Meaning                                          | Destination         |
|---------------|------------|---------------------------------------------------|---------------------|
| set           | empty      | pre-MR pause (this ADR)                            | `StatusWorking`     |
| empty         | empty      | `discoverSpec`'s `DecisionAsk` pause (ADR-0094)    | `StatusDiscovering` |
| —             | non-empty  | any other pause (mid-flight, filter, hook, ...)    | `StatusAwaitingMerge` |

Returning `StatusWorking` re-invokes the `AddStep(StatusWorking, d.work,
...)` step for the same `CurrentUnit`, actually retrying the failed
operation — unlike `cmdRetry`'s existing post-MR behavior, which only
clears `PauseReason` and expects an external event (a re-comment, a CI
rerun) to naturally retrigger `resume()`. There's no external event
available pre-MR, so retrying immediately is the only useful behavior.

**3. The human path is the CLI, not an MR comment.** There's no MR yet to
comment `/syntropy resume`/`/syntropy retry` on. `syntropy resume <run-id>`
(ADR-0037/ADR-0062) already POSTs to the daemon's `/control` endpoint when
reachable, which synthesizes a `provider.Event{Kind: EventNoteAdded, MR:
provider.MR{}, Note: {Body: "/syntropy " + verb}}` and dispatches it via
`wf.Callback` — `ev.MR` is the documented zero-value case
(`controlHandler`'s own comment: "None, the control handlers will attempt
to post to MR IID=0 which will fail gracefully"). `postBotReply` calling
`PostComment` with `IID=0` is expected to error and is ignored, exactly
like every other control-verb handler already tolerates. A direct `POST
/control {"verb":"retry"}` reaches the same path.

## Alternatives considered

- **Return the real Go `error` from `work()` for these sites and let the
  library's `ErrBackOff`+`PauseAfterErrCount` circuit breaker (ADR-0062)
  retry/auto-pause, mirroring `setup()`.** Rejected: that auto-pause is
  `workflow.RunStatePaused`, a library-level concept distinct from our own
  `AgentStatus.StatusPaused` (see ADR-0062's own distinction). Increment-1
  already wired `StatusPaused` as the graph destination and the spec's
  wording ("pause and be resumable") points at our own business-level
  pause, which is visible via `syntropy status` and carries a
  `PauseReason` — the auto-pause path can't set `PauseReason` at all
  (`onAutoPause`'s doc: the failing step's own `AgentState` mutations
  aren't persisted on the erroring attempt). Using our own `StatusPaused`
  keeps the pause reason legible and reuses the resume-routing machinery
  this ADR adds, instead of adding a second, less-visible pause mechanism
  alongside it.
- **Extend the reconciler (ADR-0033/ADR-0090) to auto-retrigger a stuck
  pre-MR pause, instead of requiring a human to run `syntropy resume`.**
  Not done: `reconciler.IsStuck` explicitly excludes `StatusPaused`
  ("a deliberate stop rather than a lost event" — see its doc comment),
  and a transient error that already survived `maxParseRetries` (for the
  runner) or a single git call isn't obviously safe to retry unboundedly
  without a backoff/count ceiling of its own. Punting to the human path
  (which already exists via `syntropy resume`) is the smaller change; an
  auto-retry policy for this specific pause shape can be layered on later
  if manual intervention proves too slow in practice.
- **Reclassify `CreateMR` failures as transient too.** Considered and
  rejected for this increment: the spec text scopes the change to
  "runner/git errors", and a `CreateMR` failure can be either a network
  blip (transient) or a permissions/branch-protection misconfiguration
  (unrecoverable) with no cheap way to tell them apart from the error
  string alone. Leaving it `StatusFailed` is the conservative default;
  worth revisiting with a dedicated ADR if operational experience shows
  `CreateMR` blips are common enough to matter.
- **Give `cmdRetry` a bespoke "no MR, nothing to retry" error message
  instead of routing to `StatusWorking`.** Rejected: it's strictly more
  useful to actually retry the operation when that's unambiguously what
  "retry" must mean here (there is no other operation to retry), and it
  keeps `cmdRetry`/`cmdResume` symmetric.

## Consequences

- A transient runner/git blip during `work()` no longer ends the Run.
  `syntropy status <run-id>` shows `StatusPaused` with a `PauseReason`
  describing what failed; `syntropy resume <run-id>` retries `work()` for
  the same `CurrentUnit` (worktree, branch, and any `PromptInjection`/plan
  state are untouched — only `PauseReason`/`LastError` are cleared).
- `StatusFailed` now means what the spec asked for: unrecoverable config,
  or a deliberate abandonment (`DecisionFail`, `/syntropy abandon`/`stop`).
  It no longer catches ordinary infrastructure noise.
- `cmdResume`/`cmdRetry`'s `CurrentUnit != "" && InFlight empty` check is
  now the third leg of a table both functions must keep in sync (see the
  decision table above) — a future change to either pause shape (e.g. a
  new pre-planner pause) needs to re-examine both `cmdResume` and
  `cmdRetry`, not just one.
- `CreateMR` and everything downstream of it in `work()` is unchanged and
  stays `StatusFailed` on error, as does every config-shaped guard clause
  at the top of `work()`.
