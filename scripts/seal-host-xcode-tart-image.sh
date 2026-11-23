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
wwdr_intermediates=$("$tart" exec -i "$guest" /usr/bin/sudo -n /bin/bash -s -- "$version" "$build" <<'GUEST'
set -euo pipefail
export LC_ALL=C
exec 3>&1 1>&2
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

# Code signing builds its CMS chain from certificates visible to the guest.
system_keychain=/Library/Keychains/System.keychain
apple_pki=https://www.apple.com/certificateauthority/
download() {
  /usr/bin/curl --fail --silent --show-error --location --retry 3 --max-time 30 \
    --proto '=https' --proto-redir '=https' --max-filesize "$1" "$2" -o "$3"
}
download 1048576 "$apple_pki" "$tmp/apple-pki.html"
paths=$(grep -Eo '/certificateauthority/AppleWWDRCAG[0-9]+\.cer' "$tmp/apple-pki.html" | sort -u) ||
  { echo 'No WWDR generation certificates found in the Apple PKI catalog.' >&2; exit 1; }
/usr/bin/security find-certificate -a -c 'Apple Root CA' -p \
  /System/Library/Keychains/SystemRootCertificates.keychain > "$tmp/apple-roots.pem"

manifest="$tmp/wwdr.tsv"
now=$(/bin/date +%s)
while IFS= read -r path; do
  name=${path##*/}
  download 65536 "$apple_pki$name" "$tmp/$name"
  pem="$tmp/$name.pem"
  /usr/bin/openssl x509 -inform DER -in "$tmp/$name" -out "$pem"
  starts=$(/usr/bin/openssl x509 -in "$pem" -noout -startdate)
  valid_from=$(/bin/date -j -u -f '%b %e %T %Y %Z' "${starts#notBefore=}" +%s)
  if [[ "$valid_from" -gt "$now" ]] || ! /usr/bin/openssl x509 -in "$pem" -noout -checkend 0 >/dev/null; then
    printf 'Skipping WWDR certificate outside its validity period: %s\n' "$name"
    continue
  fi
  /usr/bin/openssl x509 -in "$pem" -noout -subject -nameopt RFC2253 |
    grep -E '(^subject= *|,)CN=Apple Worldwide Developer Relations (Certification Authority|CA - G[0-9]+)(,|$)' >/dev/null
  /usr/bin/openssl x509 -in "$pem" -noout -text | grep -F 'CA:TRUE' >/dev/null
  /usr/bin/openssl verify -trusted "$tmp/apple-roots.pem" "$pem"
  fingerprint=$(/usr/bin/openssl x509 -in "$pem" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d :)
  printf '%s\t%s\n' "$name" "$fingerprint" >> "$manifest"
done <<< "$paths"
[[ -s "$manifest" ]] || { echo 'No valid WWDR intermediates in the Apple PKI catalog.' >&2; exit 1; }

has_certificate() {
  /usr/bin/security find-certificate -a -Z "$system_keychain" |
    grep -Fx "SHA-256 hash: $1" >/dev/null
}
while IFS=$'\t' read -r name fingerprint; do
  if ! has_certificate "$fingerprint"; then
    /usr/bin/security add-certificates -k "$system_keychain" "$tmp/$name.pem"
  fi
  has_certificate "$fingerprint" ||
    { printf 'WWDR intermediate %s is missing from the VM system keychain.\n' "$name" >&2; exit 1; }
done < "$manifest"

printf 'print("Ephemera macOS compile check")\n' > "$tmp/main.swift"
/usr/bin/xcrun --sdk macosx swiftc "$tmp/main.swift" -o "$tmp/smoke"
"$tmp/smoke"
sdk=$(/usr/bin/xcrun --sdk iphoneos --show-sdk-path)
/usr/bin/xcrun --sdk iphoneos swiftc -target arm64-apple-ios18.0 -sdk "$sdk" -emit-object "$tmp/main.swift" -o "$tmp/ios.o"
test -s "$tmp/ios.o"
/bin/launchctl disable system/com.openssh.sshd
/bin/sync
cat "$manifest" >&3
GUEST
)
# Shut down from the guest so the newly prepared APFS volume is flushed.
"$tart" exec "$guest" /usr/bin/sudo -n /sbin/shutdown -h now >&2 || true
stopped=false
for ((attempt=0; attempt<60; attempt++)); do
  if "$tart" list --source local --format json | jq -e --arg name "$guest" 'any(.[]; .Name == $name and .State == "stopped")' >/dev/null; then
    stopped=true
    break
  fi
  sleep 2
done
"$stopped" || { echo 'Guest did not shut down cleanly; no image was sealed.' >&2; exit 1; }
name="apple-macos-$(uuidgen | tr '[:upper:]' '[:lower:]')"
destination="$state/vms/$name"
receipt="$state/$name.apple-build.json"
[[ ! -e "$destination" && ! -e "$receipt" ]]
draft=$(mktemp "$state/.apple-build.XXXXXXXX")
trap 'if [[ -e "$draft" ]]; then unlink "$draft"; fi' EXIT
jq -eRn --arg os "$os_build" --arg version "$version" --arg build "$build" \
  '[inputs | if test("^AppleWWDRCAG[0-9]+\\.cer\\t[A-F0-9]{64}$")
    then split("\t") | {name:.[0],sha256:.[1]}
    else error("invalid WWDR certificate receipt")
    end] | select(length > 0) |
    {source:"apple-ipsw+host-xcode",os_build:$os,xcode_version:$version,xcode_build:$build,wwdr_intermediates:.}' \
  <<< "$wwdr_intermediates" > "$draft"
mv "$source" "$destination"
if ! mv "$draft" "$receipt"; then
  mv "$destination" "$source"
  exit 1
fi
printf 'local:%s\n' "$name"
