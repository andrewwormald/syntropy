#!/usr/bin/env bash
# Shared helpers for syntropy. Source this file; do not run it.
set -euo pipefail

SYN_HOME=${SYN_HOME:-$HOME/.syntropy}
MARK='<!-- syntropy -->'

die() { echo "ERROR: $*" >&2; exit 1; }
need() { for c in "$@"; do command -v "$c" >/dev/null || die "missing tool: $c"; done; }

run_dir() { echo "$SYN_HOME/runs/$1"; }
st_file() { echo "$(run_dir "$1")/state.json"; }
st_get() { jq -r "$2 // empty" "$(st_file "$1")"; }
# st_upd <run> <jq-filter> [jq args...]
st_upd() {
  local f flt=$2; f=$(st_file "$1"); shift 2
  jq "$@" "$flt" "$f" > "$f.tmp" && mv "$f.tmp" "$f"
}

load_run() {
  RUN=$1
  [ -f "$(st_file "$RUN")" ] || die "unknown run: $RUN"
  PROV=$(st_get "$RUN" .provider); PROJ=$(st_get "$RUN" .project)
  BASE=$(st_get "$RUN" .base_branch); MR=$(st_get "$RUN" .mr.id)
  WT=$(run_dir "$RUN")/worktree
  DRAFT=$(jq -r '.draft_mrs // false' "$(st_file "$RUN")")   # not st_get: it drops false
}

# pf <fn> [args]: dispatch to the provider layer (gl_* or gh_*).
pf() { local k=gh; [ "$PROV" = gitlab ] && k=gl; "${k}_$1" "${@:2}"; }
pe() { echo "${PROJ//\//%2F}"; }
JSONH=(-H 'Content-Type: application/json')

# ---- GitLab (glab api) -------------------------------------------------
gl_user() { glab api user | jq -r .username; }
gl_find_mr() { glab api "projects/$(pe)/merge_requests?state=opened&source_branch=$1" | jq -r '.[0].iid // empty'; }
# gl_create_mr <branch> <title>; description on stdin
gl_create_mr() {
  jq -Rs --arg s "$1" --arg t "$BASE" --arg ti "$2" --argjson d "$DRAFT" \
    '{source_branch:$s,target_branch:$t,title:(if $d then "Draft: "+$ti else $ti end),description:.}' |
    glab api -X POST "projects/$(pe)/merge_requests" --input - "${JSONH[@]}" |
    jq -c '{id:(.iid|tostring),url:.web_url}'
}
gl_mr_state() {
  glab api "projects/$(pe)/merge_requests/$1" | jq -c '{
    state:(if .state=="merged" then "merged" elif .state=="closed" then "closed" else "open" end),
    conflict:(.has_conflicts==true),
    ci:((.head_pipeline.status // "none") | if .=="success" then "passed" elif .=="failed" then "failed"
        elif (.=="none" or .=="canceled" or .=="skipped") then "none" else "pending" end),
    ci_ref:((.head_pipeline.id // "") | tostring)}'
}
gl_notes() {
  glab api --paginate "projects/$(pe)/merge_requests/$1/discussions?per_page=100" | jq -sc '
    (add // []) | [ .[] as $d | $d.notes[] | select(.system==false and (.resolved // false)==false)
      | {key:("n:"+(.id|tostring)),author:.author.username,bot:(.author.bot // false),
         body:.body,thread:$d.id} ]'
}
gl_comment() { jq -Rs '{body:.}' | glab api -X POST "projects/$(pe)/merge_requests/$1/notes" --input - "${JSONH[@]}" >/dev/null; }
gl_resolve() { [ -n "${2:-}" ] || return 0; echo '{"resolved":true}' | glab api -X PUT "projects/$(pe)/merge_requests/$1/discussions/$2" --input - "${JSONH[@]}" >/dev/null; }
gl_retry_ci() { glab api -X POST "projects/$(pe)/pipelines/$2/retry" >/dev/null; }
gl_ci_log() {
  local j
  for j in $(glab api "projects/$(pe)/pipelines/$2/jobs?scope=failed&per_page=5" | jq -r '.[].id'); do
    echo "--- job $j"; glab api "projects/$(pe)/jobs/$j/trace" | tail -n 40
  done
}
gl_close() { echo '{"state_event":"close"}' | glab api -X PUT "projects/$(pe)/merge_requests/$1" --input - "${JSONH[@]}" >/dev/null; }

gl_mr_info() {
  glab api "projects/$(pe)/merge_requests/$1" | jq -c '{branch:.source_branch,base:.target_branch,author:.author.username,
    title:.title,url:.web_url,fork:(.source_project_id!=.target_project_id),
    state:(if .state=="merged" then "merged" elif .state=="closed" then "closed" else "open" end)}'
}

# ---- GitHub (gh api) ---------------------------------------------------
gh_user() { gh api user -q .login; }
gh_find_mr() { gh api "repos/$PROJ/pulls?state=open&head=${PROJ%%/*}:$1" | jq -r '.[0].number // empty'; }
gh_create_mr() {
  jq -Rs --arg h "$1" --arg b "$BASE" --arg t "$2" --argjson d "$DRAFT" \
    '{head:$h,base:$b,title:$t,body:.,draft:$d}' |
    gh api -X POST "repos/$PROJ/pulls" --input - | jq -c '{id:(.number|tostring),url:.html_url}'
}
gh_mr_state() {
  local pr sha chk
  pr=$(gh api "repos/$PROJ/pulls/$1"); sha=$(jq -r .head.sha <<<"$pr")
  chk=$(gh api "repos/$PROJ/commits/$sha/check-runs?per_page=100" | jq -c .check_runs)
  jq -nc --argjson pr "$pr" --argjson c "$chk" '{
    state:(if $pr.merged then "merged" elif $pr.state=="closed" then "closed" else "open" end),
    conflict:($pr.mergeable_state=="dirty"),
    ci:(if ($c|length)==0 then "none"
        elif any($c[]; .conclusion=="failure" or .conclusion=="timed_out") then "failed"
        elif any($c[]; .status!="completed") then "pending" else "passed" end),
    ci_ref:$pr.head.sha}'
}
gh_notes() {
  local a b
  a=$(gh api --paginate "repos/$PROJ/issues/$1/comments?per_page=100" | jq -sc '(add // []) |
    map({key:("i:"+(.id|tostring)),author:.user.login,bot:(.user.type=="Bot"),body:.body,thread:""})')
  b=$(gh api --paginate "repos/$PROJ/pulls/$1/comments?per_page=100" | jq -sc '(add // []) |
    map({key:("r:"+(.id|tostring)),author:.user.login,bot:(.user.type=="Bot"),body:.body,
         thread:((.in_reply_to_id // .id)|tostring)})')
  jq -nc --argjson a "$a" --argjson b "$b" '$a+$b'
}
gh_comment() { jq -Rs '{body:.}' | gh api -X POST "repos/$PROJ/issues/$1/comments" --input - >/dev/null; }
gh_resolve() {
  [ -n "${2:-}" ] || return 0
  local tid
  tid=$(gh api graphql -f query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id comments(first:1){nodes{databaseId}}}}}}}' \
    -f o="${PROJ%%/*}" -f r="${PROJ##*/}" -F n="$1" |
    jq -r --argjson c "$2" '.data.repository.pullRequest.reviewThreads.nodes[]|select(.comments.nodes[0].databaseId==$c)|.id' | head -1)
  [ -n "$tid" ] || return 0
  gh api graphql -f query='mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{isResolved}}}' -f t="$tid" >/dev/null
}
gh_retry_ci() {
  local i
  for i in $(gh api "repos/$PROJ/commits/$2/check-runs?per_page=100" | jq -r '[.check_runs[]
      | select(.conclusion=="failure" or .conclusion=="timed_out")
      | .details_url | capture("/runs/(?<r>[0-9]+)")? | .r] | unique | .[]'); do
    gh api -X POST "repos/$PROJ/actions/runs/$i/rerun-failed-jobs" >/dev/null
  done
}
gh_ci_log() {
  local i
  for i in $(gh api "repos/$PROJ/commits/$2/check-runs?per_page=100" | jq -r '[.check_runs[]
      | select(.conclusion=="failure" or .conclusion=="timed_out")
      | .details_url | capture("/runs/(?<r>[0-9]+)")? | .r] | unique | .[]'); do
    echo "--- run $i"; gh run view "$i" -R "$PROJ" --log-failed 2>&1 | cut -f3- |
      sed $'s/\x1b\\[[0-9;]*m//g; s/^\xEF\xBB\xBF//; s/^[0-9T:.Z-]* //' | tail -n 40
  done
}
gh_mr_info() {
  gh api "repos/$PROJ/pulls/$1" | jq -c '{branch:.head.ref,base:.base.ref,author:.user.login,title:.title,url:.html_url,
    fork:((.head.repo.full_name // "")!=.base.repo.full_name),
    state:(if .merged then "merged" elif .state=="closed" then "closed" else "open" end)}'
}
gh_close() { echo '{"state":"closed"}' | gh api -X PATCH "repos/$PROJ/pulls/$1" --input - >/dev/null; }

# ---- Comment classification ---------------------------------------------
# ctl_parse <body>: prints "verb<TAB>args" when body starts with a
# /syntropy token (typo-tolerant, distance <= 2). Exit 1 otherwise.
ctl_parse() {
  local s=${1#"${1%%[![:space:]]*}"} tok rest word d
  [[ $s == /* ]] || return 1
  tok=${s%%[[:space:]]*}; rest=""
  [ "$tok" != "$s" ] && rest=${s#"$tok"} && rest=${rest#"${rest%%[![:space:]]*}"}
  word=$(tr '[:upper:]' '[:lower:]' <<<"${tok#/}")
  d=$(awk -v a="$word" -v b="syntropy" 'BEGIN{la=length(a);lb=length(b)
    for(j=0;j<=lb;j++)p[j]=j
    for(i=1;i<=la;i++){c[0]=i
      for(j=1;j<=lb;j++){x=(substr(a,i,1)==substr(b,j,1))?0:1
        m=p[j]+1; if(c[j-1]+1<m)m=c[j-1]+1; if(p[j-1]+x<m)m=p[j-1]+x; c[j]=m}
      for(j=0;j<=lb;j++)p[j]=c[j]}
    print p[lb]}')
  [ "$d" -le 2 ] || return 1
  local verb=${rest%%[[:space:]]*} args=""
  [ "$verb" != "$rest" ] && args=${rest#"$verb"} && args=${args#"${args%%[![:space:]]*}"}
  printf '%s\t%s\n' "$(tr '[:upper:]' '[:lower:]' <<<"$verb")" "$args"
}

# is_noise <body>: true for emoji-only, thanks/lgtm-style, or learned phrases.
is_noise() {
  local n
  n=$(tr '[:upper:]' '[:lower:]' <<<"$1" | tr -c '[:alnum:]+ \n' ' ' | tr -s ' \n' ' ' | sed 's/^ //;s/ $//')
  [ -z "$n" ] && return 0
  [[ $n =~ ^(lgtm|looks good|looks good to me|thanks|thank you|thx|ty|ok|okay|nice|great|ack|\+1|approved|done|sgtm)$ ]] && return 0
  [ -f "$SYN_HOME/phrases.txt" ] && grep -qixF -- "$n" "$SYN_HOME/phrases.txt" && return 0
  return 1
}

# is_flake: reads a CI log on stdin; true when it matches a known flake.
is_flake() {
  local log; log=$(cat)
  grep -Eqi "${SYN_FLAKES:-ECONNRESET|connection reset|i/o timeout|TLS handshake timeout|503 Service Unavailable|runner system failure|Could not resolve host|rate limit|no space left on device}" <<<"$log" && return 0
  [ -f "$SYN_HOME/flakes.txt" ] && grep -Eqif "$SYN_HOME/flakes.txt" <<<"$log"
}
