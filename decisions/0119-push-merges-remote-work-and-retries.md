# ADR-0119: A rejected push merges the remote work and retries

**Status**: Accepted
**Date**: 2026-10-28

## Context

The daemon paused a Run when `git push` was rejected with `(fetch first)`:
someone else had pushed to the MR branch. This happens often. A reviewer
uses "Update branch", a bot fixes a title, or a person adds a commit. A pause
needs a human for a problem the agent can solve.

`syn-push.sh` already merged remote commits that it saw before the push. It did
not handle a push that lost a race after that fetch. Git then reports
`fetch first`, `non-fast-forward`, or `cannot lock ref ... failed to update ref`.

## Decision

`syn-push.sh` loops up to three times: fetch, merge the remote branch if it is
ahead, push. It retries only when the push fails for one of those three reasons.

- Merge, not rebase, and never force. A rebase rewrites commits that a human
  may have pulled. A merge keeps both histories.
- Merge conflicts exit 2 with the file list. The agent resolves them and runs
  the script again.
- Any other refusal (protected branch, auth, a hook) exits 4 with the git
  output. The agent tells the user. A retry cannot fix these.

## Alternatives considered

- **Rebase onto the remote.** Cleaner history, but it rewrites the agent's
  commits and needs a force push if the agent had already pushed some.
- **Pause and ask, as the daemon did.** Safe, but it stops work for a case
  that has a safe automatic answer.
- **Retry forever.** A branch that moves on every push is a sign of a person
  or bot fighting the agent. Three tries, then ask.

## Consequences

- A human push no longer stops a Run. The agent sees a merge commit it did not write.
- If a human force-pushes (for example, a GitLab "Rebase"), local and remote
  have diverged. The merge brings back the old commits. The agent should tell the user.
