#!/usr/bin/env bash
# Commit pending work and push to the MR branch (never force). Also completes a conflict merge.
# Usage: syn-push.sh <run> "<commit message>"
# Prints PUSHED <sha> or NOCHANGE. Exit 3 = conflict markers remain. Exit 2 = remote moved, conflicts.
. "$(dirname "$0")/lib.sh"; need git
load_run "${1:?run}"; msg=${2:?message}; cd "$WT"
b=$(st_get "$RUN" .mr.branch); [ -n "$b" ] || b=$(git rev-parse --abbrev-ref HEAD)
git add -A
bad=$(git diff --cached --check 2>&1 | grep 'conflict marker' | cut -d: -f1 | sort -u || true)
[ -z "$bad" ] || { echo "UNRESOLVED:"; echo "$bad"; exit 3; }
git diff --cached --quiet || git commit -q -m "$msg" -m "Co-Authored-By: ${SYN_COAUTHOR:-Claude <noreply@anthropic.com>}"
git fetch -q origin "$b" 2>/dev/null || true
if [ -n "$(git rev-list "HEAD..origin/$b" 2>/dev/null)" ]; then   # a human pushed too: merge, never force
  git merge --no-edit "origin/$b" >/dev/null 2>&1 || { echo "CONFLICTS:"; git diff --name-only --diff-filter=U; exit 2; }
fi
if git rev-parse -q --verify "origin/$b" >/dev/null && [ -z "$(git rev-list "origin/$b..HEAD")" ]; then echo NOCHANGE; exit 0; fi
git push -q origin "HEAD:$b" 2> >(grep -v "^remote:" >&2)
echo "PUSHED $(git rev-parse --short HEAD)"
