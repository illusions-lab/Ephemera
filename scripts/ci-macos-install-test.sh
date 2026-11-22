#!/bin/bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
box=$(mktemp -d /tmp/ephe-macos-ci.XXXXXX)
trap 'rm -rf "$box"' EXIT
umask 077
export HOME="$box/home"
root="$HOME/Library/Application Support/Ephemera"
mkdir -p "$root/tools/tart.app/Contents/MacOS" "$box/bin" "$box/bundle"
export PATH="$box/bin:$PATH"
cat > "$root/tools/tart.app/Contents/MacOS/tart" <<'SH'
#!/bin/bash
echo 2.42.0
SH
cat > "$box/bin/launchctl" <<'SH'
#!/bin/bash
root="$HOME/Library/Application Support/Ephemera"
case "$1:$2" in
  print:gui/*/com.illusions-lab.ephemera) test -f "$root/loaded" ;;
  print:gui/*/com.illusions-lab.ephemera.dev) test -f "$root/dev-loaded" ;;
  print:*) exit 0 ;;
  bootout:*) rm -f "$root/loaded" ;;
  bootstrap:*) touch "$root/loaded" ;;
  *) exit 1 ;;
esac
SH
cat > "$box/bundle/ephemera" <<'SH'
#!/bin/bash
set -e
root="$HOME/Library/Application Support/Ephemera"
case "$1" in
  version) echo 'v1.0.0 fixture' ;;
  setup)
    if test ! -f "$root/config.yaml"; then printf 'preserved\n' > "$root/config.yaml"; fi
    mkdir -p "$HOME/Library/LaunchAgents"
    printf 'candidate\n' > "$HOME/Library/LaunchAgents/com.illusions-lab.ephemera.plist"
    printf 'start\n' >> "$root/starts"
    touch "$root/loaded"
    ;;
  status) printf '{"daemon":{"github":"connected"}}\n' ;;
  *) exit 1 ;;
esac
SH
cat > "$box/bundle/ephemerad" <<'SH'
#!/bin/bash
echo 'v1.0.0 fixture'
SH
cp "$repo"/packaging/macos/{activate-macos,server-config}.sh "$box/bundle"
chmod +x "$box/bin/launchctl" "$box/bundle/"ephemer* "$root/tools/tart.app/Contents/MacOS/tart"
touch "$root/dev-loaded"
if "$repo/scripts/install-macos.sh" --bundle "$box/bundle"; then exit 1; fi
[[ ! -L "$root/current" ]]
rm "$root/dev-loaded"
cp "$box/bundle/ephemera" "$box/original-ephemera"
"$repo/scripts/install-macos.sh" --bundle "$box/bundle"
previous=$(readlink "$root/current")
[[ "$("$HOME/.local/bin/ephe" version)" == 'v1.0.0 fixture' ]]
"$repo/scripts/install-macos.sh" --bundle "$box/bundle"
[[ "$(wc -l < "$root/starts" | tr -d ' ')" == 1 ]]
# Refuse a mismatched bundle before touching the active installation.
sed -i '' s/1.0.0/2.0.0/ "$box/bundle/ephemerad"
if "$repo/scripts/install-macos.sh" --bundle "$box/bundle"; then exit 1; fi
[[ "$(readlink "$root/current")" == "$previous" ]]
# shellcheck disable=SC2016
sed -i '' -e s/1.0.0/2.0.0/ -e '/touch "\$root\/loaded"/a\
    exit 1\
' "$box/bundle/ephemera"
if "$repo/scripts/install-macos.sh" --bundle "$box/bundle"; then exit 1; fi
[[ "$(readlink "$root/current")" == "$previous" && -f "$root/loaded" ]]
[[ "$(cat "$root/config.yaml")" == preserved ]]
[[ ! -e "$root/.install-transaction" ]]

# Model an interruption after switching the active symlink.
plist="$HOME/Library/LaunchAgents/com.illusions-lab.ephemera.plist"
interrupted=$(mktemp -d "$root/.install.XXXXXX")
printf '%s\n' "$previous" > "$interrupted/previous"
cp "$plist" "$interrupted/previous.plist"
touch "$interrupted/was-running"
printf '%s\n' "$interrupted" > "$root/.install-transaction"
ln -sfh "$root/releases/incomplete" "$root/current"
cp "$box/original-ephemera" "$box/bundle/ephemera"
sed -i '' s/2.0.0/1.0.0/ "$box/bundle/ephemerad"
"$repo/scripts/install-macos.sh" --bundle "$box/bundle"
[[ "$(readlink "$root/current")" == "$previous" && -f "$root/loaded" ]]
[[ ! -e "$interrupted" && ! -e "$root/.install-transaction" ]]

# Generate the real plist using a home directory that includes spaces.
"$repo/packaging/macos/activate-macos.sh"
/usr/bin/plutil -lint "$plist"
[[ "$(/usr/bin/plutil -extract ProgramArguments.0 raw "$plist")" == "$root/current/ephemerad" ]]
[[ "$(/usr/bin/plutil -extract ProgramArguments.1 raw "$plist")" == --create ]]
