#!/usr/bin/env bash
# Record a finished unit and reset the worktree to fresh base.
# Usage: syn-next.sh <run> merged|closed|skip
# Prints {"done":N,"blacklisted":N,"budget":"ok|reached","paused":bool}.
. "$(dirname "$0")/lib.sh"; need jq git
load_run "${1:?run}"; how=${2:?merged|closed|skip}; unit=$(st_get "$RUN" .unit)
if [ "$(st_get "$RUN" .mode)" = adopt ]; then
  st_upd "$RUN" '.status="done"'; echo '{"mode":"adopt","status":"done"}'; exit 0
fi
case $how in
  merged) st_upd "$RUN" '.done+=[$u]' --arg u "$unit";;
  closed|skip) st_upd "$RUN" '.blacklisted+=[$u]' --arg u "$unit";;
  *) die "unknown outcome: $how";;
esac
st_upd "$RUN" '.mr={}|.unit=""|.retries=0'
cd "$WT"
git reset -q --hard; git clean -qfd                       # discard stray work (ADR-0104)
git fetch -q origin "$BASE"; git checkout -q --detach "origin/$BASE"
jq -c '{done:(.done|length),blacklisted:(.blacklisted|length),
        budget:(if (.done|length)+(.blacklisted|length) >= .max_units then "reached" else "ok" end),
        paused:.paused}' "$(st_file "$RUN")"
