#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat >&2 <<'HELP'
Usage:
  build-official-tart-image.sh create APPLE_IPSW XCODE_ARCHIVES DISK_GB
  build-official-tart-image.sh provision VM APPLE_IPSW XCODE_ARCHIVES UPSTREAM_CHECKOUT

Run as the Ephemera service account on the deployment host. Set TART to the
verified Tart binary if it is not on PATH. For provision, start the VM created
by this script and set PKR_VAR_vm_username, PKR_VAR_vm_password and
PKR_VAR_xcode_install_storage_url. The URL must serve the verified archives
from a private endpoint reachable by the guest.
HELP
  exit 2
}

fail() { printf '%s\n' "$*" >&2; exit 1; }

[[ $# -ge 1 ]] || usage
case "$1:$#" in create:4|provision:5) ;; *) usage ;; esac
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID != 0 ]] ||
  fail 'Run on Apple Silicon as the Ephemera service account.'

revision=6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab
script_dir=$(cd "$(dirname "$0")" && pwd)
tart=${TART:-tart}
if [[ $1 == create ]]; then
  bash "$script_dir/check-official-macos-inputs.sh" "$2" "$3"
else
  bash "$script_dir/check-official-macos-inputs.sh" "$3" "$4"
fi
export TART_HOME="$HOME/Library/Application Support/Ephemera/state/tart"
mkdir -p "$TART_HOME"
[[ ! -L "$TART_HOME" && $(stat -f '%u:%Lp' "$TART_HOME") == "$EUID:700" ]] ||
  fail 'Ephemera Tart home must be private and owned by the service account.'

if [[ $1 == create ]]; then
  [[ $4 =~ ^[1-9][0-9]*$ ]] || fail 'Disk size must be a positive number of GiB.'
  name="github-macos26-$(uuidgen | tr '[:upper:]' '[:lower:]')"
  "$tart" create "$name" --from-ipsw "$2" --disk-size "$4"
  (set -o noclobber; printf '%s\n' "$revision" > "$TART_HOME/$name.github-pending")
  printf '\nCreated %s. Start it and complete guest setup before provisioning.\n' "$name"
  exit
fi

name=$2
[[ $name =~ ^github-macos26-[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]] ||
  fail 'Specify the VM created by this script.'
receipt="$TART_HOME/$name.github-revision"
pending="$TART_HOME/$name.github-pending"
[[ -f "$pending" && ! -e "$receipt" && $(cat "$pending") == "$revision" ]] ||
  fail 'VM has no matching pending build or is already provisioned.'
upstream=$(cd "$5" && pwd)
[[ $(git -C "$upstream" rev-parse HEAD) == "$revision" &&
   -z $(git -C "$upstream" status --porcelain --untracked-files=all) ]] ||
  fail 'Use a clean checkout of the pinned GitHub runner-images revision.'
: "${PKR_VAR_vm_username:?Set the disposable guest username}"
: "${PKR_VAR_vm_password:?Set the disposable guest password}"
: "${PKR_VAR_xcode_install_storage_url:?Set the private Xcode archive base URL}"
[[ $PKR_VAR_xcode_install_storage_url == */ ]] || fail 'Archive base URL must end in /.'
export PKR_VAR_xcode_install_sas=${PKR_VAR_xcode_install_sas:-}
[[ $("$tart" exec "$name" /usr/bin/sw_vers -buildVersion) == 25G332 ]] ||
  fail 'The guest must run Apple build 25G332 and have a working Tart Guest Agent.'
ip=$("$tart" ip "$name" --wait 30)
[[ -n "$ip" ]] || fail 'Guest IP is unavailable.'
template="$upstream/images/macos/templates/macOS-26.arm64.anka.pkr.hcl"
mkdir -p "$upstream/images/image-output"
packer init "$template"
options=(-var builder_type=null -var "source_vm_name=$ip" -var "build_id=$name")
packer validate "${options[@]}" "$template"
packer build "${options[@]}" "$template"
[[ -s "$upstream/images/image-output/software-report.json" ]] ||
  fail 'GitHub provisioning did not produce its software report.'
"$tart" exec "$name" /usr/bin/sudo -n /usr/bin/true
"$tart" exec "$name" /usr/bin/xcodebuild -version
for sdk in macosx iphoneos; do
  "$tart" exec "$name" /usr/bin/xcrun --sdk "$sdk" --show-sdk-path
done
# Keep SSH confined to provisioning. The Runner is bootstrapped through the guest agent.
"$tart" exec "$name" /usr/bin/sudo -n /bin/launchctl disable system/com.openssh.sshd
"$tart" stop "$name"
(set -o noclobber; printf '%s\n' "$revision" > "$receipt")
printf 'Set vm.image_ref to local:%s after reviewing the upstream software report.\n' "$name"
