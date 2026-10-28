#!/usr/bin/env bash
# Install or update the syntropy skill from GitHub.
#   curl -fsSL https://raw.githubusercontent.com/andrewwormald/syntropy/main/install.sh | bash
# Options (after `bash -s --` when piped):
#   --dir <skills-dir>   where agent skills live (default ~/.claude/skills)
#   --ref <tag|branch>   install this ref instead of the latest release
# Needs: curl, tar, jq (latest-release mode), shasum.
set -euo pipefail
REPO=${SYN_REPO:-andrewwormald/syntropy}; API=${SYN_RELEASE_API:-https://api.github.com/repos/$REPO}
dir="$HOME/.claude/skills"; ref=""
while [ $# -gt 0 ]; do case $1 in --dir) dir=$2; shift;; --ref) ref=$2; shift;; *) echo "unknown option: $1" >&2; exit 1;; esac; shift; done
for c in curl tar shasum; do command -v "$c" >/dev/null || { echo "missing tool: $c" >&2; exit 1; }; done
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT; mkdir -p "$dir" "$tmp/x"
if [ -n "$ref" ]; then
  curl -fsSL -o "$tmp/s.tgz" "https://codeload.github.com/$REPO/tar.gz/$ref"
  tar -xzf "$tmp/s.tgz" -C "$tmp/x"
  src=$(ls -d "$tmp"/x/*/skills/syntropy 2>/dev/null | head -1) || true
  [ -n "$src" ] || { echo "ref $ref has no skills/syntropy" >&2; exit 1; }
  mkdir "$tmp/y" && mv "$src" "$tmp/y/syntropy" && rm -rf "$tmp/x" && mv "$tmp/y" "$tmp/x"; v=$ref
else
  command -v jq >/dev/null || { echo "missing tool: jq" >&2; exit 1; }
  rel=$(curl -fsSL "$API/releases/latest"); v=$(jq -r .tag_name <<<"$rel")
  u=$(jq -r '.assets[]|select(.name=="syntropy-skill.tar.gz")|.browser_download_url' <<<"$rel")
  c=$(jq -r '.assets[]|select(.name=="syntropy-skill.tar.gz.sha256")|.browser_download_url' <<<"$rel")
  [ -n "$u" ] && [ -n "$c" ] || { echo "release $v has no skill assets" >&2; exit 1; }
  curl -fsSL -o "$tmp/s.tgz" "$u"; curl -fsSL -o "$tmp/s.sha" "$c"
  [ "$(awk '{print $1}' "$tmp/s.sha")" = "$(shasum -a 256 "$tmp/s.tgz" | awk '{print $1}')" ] || { echo "checksum mismatch. Nothing changed." >&2; exit 1; }
  tar -xzf "$tmp/s.tgz" -C "$tmp/x"
fi
[ -f "$tmp/x/syntropy/SKILL.md" ] || { echo "archive has no syntropy/SKILL.md. Nothing changed." >&2; exit 1; }
echo "$v" > "$tmp/x/syntropy/VERSION"
[ -e "$dir/syntropy" ] && mv "$dir/syntropy" "$dir/syntropy.old.$$"
mv "$tmp/x/syntropy" "$dir/syntropy"; rm -rf "$dir/syntropy.old.$$"
echo "Installed syntropy $v to $dir/syntropy"
