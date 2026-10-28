#!/usr/bin/env bash
# One-line status per run. Usage: syn-status.sh [run]
. "$(dirname "$0")/lib.sh"; need jq
for f in $( [ -n "${1:-}" ] && echo "$(st_file "$1")" || ls "$SYN_HOME"/runs/*/state.json 2>/dev/null ); do
  jq -r '"\(.id) \(.mode // "sweep") \(.status) paused=\(.paused) merged=\(.done|length) skipped=\(.blacklisted|length) mr=\(.mr.id // "-") unit=\(.unit) goal=\(.goal[:60])"' "$f"
done
