#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 1 || ! "$1" =~ ^xcode-trial-[a-z0-9-]+$ || -z "${TART_HOME:-}" ]]; then
  echo "Usage: TART_HOME=/absolute/trial/home $0 xcode-trial-NAME" >&2
  exit 2
fi

tart=${TART:-tart}
ephe=${EPHE:-ephe}
guest=$1
trial=$(cd "$TART_HOME" && pwd -P)
export TART_HOME="$trial"
state="$HOME/Library/Application Support/Ephemera/state/tart"
mkdir -p "$state/vms"
[[ ! -L "$state" && ! -L "$state/vms" && $(stat -f '%u:%Lp' "$state") == "$EUID:700" ]] ||
  { echo 'Ephemera Tart home must be private and owned by this account.' >&2; exit 1; }
state=$(cd "$state" && pwd -P)
[[ "$trial" != "$state" ]] || { echo 'Use a separate trial Tart home.' >&2; exit 1; }
source="$trial/vms/$guest"
[[ -f "$source/config.json" && ! -L "$source" &&
   $(stat -f '%d' "$source") == "$(stat -f '%d' "$state/vms")" ]] ||
  { echo 'The trial VM must exist on the same volume as the production Tart cache.' >&2; exit 1; }

host=$("$ephe" image check-host-xcode --json)
version=$(jq -er '.version' <<< "$host")
build=$(jq -er '.build' <<< "$host")
os_build=$("$tart" exec "$guest" /usr/bin/sw_vers -buildVersion)
"$tart" exec -i "$guest" /usr/bin/sudo -n /bin/bash -s -- "$version" "$build" >&2 <<'GUEST'
set -euo pipefail
app=/Applications/Xcode.app
/usr/bin/codesign --verify --strict -R='anchor apple generic and identifier "com.apple.dt.Xcode" and certificate leaf[subject.O] = "Apple Inc."' "$app"
export DEVELOPER_DIR="$app/Contents/Developer"
tool="$DEVELOPER_DIR/usr/bin/xcodebuild"
"$tool" -checkFirstLaunchStatus || { echo 'Complete Apple license review and Xcode first launch inside this guest.' >&2; exit 1; }
actual=$("$tool" -version)
grep -Fxq "Xcode $1" <<< "$actual"
grep -Fxq "Build version $2" <<< "$actual"
tmp=$(mktemp -d -t ephe-image-smoke)
trap 'rm -rf "$tmp"' EXIT
printf 'print("Ephemera macOS compile check")\n' > "$tmp/main.swift"
/usr/bin/xcrun --sdk macosx swiftc "$tmp/main.swift" -o "$tmp/smoke"
"$tmp/smoke"
sdk=$(/usr/bin/xcrun --sdk iphoneos --show-sdk-path)
/usr/bin/xcrun --sdk iphoneos swiftc -target arm64-apple-ios18.0 -sdk "$sdk" -emit-object "$tmp/main.swift" -o "$tmp/ios.o"
test -s "$tmp/ios.o"
/bin/launchctl disable system/com.openssh.sshd
GUEST
"$tart" stop "$guest" >&2
"$tart" list --source local --format json | jq -e --arg name "$guest" 'any(.[]; .Name == $name and .State == "stopped")' >/dev/null
name="apple-macos-$(uuidgen | tr '[:upper:]' '[:lower:]')"
destination="$state/vms/$name"
receipt="$state/$name.apple-build.json"
[[ ! -e "$destination" && ! -e "$receipt" ]]
draft=$(mktemp "$state/.apple-build.XXXXXXXX")
trap 'if [[ -e "$draft" ]]; then unlink "$draft"; fi' EXIT
jq -n --arg os "$os_build" --arg version "$version" --arg build "$build" \
  '{source:"apple-ipsw+host-xcode",os_build:$os,xcode_version:$version,xcode_build:$build}' > "$draft"
mv "$source" "$destination"
if ! mv "$draft" "$receipt"; then
  mv "$destination" "$source"
  exit 1
fi
printf 'local:%s\n' "$name"
