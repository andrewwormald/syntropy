#!/usr/bin/env bash
# Post a comment (stdin) on the MR. Optionally resolve a thread.
# Usage: syn-say.sh <run> [--resolve <thread-id>]
. "$(dirname "$0")/lib.sh"; need jq
load_run "${1:?run}"; shift || true
body=$(cat)
# Cap prose at 90 words (aim for 60). Fenced code passes whole and is not counted.
cap=${SYN_MAX_WORDS:-90}
body=$(awk -v cap="$cap" 'BEGIN{n=0;cut=0}
  /^```/{f=!f;print;next}
  f{print;next}
  {out="";for(i=1;i<=NF;i++){ if(n>=cap){cut=1;break} out=out (out==""?"":" ") $i;n++ } if(out!="")print out; else if(NF==0)print}
  END{if(cut)print "[...cut: comment over " cap " words]"}' <<<"$body")
printf '%s\n\n%s\n' "$body" "$MARK" | pf comment "$MR"
if [ "${1:-}" = --resolve ]; then
  pf resolve "$MR" "${2:-}" || echo "WARN: could not resolve thread ${2:-}" >&2   # surface, never swallow (ADR-0115)
fi
echo posted
