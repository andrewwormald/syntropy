#!/usr/bin/env bash
# Post a comment (stdin) on the MR. Optionally resolve a thread.
# Usage: syn-say.sh <run> [--resolve <thread-id>]
. "$(dirname "$0")/lib.sh"; need jq
load_run "${1:?run}"; shift || true
body=$(cat)
printf '%s\n\n%s\n' "$body" "$MARK" | pf comment "$MR"
if [ "${1:-}" = --resolve ]; then
  pf resolve "$MR" "${2:-}" || echo "WARN: could not resolve thread ${2:-}" >&2   # surface, never swallow (ADR-0115)
fi
echo posted
