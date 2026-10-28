#!/usr/bin/env bash
# Skill update check and apply. Usage: syn-update.sh [--apply]
# Check (default): at most one GitHub call per 24h, cached in $SYN_HOME/update.json.
#   Prints ONE line if a newer release exists. Silent otherwise. Never fails the caller.
# --apply: download the latest release, verify its checksum, replace this skill folder.
# Disable the check: SYN_NO_UPDATE_CHECK=1.
. "$(dirname "$0")/lib.sh"
API=${SYN_RELEASE_API:-https://api.github.com/repos/andrewwormald/syntropy}
dir=$(cd "$(dirname "$0")/.." && pwd -P); cur=$(cat "$dir/VERSION" 2>/dev/null || echo v0.0.0)
cache=$SYN_HOME/update.json

# is_newer <current> <latest>: numeric major.minor.patch compare; unparsable = false.
is_newer() {
  awk -v a="${1#v}" -v b="${2#v}" 'BEGIN{
    if (a !~ /^[0-9]+\.[0-9]+\.[0-9]+/ || b !~ /^[0-9]+\.[0-9]+\.[0-9]+/) exit 1
    split(a,x,/[.+-]/); split(b,y,/[.+-]/)
    for(i=1;i<=3;i++){ if (y[i]+0!=x[i]+0) exit !(y[i]+0>x[i]+0) } exit 1 }'
}
fetch_release() { curl -fsSL -m 10 -H 'Accept: application/vnd.github+json' "$API/releases/latest"; }

if [ "${1:-}" != --apply ]; then
  [ -z "${SYN_NO_UPDATE_CHECK:-}" ] || exit 0
  command -v curl >/dev/null && command -v jq >/dev/null || exit 0
  now=$(date +%s); latest=""
  if [ -f "$cache" ] && [ $((now - $(jq -r '.checked_at // 0' "$cache" 2>/dev/null || echo 0))) -lt 86400 ]; then
    latest=$(jq -r '.latest // ""' "$cache" 2>/dev/null || true)
  else
    rel=$(fetch_release 2>/dev/null) || exit 0                      # offline or API error: stay silent
    latest=$(jq -r '.tag_name // ""' <<<"$rel" 2>/dev/null) || exit 0
    [ -n "$latest" ] || exit 0
    mkdir -p "$SYN_HOME" && jq -n --arg l "$latest" --argjson t "$now" '{latest:$l,checked_at:$t}' > "$cache" 2>/dev/null || true
  fi
  is_newer "$cur" "$latest" && echo "A newer syntropy skill is available: $latest (you have $cur). Update: $dir/scripts/syn-update.sh --apply" || true
  exit 0
fi

need curl jq tar shasum
logical=$(cd "$(dirname "$0")/.." && pwd)
[ ! -L "$logical" ] || die "$logical is a symlink to a checkout. Update it with git pull instead."
rel=$(fetch_release) || die "cannot reach GitHub ($API)"
latest=$(jq -r .tag_name <<<"$rel")
is_newer "$cur" "$latest" || { echo "Already up to date ($cur)."; exit 0; }
tgz=$(jq -r '.assets[]|select(.name=="syntropy-skill.tar.gz")|.browser_download_url' <<<"$rel")
sum=$(jq -r '.assets[]|select(.name=="syntropy-skill.tar.gz.sha256")|.browser_download_url' <<<"$rel")
[ -n "$tgz" ] && [ -n "$sum" ] || die "release $latest has no skill assets"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
curl -fsSL -m 120 -o "$tmp/s.tgz" "$tgz"; curl -fsSL -m 30 -o "$tmp/s.sha" "$sum"
[ "$(awk '{print $1}' "$tmp/s.sha")" = "$(shasum -a 256 "$tmp/s.tgz" | awk '{print $1}')" ] || die "checksum mismatch. Nothing changed."
mkdir "$tmp/x" && tar -xzf "$tmp/s.tgz" -C "$tmp/x"
[ -f "$tmp/x/syntropy/SKILL.md" ] || die "archive has no syntropy/SKILL.md. Nothing changed."
echo "$latest" > "$tmp/x/syntropy/VERSION"
mv "$logical" "$logical.old.$$" && mv "$tmp/x/syntropy" "$logical" && rm -rf "$logical.old.$$"
rm -f "$cache"; echo "Updated syntropy skill: $cur -> $latest"
