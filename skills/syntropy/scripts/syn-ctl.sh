#!/usr/bin/env bash
# Run an author control command with no LLM. Usage:
#   syn-ctl.sh <run> <verb> [args...]
# Verbs: pause resume skip retry prompt status stop. Unknown verb = prompt (ADR-0042).
# Last output line is a directive: NEXT=<wait|pick-unit|rework|stop>.
. "$(dirname "$0")/lib.sh"; need jq
load_run "${1:?run}"; verb=${2:?verb}; shift 2; args="$*"; d=$(dirname "$0")
say() { printf '%s' "$1" | "$d/syn-say.sh" "$RUN" >/dev/null; }
case $verb in
  pause)  st_upd "$RUN" '.paused=true'; say "Paused. Reply \`/syntropy resume\` to continue."; echo "NEXT=stop";;
  resume) st_upd "$RUN" '.paused=false'; say "Resumed."; echo "NEXT=wait";;
  stop)   st_upd "$RUN" '.status="stopped"'; say "Stopped. The MR stays open."; echo "NEXT=stop";;
  status) line=$(jq -r '"Run \(.id): \(.done|length) merged, \(.blacklisted|length) skipped, unit \(.unit), paused=\(.paused)."' "$(st_file "$RUN")")
          say "$line"; echo "$line"; echo "NEXT=wait";;
  retry)  ref=$(pf mr_state "$MR" | jq -r .ci_ref); [ -n "$ref" ] && pf retry_ci "$MR" "$ref"
          st_upd "$RUN" '.ci_handled=[]'; say "Retrying CI."; echo "NEXT=wait";;
  skip)   if [ "$(st_get "$RUN" .mode)" = adopt ]; then echo "skip does not apply to an adopted MR. Use stop."; echo "NEXT=wait"; exit 0; fi
          say "Skipping this unit${args:+: $args}."; pf close "$MR"; "$d/syn-next.sh" "$RUN" skip; echo "NEXT=pick-unit";;
  prompt|*) [ "$verb" = prompt ] || args="$verb $args"
          st_upd "$RUN" '.prompts+=[$p]' --arg p "$args"
          say "Got it. Applying your guidance."; echo "GUIDANCE: $args"; echo "NEXT=rework";;
esac
