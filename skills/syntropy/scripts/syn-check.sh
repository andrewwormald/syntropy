#!/usr/bin/env bash
# Pure config check. Usage: syn-check.sh <repo>. Exit 0 = OK, 1 = MISSING.
# Prints the title convention and spec tool so Claude never reads YAML itself.
. "$(dirname "$0")/lib.sh"; need jq git
repo=${1:-.}; [ -d "$repo" ] || die "repo must be a directory path (use . for the current clone), got: $repo"
f="$repo/.syntropy.yml"
val() { [ -f "$f" ] || return 0; sed -n "s/^$1:[[:space:]]*//p" "$f" | head -1 | sed 's/^["'\'']//;s/["'\'']$//'; }
tc=$(val title_convention); st=$(val spec_tool)
[ -z "$st" ] && [ -f "$SYN_HOME/config" ] && st=$(sed -n 's/^spec_tool=//p' "$SYN_HOME/config")
echo "Spec tool: ${st:-(none set — default spec flow)}"
command -v glab >/dev/null && echo "Provider CLI: glab"; command -v gh >/dev/null && echo "Provider CLI: gh"
if [ -z "$tc" ]; then echo "MISSING: title_convention in $f"; exit 1; fi
[ "$tc" = blank ] && tc="(none — user declined)"
echo "Title convention: $tc"; echo "OK: $repo has every field configured"
