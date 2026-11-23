#!/bin/bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"
box=$(mktemp -d /tmp/ephe-ci.XXXXXX)
status_pid=
cleanup() {
  if [[ -n "$status_pid" ]]; then
    kill "$status_pid" 2>/dev/null || true
    wait "$status_pid" || true
  fi
  rm -rf "$box"
}
trap cleanup EXIT
umask 077

# Compile the real CLI against two different embedded Tart manifests.
manifest="$repo/internal/macos/tart-manifest.tsv"
mkdir -p "$box"/{bundle-a,bundle-b,bin}
cp "$manifest" "$box/bundle-a/tart-manifest.tsv"
printf '%s\t%s\t%064d\n' 99.0.0 https://example.invalid/tart.tar.gz 2 > "$box/bundle-b/tart-manifest.tsv"
python3 - "$manifest" "$box/bundle-b/tart-manifest.tsv" > "$box/overlay.json" <<'PY'
import json
import sys
json.dump({"Replace": {sys.argv[1]: sys.argv[2]}}, sys.stdout)
PY
go test -c -o "$box/bundle-a/ephemera" ./internal/cli
go test -c -ldflags "-X github.com/illusions-lab/Ephemera/internal/version.Version=v99.0.0" \
  -overlay "$box/overlay.json" -o "$box/bundle-b/ephemera" ./internal/cli
for bundle in "$box/bundle-a" "$box/bundle-b"; do
  cp "$bundle/ephemera" "$bundle/ephemerad"
  cp packaging/macos/server-config.sh "$bundle"
done

export HOME="$box/home"
export EPHE_INSTALL_FIXTURE=1
export PATH="$box/bin:$PATH"
root="$HOME/Library/Application Support/Ephemera"
plist="$HOME/Library/LaunchAgents/com.illusions-lab.ephemera.plist"
tart="$root/tools/tart.app/Contents/MacOS/tart"
mkdir -p "$(dirname "$tart")" "$root/state"

cat > "$box/bin/launchctl" <<'SH'
#!/bin/bash
set -eu
root="$HOME/Library/Application Support/Ephemera"
case "$1:$2" in
  print:gui/*/com.illusions-lab.ephemera) test -f "$root/loaded" ;;
  print:gui/*/com.illusions-lab.ephemera.dev) test -f "$root/dev-loaded" ;;
  print:*) exit 0 ;;
  bootout:*) rm -f "$root/loaded" "$root/readiness.json" ;;
  bootstrap:*)
    args=()
    for i in {0..7}; do
      args+=("$(/usr/bin/plutil -extract "ProgramArguments.$i" raw "$3")")
    done
    release=$(dirname "${args[0]}")
    tart_version=$("$release/tart" --version)
    printf '%s\n' "$tart_version" >> "$root/starts"
    if [[ -f "$root/reject-version" && "$(cat "$root/reject-version")" == "$tart_version" ]]; then exit 1; fi
    "${args[@]}"
    printf '%s\n' "$release" > "$root/loaded"
    ;;
  *) exit 1 ;;
esac
SH
chmod +x "$box/bin/launchctl"
write_tart() {
  printf '#!/bin/bash\nversion=%s\n' "$1" > "$tart"
  cat >> "$tart" <<'SH'
set -eu
case "$1" in
  --version) echo "$version" ;;
  pull)
    test ! -e "$TART_HOME/reject-pull"
    mkdir -p "$TART_HOME/fixture-oci"
    touch "$TART_HOME/fixture-oci/${2##*@}"
    printf '%s\n' "$version" >> "$TART_HOME/fixture-pulls"
    ;;
  list)
    /usr/bin/python3 - "$TART_HOME" "$3" <<'PYTHON'
import json
from pathlib import Path
import sys

root, source = Path(sys.argv[1]), sys.argv[2]
if source == "oci":
    entries = [
        {"Name": "ghcr.io/cirruslabs/macos-tahoe-base@" + item.name}
        for item in (root / "fixture-oci").glob("sha256:*")
    ]
else:
    entries = [
        {"Name": item.name, "Running": (item / "running").exists()}
        for item in (root / "vms").glob("ephemera-*")
    ]
print(json.dumps(entries))
PYTHON
    ;;
  clone)
    mkdir -p "$TART_HOME/vms"
    case "$2" in
      *@*)
        test -f "$TART_HOME/fixture-oci/${2##*@}"
        mkdir "$TART_HOME/vms/$3"
        printf '{"version":1}\n' > "$TART_HOME/vms/$3/config.json"
        printf 'disk\n' > "$TART_HOME/vms/$3/disk.img"
        printf 'nvram\n' > "$TART_HOME/vms/$3/nvram.bin"
        ;;
      *)
        test -f "$TART_HOME/vms/$2/provisioned"
        cp -R "$TART_HOME/vms/$2" "$TART_HOME/vms/$3"
        ;;
    esac
    ;;
  set) test -d "$TART_HOME/vms/$2" ;;
  run)
    vm="$TART_HOME/vms/${!#}"
    touch "$vm/running"
    while [[ -f "$vm/running" ]]; do sleep 0.05; done
    ;;
  stop) rm -f "$TART_HOME/vms/$4/running" ;;
  delete) rm -rf "$TART_HOME/vms/$2" ;;
  exec)
    vm="$TART_HOME/vms/$2"
    test -f "$vm/running"
    case "$*" in
      *Runner.Listener*--version)
        test -f "$vm/provisioned"
        cat "$vm/provisioned"
        ;;
      *ephemera-provision*)
        version_arg="${@: -2:1}"
        printf '%s\n' "$version_arg" > "$vm/provisioned"
        printf '%s\n' "$2" >> "$TART_HOME/fixture-provisions"
        ;;
      *'/usr/bin/true'*) ;;
      *) exit 1 ;;
    esac
    ;;
  *) exit 1 ;;
esac
SH
  chmod +x "$tart"
}

for profile in first edited upgrade; do
  sed -e 's@https://github.com/OWNER/REPO@https://github.com/fixture/ephemera@' \
    -e 's/: 0$/: 1/' -e "s/ephemera-macos-test/$profile/" packaging/macos/config.dev.yaml > "$box/$profile.yaml"
done
# A separate OCI digest forces preparation on the upgrade while preserving
# the existing prepared image for rollback.
upgrade_digest=$(printf '%.0s1' {1..64})
sed -i '' -E "s/sha256:[[:xdigit:]]{64}/sha256:$upgrade_digest/" "$box/upgrade.yaml"
openssl genrsa -out "$box/first.pem" 2048 2>/dev/null
openssl genrsa -out "$box/upgrade.pem" 2048 2>/dev/null
"$box/bundle-a/ephemera" serve-status-fixture &
status_pid=$!
for _ in {1..50}; do
  if [[ -S "$root/state/status.sock" && -S "$root/state/ctl.sock" ]]; then break; fi
  sleep 0.1
done
[[ -S "$root/state/status.sock" && -S "$root/state/ctl.sock" ]]

base_version=$(cut -f1 "$manifest")
write_tart "$base_version"
install() {
  scripts/install-macos.sh --bundle "$box/bundle-$1" --non-interactive \
    --config "$box/$2.yaml" --github-app-key "$box/${3:-$2}.pem"
}
if scripts/install-macos.sh --bundle "$box/bundle-a" --non-interactive \
    --config "$box/first.yaml" --github-app-key "$box/first.pem" \
    --host-xcode "$box/missing Xcode.app" --xcode-version 27.2 --xcode-build 27B52 \
    >"$box/xcode-preflight.log" 2>&1; then
  echo 'installer accepted a missing host Xcode' >&2
  exit 1
fi
grep -F 'install full Xcode from Apple' "$box/xcode-preflight.log"
[[ ! -L "$root/current" && ! -e "$root/config.yaml" ]]

touch "$root/dev-loaded"
if install a first; then exit 1; fi
[[ ! -L "$root/current" ]]
rm "$root/dev-loaded"

printf '%s\n' "$base_version" > "$root/reject-version"
if install a first; then exit 1; fi
[[ ! -e "$plist" && ! -L "$root/current" && ! -f "$root/loaded" ]]
[[ ! -e "$root/.activation-transaction.json" && -s "$root/config.yaml" ]]
rm "$root/reject-version"
install a first
previous=$(readlink "$root/current")
[[ "$(cat "$root/loaded")" == "$previous" ]]
[[ "$("$HOME/.local/bin/ephe" version)" == "$("$box/bundle-a/ephemera" version)" ]]
/usr/bin/plutil -lint "$plist"
[[ "$(/usr/bin/plutil -extract ProgramArguments.0 raw "$plist")" == "$previous/ephemerad" ]]
[[ "$(/usr/bin/plutil -extract ProgramArguments.1 raw "$plist")" == --create ]]
initial_id=$(/usr/bin/plutil -extract ProgramArguments.7 raw "$plist")

cp "$box/edited.yaml" "$root/config.yaml"
"$HOME/.local/bin/ephe" setup
edited_id=$(/usr/bin/plutil -extract ProgramArguments.7 raw "$plist")
[[ -n "$edited_id" && "$initial_id" != "$edited_id" ]]
[[ "$(readlink "$root/current")" == "$previous" ]]
cp "$plist" "$box/previous.plist"
edit_snapshot=$(/usr/bin/plutil -extract ProgramArguments.3 raw "$plist")
grep -q 'scale_set_name: edited' "$edit_snapshot"

# Replacing the unversioned preseed must leave the active Tart and the old
# credential intact, even when a candidate has prepared a new image and key.
write_tart 99.0.0
touch "$root/state/tart/reject-pull"
starts=$(cat "$root/starts")
if install b upgrade; then exit 1; fi
cmp "$plist" "$box/previous.plist"
[[ "$(cat "$root/starts")" == "$starts" && "$(cat "$root/loaded")" == "$previous" ]]
rm "$root/state/tart/reject-pull"
printf '99.0.0\n' > "$root/reject-version"
if install b upgrade; then exit 1; fi
cmp "$plist" "$box/previous.plist"
[[ "$(readlink "$root/current")" == "$previous" && "$(cat "$root/loaded")" == "$previous" ]]
[[ "$("$previous/tart" --version)" == "$base_version" ]]
[[ "$(tail -2 "$root/starts")" == "$(printf '99.0.0\n%s' "$base_version")" ]]
grep -q 'scale_set_name: upgrade' "$root/config.yaml"
cmp "$box/upgrade.pem" "$root/credentials/github-app.pem"
cmp "$box/first.pem" "$(/usr/bin/plutil -extract ProgramArguments.5 raw "$plist")"

rm "$root/reject-version"
install b upgrade
upgraded=$(readlink "$root/current")
[[ "$upgraded" != "$previous" && "$("$upgraded/tart" --version)" == 99.0.0 ]]
[[ "$(cat "$root/loaded")" == "$upgraded" ]]
[[ "$("$previous/tart" --version)" == "$base_version" ]]
cp "$plist" "$box/upgraded.plist"

# Both candidate dependencies survive. An attempted switch to the older
# candidate can prepare its image, then roll back to the newer Tart.
printf '%s\n' "$base_version" > "$root/reject-version"
if install a first; then exit 1; fi
cmp "$plist" "$box/upgraded.plist"
[[ "$(readlink "$root/current")" == "$upgraded" && "$(cat "$root/loaded")" == "$upgraded" ]]
[[ "$(tail -2 "$root/starts")" == "$(printf '%s\n99.0.0' "$base_version")" ]]
[[ ! -e "$root/.activation-transaction.json" ]]
[[ "$(wc -l < "$root/state/tart/fixture-provisions" | tr -d ' ')" == 2 ]]
