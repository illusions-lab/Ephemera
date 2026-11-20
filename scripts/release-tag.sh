#!/usr/bin/env bash
set -euo pipefail
# Called only by the serialized CI release job after fetching all remote tags.
git fetch --force --tags origin
commit=$(git rev-parse HEAD)
existing=$(git tag --points-at "$commit" | grep -E '^v0\.1\.[0-9]+$' | sort -V | tail -n1 || true)
if [[ -n "$existing" ]]; then
  tag=$existing
else
  last=$(git tag -l 'v0.1.*' | grep -E '^v0\.1\.[0-9]+$' | sort -V | tail -n1 || true)
  if [[ -z "$last" ]]; then tag=v0.1.0; else tag="v0.1.$(( ${last##*.} + 1 ))"; fi
  git tag "$tag" "$commit"
  git push origin "refs/tags/$tag"
fi
printf 'tag=%s\n' "$tag" >> "$GITHUB_OUTPUT"
