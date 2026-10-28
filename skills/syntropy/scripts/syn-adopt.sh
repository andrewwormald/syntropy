#!/usr/bin/env bash
# Take over an EXISTING MR/PR: keep CI green, handle comments and reviews.
# Usage (inside a clone of the project): syn-adopt.sh <url|number> [--new-only]
# Existing unresolved comments are delivered by syn-wait.sh unless --new-only.
# Prints the run id. Then call syn-wait.sh. Works on same-repo branches only.
. "$(dirname "$0")/lib.sh"; need jq git
ref=${1:?usage: syn-adopt.sh <url|number> [--new-only]}; newonly=0; [ "${2:-}" = --new-only ] && newonly=1
repo=$(git rev-parse --show-toplevel 2>/dev/null) || die "run inside a clone of the project"
if [[ $ref =~ ^https?://[^/]+/(.+)/-/merge_requests/([0-9]+) ]]; then PROV=gitlab; PROJ=${BASH_REMATCH[1]}; mr=${BASH_REMATCH[2]}
elif [[ $ref =~ ^https?://github\.com/([^/]+/[^/]+)/pull/([0-9]+) ]]; then PROV=github; PROJ=${BASH_REMATCH[1]}; mr=${BASH_REMATCH[2]}
elif [[ $ref =~ ^[0-9]+$ ]]; then
  u=$(git -C "$repo" remote get-url origin); mr=$ref
  PROJ=$(sed -E 's#^(git@|ssh://git@|https?://)[^/:]+[:/]##; s#\.git$##' <<<"$u")
  [[ $u == *github.com* ]] && PROV=github || PROV=gitlab
else die "give an MR/PR URL or a number"; fi
case $PROV in gitlab) need glab;; github) need gh;; esac
info=$(pf mr_info "$mr") || die "cannot read $PROV $PROJ !$mr (auth? wrong repo?)"
[ "$(jq -r .state <<<"$info")" = open ] || die "MR is $(jq -r .state <<<"$info"), not open"
[ "$(jq -r .fork <<<"$info")" = false ] || die "fork MRs are not supported (cannot push to the fork)"
branch=$(jq -r .branch <<<"$info"); BASE=$(jq -r .base <<<"$info")
me=$(pf user); mrauthor=$(jq -r .author <<<"$info")
[ "$me" = "$mrauthor" ] || echo "WARN: MR author is $mrauthor, you are $me. Only $me can send /syntropy commands." >&2
id=$(printf '%s%s' "$ref" "$(date +%s)$$$RANDOM" | shasum | cut -c1-8)
rd=$(run_dir "$id"); mkdir -p "$rd"
git -C "$repo" fetch -q origin "$branch" "$BASE"
git -C "$repo" worktree add -q -B "syn/$id/adopt" "$rd/worktree" "origin/$branch"
seen='[]'; [ $newonly = 1 ] && seen=$(pf notes "$mr" | jq -c '[.[].key]')
jq -n --arg id "$id" --arg prov "$PROV" --arg proj "$PROJ" --arg base "$BASE" --arg repo "$repo" --arg me "$me" \
  --arg mr "$mr" --arg url "$(jq -r .url <<<"$info")" --arg br "$branch" --arg title "$(jq -r .title <<<"$info")" \
  --argjson seen "$seen" \
  '{id:$id,mode:"adopt",goal:("babysit: "+$title),provider:$prov,project:$proj,base_branch:$base,base_repo:$repo,
    author:$me,draft_mrs:false,max_units:1,status:"working",paused:false,done:[],blacklisted:[],prompts:[],
    mr:{id:$mr,url:$url,branch:$br},unit:$title,seen:$seen,ci_handled:[],retries:0,conflict_handled:false}' > "$rd/state.json"
"$(dirname "$0")/syn-update.sh" >&2 || true   # best-effort notice
echo "$id"
