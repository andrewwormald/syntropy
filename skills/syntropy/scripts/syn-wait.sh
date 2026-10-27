#!/usr/bin/env bash
# Token-free wait. Polls the MR/PR until something needs the LLM, then prints
# ONE JSON line and exits 0. Run it in the background; you wake on exit.
# Usage: syn-wait.sh <run> [max-seconds=14400]
# Events: merged | closed | comments | ci_failed | conflict | pause | timeout
. "$(dirname "$0")/lib.sh"; need jq
load_run "${1:?run}"; MAXS=${2:-14400}; POLL=${SYN_POLL:-30}
[ -n "$MR" ] || die "run $RUN has no open MR; run syn-open.sh first"
author=$(st_get "$RUN" .author); errs=0; t0=$SECONDS
emit() { jq -nc --arg e "$1" --arg r "${2:-}" '{event:$e}+(if $r!="" then {reason:$r} else {} end)'; exit 0; }
emit_json() { jq -nc --arg e "$1" --argjson d "$2" '{event:$e}+$d'; exit 0; }

while :; do
  if ! s=$(pf mr_state "$MR" 2>/dev/null); then
    errs=$((errs+1))
    [ "$errs" -ge "${SYN_MAXERR:-8}" ] && emit pause "provider API failed $errs times (auth expired?)"
    sleep $((POLL*errs > 600 ? 600 : POLL*errs)); continue
  fi
  errs=0
  state=$(jq -r .state <<<"$s")
  if [ "$state" = merged ]; then emit merged; fi
  if [ "$state" = closed ]; then
    sleep 5; [ "$(pf mr_state "$MR" | jq -r .state)" = closed ] && emit closed   # re-verify (ADR-0096)
    continue
  fi

  # --- new comments (before CI, so a human is never hidden behind a red build)
  notes=$(pf notes "$MR" 2>/dev/null || echo '[]')
  seen=$(st_get "$RUN" '.seen|tostring')
  new=$(jq -c --argjson seen "$seen" '[.[]|select(.key as $k|$seen|index($k)|not)]' <<<"$notes")
  items='[]'
  if [ "$(jq length <<<"$new")" -gt 0 ]; then
    st_upd "$RUN" '.seen=((.seen+$k)|.[-500:])' --argjson k "$(jq -c '[.[].key]' <<<"$new")"
    while IFS= read -r n; do
      body=$(jq -r .body <<<"$n"); who=$(jq -r .author <<<"$n"); thread=$(jq -r .thread <<<"$n")
      [[ $body == *"$MARK"* ]] && continue                                   # our own echo (ADR-0035)
      [ "$(jq -r .bot <<<"$n")" = true ] && continue
      [[ $who =~ (\[bot\]$|_bot$|^project_[0-9]+_bot) ]] && continue
      if [ "$who" = "$author" ] && cp=$(ctl_parse "$body"); then
        items=$(jq -c --arg v "${cp%%$'\t'*}" --arg a "${cp#*$'\t'}" --arg t "$thread" \
          '.+[{type:"control",verb:$v,args:$a,thread:$t}]' <<<"$items"); continue
      fi
      is_noise "$body" && continue
      if [ "$who" = "$author" ]; then
        items=$(jq -c --arg w "$who" --arg b "${body:0:1500}" --arg t "$thread" \
          '.+[{type:"note",from:"author",author:$w,body:$b,thread:$t}]' <<<"$items")
      else
        # Untrusted: screen BEFORE the body can reach the session (ADR-0106).
        IFS=$'\t' read -r verdict why < <(printf '%s' "$body" | "$(dirname "$0")/syn-screen.sh")
        if [ "$verdict" = safe ]; then
          items=$(jq -c --arg w "$who" --arg b "${body:0:1500}" --arg t "$thread" \
            '.+[{type:"note",from:"reviewer",author:$w,body:$b,thread:$t}]' <<<"$items")
        else
          items=$(jq -c --arg w "$who" --arg v "$verdict" --arg r "$why" --arg t "$thread" \
            '.+[{type:"blocked",author:$w,verdict:$v,reason:$r,thread:$t}]' <<<"$items")
        fi
      fi
    done < <(jq -c '.[]' <<<"$new")
    [ "$(jq length <<<"$items")" -gt 0 ] && emit_json comments "$(jq -nc --argjson i "$items" '{items:$i}')"
  fi

  # --- merge conflict
  if [ "$(jq -r .conflict <<<"$s")" = true ]; then
    [ "$(st_get "$RUN" .conflict_handled)" = true ] || { st_upd "$RUN" '.conflict_handled=true'; emit conflict; }
  else
    [ "$(st_get "$RUN" .conflict_handled)" = true ] && st_upd "$RUN" '.conflict_handled=false'
  fi

  # --- CI
  if [ "$(jq -r .ci <<<"$s")" = failed ]; then
    ref=$(jq -r .ci_ref <<<"$s")
    if [ "$(st_get "$RUN" ".ci_handled|index(\"$ref\")|tostring")" = null ]; then
      log=$(pf ci_log "$MR" "$ref" 2>/dev/null | tail -n 80 || true)
      retries=$(st_get "$RUN" .retries)
      if is_flake <<<"$log" && [ "${retries:-0}" -lt "${SYN_MAX_RETRIES:-2}" ]; then
        pf retry_ci "$MR" "$ref" && st_upd "$RUN" '.retries+=1'     # known flake: no LLM
      else
        st_upd "$RUN" '.ci_handled+=[$r]' --arg r "$ref"
        emit_json ci_failed "$(jq -nc --arg l "$(tail -n 40 <<<"$log" | head -c 3000)" --arg r "$ref" '{ref:$r,log:$l}')"
      fi
    fi
  fi

  [ $((SECONDS-t0)) -ge "$MAXS" ] && emit timeout
  sleep "$POLL"
done
