#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 ]]; then
  echo "Usage: $0 BUILD_VM XCODE_VERSION XCODE_BUILD [XCODE_APP_PATH]" >&2
  exit 2
fi

vm=$1
version=$2
build=$3
app=${4:-auto}
tart=${TART:-tart}
ephe=${EPHE:-ephe}

[[ "$vm" =~ ^xcode-trial-[a-z0-9-]+$ && -n "${TART_HOME:-}" ]] || {
  echo 'Use a disposable xcode-trial-* build VM in a separate TART_HOME.' >&2
  exit 1
}
[[ "$TART_HOME" == /* && -d "$TART_HOME" ]] || {
  echo 'TART_HOME must be an existing absolute path.' >&2
  exit 1
}
trial_home=$(cd "$TART_HOME" && pwd -P)
export TART_HOME="$trial_home"
ephemera_state="$HOME/Library/Application Support/Ephemera/state/tart"
if [[ -d "$ephemera_state" && "$trial_home" == "$(cd "$ephemera_state" && pwd -P)" ]]; then
  echo 'Use a separate Tart home for image trials.' >&2
  exit 1
fi

metadata=$("$ephe" image check-host-xcode --path "$app" --version "$version" --build "$build" --json)
app=$(jq -er '.path' <<< "$metadata")
name=${app##*/}
parent=${app%/*}

# Reject an incomplete guest before copying a large archive.
"$tart" exec "$vm" /usr/bin/sudo -n /usr/bin/true
"$tart" exec "$vm" /usr/bin/test ! -e /Applications/Xcode.app

guest_copy=$(cat <<'GUEST'
set -euo pipefail
name=$1
expected_version=$2
expected_build=$3
destination=/Applications/Xcode.app
lock=/Applications/.ephe-xcode-copy.lock
mkdir "$lock"
trap 'rmdir "$lock"' EXIT
[[ ! -e "$destination" && ! -L "$destination" ]]
stage=$(mktemp -d /Applications/.ephe-xcode.XXXXXXXX)
trap 'rm -rf "$stage"; rmdir "$lock"' EXIT
/usr/bin/tar -xf - -C "$stage"
app="$stage/$name"
/usr/bin/codesign --verify --deep --strict -R='anchor apple generic and identifier "com.apple.dt.Xcode" and certificate leaf[subject.O] = "Apple Inc."' "$app"
export DEVELOPER_DIR="$app/Contents/Developer"
tool="$DEVELOPER_DIR/usr/bin/xcodebuild"
actual=$("$tool" -version)
grep -Fxq "Xcode $expected_version" <<< "$actual"
grep -Fxq "Build version $expected_build" <<< "$actual"
"$tool" -runFirstLaunch
for sdk in macosx iphoneos; do
  path=$(/usr/bin/xcrun --sdk "$sdk" --show-sdk-path)
  test -d "$path"
done
mv "$app" "$destination"
GUEST
)

/usr/bin/tar -cf - -C "$parent" "$name" |
  "$tart" exec -i "$vm" /usr/bin/sudo -n /bin/bash -c "$guest_copy" copy-host-xcode "$name" "$version" "$build"
printf 'Copied and verified Xcode %s (%s) inside %s.\n' "$version" "$build" "$vm"
