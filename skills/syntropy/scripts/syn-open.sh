#!/usr/bin/env bash
# Commit the worktree, push, open a draft MR/PR. Usage:
#   syn-open.sh <run> <unit-id> "<title>" < description
# Exit 3 = worktree has no changes (hallucination guard): blacklist the unit.
. "$(dirname "$0")/lib.sh"; need jq git
load_run "${1:?run}"; unit=${2:?unit-id}; title=${3:?title}
[ "$(st_get "$RUN" .mode)" = adopt ] && die "adopted run: the MR already exists; use syn-push.sh"
desc=$(cat)
cd "$WT"
slug=$(tr -c '[:alnum:]' '-' <<<"$unit" | sed 's/^-*//;s/-*$//' | cut -c1-40)
branch="syn/$RUN/$slug"
git checkout -q -B "$branch"
git add -A
# Binary blobs never ride along (ADR-0032).
while IFS=$'\t' read -r a d f; do
  [ "$a" = "-" ] && [ "$d" = "-" ] && { git reset -q -- "$f"; echo "skipped binary: $f" >&2; }
done < <(git diff --cached --numstat)
git diff --cached --quiet && { echo "NO_CHANGES: nothing to commit for $unit" >&2; exit 3; }
co=${SYN_COAUTHOR:-Claude <noreply@anthropic.com>}
git commit -q -m "$title" -m "Co-Authored-By: $co"
git push -q -u origin "$branch" 2> >(grep -v "^remote:" >&2)
existing=$(pf find_mr "$branch")
if [ -n "$existing" ]; then out=$(jq -nc --arg i "$existing" '{id:$i,url:""}')
else out=$(printf '%s\n' "$desc" | pf create_mr "$branch" "$title"); fi
id=$(jq -r .id <<<"$out"); url=$(jq -r .url <<<"$out")
seed=$(PROV=$PROV pf notes "$id" | jq -c '[.[].key]')
st_upd "$RUN" '.mr={id:$i,url:$u,branch:$b}|.unit=$un|.seen=$s|.ci_handled=[]|.retries=0|.conflict_handled=false' \
  --arg i "$id" --arg u "$url" --arg b "$branch" --arg un "$unit" --argjson s "$seed"
echo "$out"
