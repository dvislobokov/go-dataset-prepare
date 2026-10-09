#!/usr/bin/env bash
# Shallow-clone a few small, permissively-licensed repos into data/repos/ for the pilot and record the resolved SHA.
# Safety: https only, pinned to a single shallow commit, no submodules, no hooks, GIT_TERMINAL_PROMPT=0, no code is
# built or run. Analyzed repositories are untrusted input; this script lives outside them and only reads files.
set -euo pipefail
export GIT_TERMINAL_PROMPT=0
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/data/repos"
mkdir -p "$DEST"

# full_name  branch
REPOS=(
  "jellydator/ttlcache v3"
  "jarcoal/httpmock v1"
)

for entry in "${REPOS[@]}"; do
  read -r fn branch <<<"$entry"
  name="${fn#*/}"
  dir="$DEST/$name"
  if [ -d "$dir/.git" ]; then
    echo "exists: $dir" >&2
  else
    git -c advice.detachedHead=false clone --depth 1 --branch "$branch" --no-tags --recurse-submodules=no \
      "https://github.com/$fn.git" "$dir"
  fi
  sha="$(git -C "$dir" rev-parse HEAD)"
  echo "$fn $branch $sha"
done
