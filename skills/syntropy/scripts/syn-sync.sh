#!/usr/bin/env bash
# Merge the base branch into the unit branch. Exit 0 clean; exit 2 = conflicts
# (prints the files; resolve them, then run syn-push.sh).
# Usage: syn-sync.sh <run>
. "$(dirname "$0")/lib.sh"; need git
load_run "${1:?run}"; cd "$WT"
git fetch -q origin "$BASE"
if git merge --no-edit "origin/$BASE" >/dev/null 2>&1; then echo "SYNCED"; else
  echo "CONFLICTS:"; git diff --name-only --diff-filter=U; exit 2
fi
