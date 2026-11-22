#!/bin/bash
set -euo pipefail
assets=$1
installer=$2
expected=${3:-}
archive=ephemera_darwin_arm64.tar.gz
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

digest=$(awk -v archive="$archive" '$2 == archive || $2 == "*" archive {print $1}' "$assets/checksums.txt")
[[ "$digest" =~ ^[[:xdigit:]]{64}$ ]]
(cd "$assets" && printf '%s  %s\n' "$digest" "$archive" | /usr/bin/shasum -a 256 -c -)
tar -xzf "$assets/$archive" -C "$work"
cmp "$installer" "$work/install-macos.sh"
for script in install-macos.sh server-config.sh; do
  test -s "$work/$script"
  bash -n "$work/$script"
done
for binary in ephemera ephemerad; do
  file -b "$work/$binary" | grep -E '^Mach-O 64-bit executable arm64'
done
actual=$("$work/ephemera" version)
[[ -n "$actual" && "$actual" == "$("$work/ephemerad" --version)" ]]
[[ -z "$expected" || "$actual" == "$expected" ]]
"$work/ephemera" tart-manifest > "$work/cli-manifest"
"$work/ephemerad" --tart-manifest > "$work/daemon-manifest"
cmp "$work/tart-manifest.tsv" "$work/cli-manifest"
cmp "$work/tart-manifest.tsv" "$work/daemon-manifest"
