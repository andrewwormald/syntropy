# ADR-0110: `CheckoutExistingBranch` prunes stale worktrees before adopting

**Status**: Accepted
**Date**: 2026-08-11

## Context

A live incident hardened `syntropy adopt` (ADR-0109) surfaced a second gap
beyond the branch-name bug: adopt is precisely the recovery path for a Run
that died abnormally (crash, `kill -9`, host replaced) — and that same dead
Run may have left `baseRepo`'s `git worktree list` pointing at a worktree
directory that no longer exists on disk, because it was never torn down via
`RemoveWorktree`.

`CheckoutExistingBranch` (`internal/git/git.go`) is the step that checks out
the MR's real branch during adopt. If a stale registration exists for that
branch, `git worktree add <dir> <branch>` fails outright with `fatal:
'<branch>' is already used by worktree at '<stale-dir>'` — even though
nothing is actually using it. Adopt would then fail every time for that MR,
with no way to recover short of a human manually running `git worktree
prune` on the host.

## Decision

`CheckoutExistingBranch` runs `git worktree prune` on `baseRepo` before
attempting `worktree add`, clearing any registrations whose directories are
gone. This is best-effort (errors are ignored) — if pruning fails, the
subsequent `worktree add` still runs and surfaces its own error, so nothing
is masked when the branch is genuinely in use by a live worktree.

This guarantees a clean slate on adoption: a dead prior Run's leftover
worktree registration can never block a later adopt of the same MR.

## Alternatives considered

- **Prune inside `setup()`'s adopt branch (`internal/refactorsweep/workflow.go`),
  before calling `CheckoutExistingBranch`** — works, but scatters the
  knowledge of "adopt can hit stale worktree registrations" outside the git
  package that owns worktree lifecycle. Rejected in favor of fixing it at
  the source of the `worktree add` call, where `RemoveWorktree` already
  prunes for the same reason.
- **Detect the specific "already used by worktree" error and retry once
  after pruning** — narrower blast radius, but pruning unconditionally is
  cheap (a no-op when there's nothing stale) and simpler than string-
  matching git's stderr.

## Consequences

- Every `CheckoutExistingBranch` call now does one extra `git worktree
  prune` — negligible cost, and consistent with `RemoveWorktree`'s existing
  use of the same command for the same class of staleness.
- `EnsureBranch` (the non-adopt path, used for brand-new units) is
  unchanged — a fresh unit branch has no prior registration to collide
  with, so pruning there would be dead weight.
