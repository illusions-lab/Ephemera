#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 /path/to/Apple.ipsw /path/to/xcode-archives" >&2
  exit 2
fi

commit=6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab
toolset=$(curl -fsSL "https://raw.githubusercontent.com/actions/runner-images/$commit/images/macos/toolsets/toolset-26.json")
packages=$(jq -er '.xcode.arm64.versions[] | [.version, .sha256] | @tsv' <<< "$toolset")

failed=0
if [[ ! -s "$1" || "$1" != *.ipsw ]]; then
  printf 'Missing Apple restore image: %s\n' "$1" >&2
  failed=1
fi

while IFS=$'\t' read -r version expected; do
  archive="$2/Xcode-$version.xip"
  if [[ ! -s "$archive" ]]; then
    printf 'Missing archive: %s\n' "$archive" >&2
    failed=1
    continue
  fi
  actual=$(shasum -a 256 < "$archive")
  if [[ "${actual%% *}" != "$expected" ]]; then
    printf 'SHA-256 mismatch: %s\n' "$archive" >&2
    failed=1
  else
    printf 'Verified: %s\n' "$archive"
  fi
done <<< "$packages"

exit "$failed"
