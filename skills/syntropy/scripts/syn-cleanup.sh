#!/usr/bin/env bash
# Remove a finished Run's worktree and state. Usage: syn-cleanup.sh <run>
. "$(dirname "$0")/lib.sh"; need git
load_run "${1:?run}"; repo=$(st_get "$RUN" .base_repo)
git -C "$repo" worktree remove --force "$WT" 2>/dev/null || true
git -C "$repo" for-each-ref --format='%(refname:short)' "refs/heads/syn/$RUN/" |
  while read -r br; do git -C "$repo" branch -qD "$br" 2>/dev/null || true; done
rm -rf "$(run_dir "$RUN")"; echo cleaned
