#!/bin/bash
set -euo pipefail
# shellcheck source=packaging/macos/server-config.sh
source "$(dirname "$0")/../packaging/macos/server-config.sh"
box=$(mktemp -d)
trap 'rm -rf "$box"' EXIT
export HOME="$box/home"
root="$HOME/Library/Application Support/Ephemera"
mkdir -p "$root"
printf '10\n' > "$box/sleep"
printf '0\n' > "$box/autorestart"
power_read() {
  printf 'Battery Power:\n sleep 7\nAC Power:\n sleep %s\n autorestart %s\n' "$(cat "$box/sleep")" "$(cat "$box/autorestart")"
}
supported=true
power_capabilities() { if "$supported"; then printf 'Capabilities for AC Power:\n sleep\n autorestart\n'; else printf ' sleep\n'; fi; }
power_write() {
  printf '%s %s\n' "$@" >> "$box/writes"
  if [[ -f "$box/fail" && "$1:$2" == autorestart:1 ]]; then return 1; fi
  printf '%s\n' "$2" > "$box/$1"
}
preview=$(server_settings --preview)
[[ "$preview" == *'AC sleep: 10'* ]]
[[ ! -e "$box/writes" ]]
supported=false
server_settings --apply
[[ "$(cat "$box/sleep")" == 0 && "$(cat "$box/autorestart")" == 0 ]]
server_settings --restore
supported=true
touch "$box/fail"
if server_settings --apply; then exit 1; fi
[[ "$(cat "$box/sleep")" == 10 && ! -e "$root/power-backup" ]]
rm "$box/fail"
server_settings --apply
cp "$root/power-backup" "$box/original"
server_settings --apply
cmp "$root/power-backup" "$box/original"
printf '17\n' > "$box/sleep"
printf 'server\n' > "$root/server-profile"
server_settings --restore
[[ "$(cat "$box/sleep")" == 17 && "$(cat "$box/autorestart")" == 0 && ! -e "$root/power-backup" ]]
[[ "$(cat "$root/server-profile")" == personal ]]
