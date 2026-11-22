#!/bin/bash
set -euo pipefail
root="$HOME/Library/Application Support/Ephemera"
label=com.illusions-lab.ephemera
domain="gui/$(id -u)"
plist="$HOME/Library/LaunchAgents/$label.plist"
mkdir -p "$HOME/Library/LaunchAgents" "$root/logs"
umask 077
tmp=$(mktemp "$HOME/Library/LaunchAgents/.ephemera.XXXXXX")
trap 'rm -f "$tmp"' EXIT
/usr/bin/plutil -create xml1 "$tmp"
/usr/bin/plutil -insert Label -string "$label" "$tmp"
/usr/bin/plutil -insert ProgramArguments -array "$tmp"
/usr/bin/plutil -insert ProgramArguments.0 -string "$root/current/ephemerad" "$tmp"
/usr/bin/plutil -insert ProgramArguments.1 -string "--create" "$tmp"
/usr/bin/plutil -insert KeepAlive -bool YES "$tmp"
/usr/bin/plutil -insert ThrottleInterval -integer 10 "$tmp"
/usr/bin/plutil -insert ExitTimeOut -integer 60 "$tmp"
/usr/bin/plutil -insert Umask -integer 63 "$tmp"
/usr/bin/plutil -insert EnvironmentVariables -dictionary "$tmp"
/usr/bin/plutil -insert EnvironmentVariables.PATH -string "$root/tools/tart.app/Contents/MacOS:/usr/bin:/bin:/usr/sbin:/sbin" "$tmp"
for stream in StandardOutPath StandardErrorPath; do
  /usr/bin/plutil -insert "$stream" -string "$root/logs/daemon.log" "$tmp"
done
/usr/bin/plutil -lint "$tmp" >/dev/null
if launchctl print "$domain/$label" >/dev/null 2>&1; then
  launchctl bootout "$domain/$label"
fi
mv -f "$tmp" "$plist"
launchctl bootstrap "$domain" "$plist"
