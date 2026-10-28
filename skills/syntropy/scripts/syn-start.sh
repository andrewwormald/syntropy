#!/usr/bin/env bash
# Create a Run from a spec. Usage: syn-start.sh <spec.md>. Prints the run id.
. "$(dirname "$0")/lib.sh"; need jq git
spec=${1:?usage: syn-start.sh <spec.md>}; [ -f "$spec" ] || die "no such spec: $spec"
fm() { awk -v k="$1" 'NR==1&&$0!="---"{exit} NR>1&&$0=="---"{exit} NR>1&&index($0,k":")==1{sub("^"k":[ \t]*","");gsub(/^["'\'']|["'\'']$/,"");print;exit}' "$spec"; }
goal=$(fm goal); PROV=$(fm provider); PROJ=$(fm project)
miss=""; for k in goal provider project; do [ -n "$(fm $k)" ] || miss="$miss $k"; done
[ -z "$miss" ] || die "spec missing required field(s):$miss"
case $PROV in gitlab) need glab;; github) need gh;; *) die "provider must be gitlab or github";; esac
status=$(fm status); case ${status:-ready} in ready|"") ;; *) die "spec status is '$status', not ready";; esac
repo=$(fm base_repo); repo=${repo:-$PWD}; repo=${repo/#\~/$HOME}
BASE=$(fm base_branch); BASE=${BASE:-main}
draft=$(fm draft_mrs); [ "$draft" = true ] || draft=false   # ready MRs by default; opt in with draft_mrs: true
git -C "$repo" remote get-url origin >/dev/null || die "$repo has no origin remote"
id=$(printf '%s%s' "$spec" "$(date +%s)$$$RANDOM" | shasum | cut -c1-8)
rd=$(run_dir "$id"); mkdir -p "$rd"; cp "$spec" "$rd/spec.md"
git -C "$repo" fetch -q origin "$BASE"
git -C "$repo" worktree add -q --detach "$rd/worktree" "origin/$BASE"
author=$(PROV=$PROV PROJ=$PROJ pf user)
jq -n --arg id "$id" --arg goal "$goal" --arg prov "$PROV" --arg proj "$PROJ" --arg base "$BASE" \
  --arg repo "$repo" --arg author "$author" --argjson draft "$draft" \
  --argjson max "${SYN_MAX_UNITS:-50}" \
  '{id:$id,mode:"sweep",goal:$goal,provider:$prov,project:$proj,base_branch:$base,base_repo:$repo,author:$author,
    draft_mrs:$draft,max_units:$max,status:"working",paused:false,done:[],blacklisted:[],prompts:[],
    mr:{},unit:"",seen:[],ci_handled:[],retries:0,conflict_handled:false}' > "$rd/state.json"
"$(dirname "$0")/syn-update.sh" >&2 || true   # best-effort notice
echo "$id"
