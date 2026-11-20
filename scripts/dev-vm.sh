#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'HELP'
Usage: scripts/dev-vm.sh --image PATH_OR_HTTPS_URL --sha512 DIGEST
                         [--bundle DIR | --installer-smoke] [--keep]

Boot a fresh Debian amd64 cloud VM and verify cloud-init, SSH and systemd.
Use a versioned genericcloud qcow2 image and its published SHA-512 digest.

--bundle DIR  Install ephemera, ephemerad, ephemera.service and config.yaml from DIR.
              Exercise install.sh twice, then restart the service.
              Use a Linux amd64 binary and a secret-free test configuration.
--installer-smoke  Build the local CLI and test installation with a fixture unit.
--keep        Leave the VM running and print login and cleanup commands.
HELP
}

die() {
  printf 'dev-vm: %s\n' "$*" >&2
  exit 1
}

image=
digest=
bundle=
installer_smoke=false
keep=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image|--sha512|--bundle)
      [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
      case "$1" in
        --image) image=$2 ;;
        --sha512) digest=$2 ;;
        --bundle) bundle=$2 ;;
      esac
      shift 2
      ;;
    --installer-smoke) installer_smoke=true; shift ;;
    --keep) keep=true; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[[ -n "$image" ]] || die '--image is required'
[[ "$digest" =~ ^[[:xdigit:]]{128}$ ]] || die '--sha512 requires a 128-character hex digest'
if "$installer_smoke"; then
  [[ -z "$bundle" ]] || die 'choose either --bundle or --installer-smoke'
  command -v go >/dev/null || die 'required command missing: go'
fi
if [[ -n "$bundle" ]]; then
  bundle=$(cd -- "$bundle" && pwd) || die 'cannot open bundle directory'
  for artifact in ephemera ephemerad ephemera.service config.yaml; do
    [[ -f "$bundle/$artifact" && -r "$bundle/$artifact" ]] || die "missing bundle file: $artifact"
  done
fi

for command in qemu-system-x86_64 qemu-img cloud-localds ssh ssh-keygen python3; do
  command -v "$command" >/dev/null || die "required command missing: $command"
done
if [[ "$image" == https://* ]]; then
  command -v curl >/dev/null || die 'required command missing: curl'
else
  [[ -f "$image" && -r "$image" ]] || die "cannot read image: $image"
fi

cache_root=${XDG_CACHE_HOME:-"$HOME/.cache"}/ephemera/dev-vm
mkdir -p -- "$cache_root"
cache_root=$(cd -- "$cache_root" && pwd)
work_dir=$(mktemp -d "$cache_root/run.XXXXXXXX")
vm_pid=
ssh_ready=false
guest=debian@127.0.0.1

run_guest() {
  ssh -p "$port" "${ssh_options[@]}" "$guest" "$@"
}

cleanup() {
  local status=$?
  trap - EXIT
  set +e
  if (( status != 0 )); then
    if "$ssh_ready"; then
      run_guest 'sudo systemctl status ephemera --no-pager; sudo journalctl -u ephemera -n 100 --no-pager'
    fi
    if [[ -s "$work_dir/qemu.log" ]]; then
      tail -n 30 "$work_dir/qemu.log" >&2
    fi
    if [[ -s "$work_dir/console.log" ]]; then
      tail -n 50 "$work_dir/console.log" >&2
    fi
  fi
  if [[ -n "$vm_pid" ]] && kill -0 "$vm_pid" 2>/dev/null; then
    if "$keep"; then
      printf 'VM files: %s\nSSH: ' "$work_dir"
      printf '%q ' ssh -p "$port" "${ssh_options[@]}" "$guest"
      printf '\nRun sudo poweroff in the guest, then remove its files with: rm -rf -- %q\n' "$work_dir"
      return "$status"
    fi
    kill "$vm_pid"
    wait "$vm_pid" 2>/dev/null
  fi
  rm -rf -- "$work_dir"
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if "$installer_smoke"; then
  bundle="$work_dir/bundle"
  mkdir "$bundle"
  (
    cd "$script_dir/.."
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$bundle/ephemera" ./cmd/ephemera
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$bundle/ephemerad" ./cmd/ephemerad
    cp configs/ephemera.example.yaml "$bundle/config.yaml"
  )
  cat > "$bundle/ephemera.service" <<'UNIT'
[Unit]
Description=Ephemera installer smoke test

[Service]
Type=oneshot
LoadCredential=github-app-key:/etc/ephemera/credentials/github-app.pem
ExecStartPre=/usr/bin/test -s %d/github-app-key
ExecStartPre=/usr/local/bin/ephemerad --help
ExecStart=/usr/local/bin/ephemera config validate
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT
fi

digest=$(printf '%s' "$digest" | tr '[:upper:]' '[:lower:]')
base_image="$cache_root/$digest.qcow2"

verify_image() {
  python3 - "$1" "$digest" <<'PYTHON'
import hashlib
import sys

path, expected = sys.argv[1:]
digest = hashlib.sha512()
with open(path, "rb") as image:
    for chunk in iter(lambda: image.read(1024 * 1024), b""):
        digest.update(chunk)
if digest.hexdigest() != expected:
    sys.exit(f"SHA-512 mismatch: {path}")
PYTHON
}

if [[ ! -f "$base_image" ]]; then
  if [[ "$image" == https://* ]]; then
    curl --fail --location --proto '=https' --proto-redir '=https' \
      --retry 2 --output "$work_dir/base.qcow2" "$image"
  else
    cp -- "$image" "$work_dir/base.qcow2"
  fi
  verify_image "$work_dir/base.qcow2"
  mv -- "$work_dir/base.qcow2" "$base_image"
else
  verify_image "$base_image"
fi

qemu-img create -q -f qcow2 -F qcow2 -b "$base_image" "$work_dir/guest.qcow2"
ssh-keygen -q -t ed25519 -N '' -f "$work_dir/ssh-key"
cat > "$work_dir/user-data" <<CLOUD
#cloud-config
users:
  - name: debian
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: true
    ssh_authorized_keys:
      - $(cat "$work_dir/ssh-key.pub")
CLOUD
printf 'instance-id: %s\nlocal-hostname: ephemera-dev\n' "${work_dir##*/}" > "$work_dir/meta-data"
cloud-localds "$work_dir/seed.img" "$work_dir/user-data" "$work_dir/meta-data"

port=$(python3 - <<'PYTHON'
import socket

with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PYTHON
)
ssh_options=(
  -i "$work_dir/ssh-key" -o IdentitiesOnly=yes -o BatchMode=yes
  -o ConnectTimeout=3 -o ServerAliveInterval=5 -o ServerAliveCountMax=3
  -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$work_dir/known_hosts"
)
accel=tcg
if [[ "$(uname -m)" == x86_64 && -r /dev/kvm && -w /dev/kvm ]]; then
  accel=kvm
fi
printf 'Booting Debian with %s on SSH port %s\n' "$accel" "$port"
# QEMU treats commas in option values as separators unless escaped by doubling.
disk=${work_dir//,/,,}/guest.qcow2
seed=${work_dir//,/,,}/seed.img
nohup qemu-system-x86_64 -accel "$accel" -m 2048 -smp 2 \
  -display none -monitor none -serial "file:$work_dir/console.log" \
  -drive "file=$disk,format=qcow2,if=virtio" \
  -drive "file=$seed,format=raw,if=virtio,readonly=on" \
  -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$port-:22" \
  -device virtio-net-pci,netdev=net0 \
  < /dev/null > "$work_dir/qemu.log" 2>&1 &
vm_pid=$!

deadline=$((SECONDS + 300))
until run_guest true 2>/dev/null; do
  kill -0 "$vm_pid" 2>/dev/null || die 'QEMU exited before SSH became available'
  (( SECONDS < deadline )) || die 'timed out waiting for guest SSH'
  sleep 2
done
ssh_ready=true
run_guest 'sh -se' <<'GUEST'
timeout 180 cloud-init status --wait
test "$(cat /proc/1/comm)" = systemd
GUEST

if [[ -n "$bundle" ]]; then
  command -v scp >/dev/null || die 'required command missing: scp'
  # This per-run RSA key checks credential installation without GitHub access.
  ssh-keygen -q -t rsa -b 2048 -m PEM -N '' -f "$work_dir/test-github-app.pem"
  scp -P "$port" "${ssh_options[@]}" \
    "$bundle/ephemera" "$bundle/ephemerad" "$bundle/ephemera.service" "$bundle/config.yaml" \
    "$work_dir/test-github-app.pem" "$script_dir/install.sh" "$guest:"
  run_guest 'sudo bash -se' <<'GUEST'
timeout 120 bash install.sh --bundle . --non-interactive \
  --config config.yaml --github-app-key test-github-app.pem
test "$(readlink /usr/local/bin/ephe)" = ephemera
ephe help | grep -F "image prepare"
ephe image prepare --help 2>&1 | grep -F "image-cache"
ephe config validate
ephemera config validate
ephemerad --help
before=$(sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)
timeout 120 bash install.sh --bundle . --non-interactive
test "$before" = "$(sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)"
test "$(stat -c '%u:%a' /etc/ephemera/credentials/github-app.pem)" = 0:600
systemctl is-enabled ephemera
systemctl is-active ephemera
timeout 60 systemctl restart ephemera
systemctl is-active ephemera
GUEST
  printf 'Ephemera installation and service smoke test passed.\n'
else
  printf 'Debian boot, cloud-init, SSH and systemd smoke test passed.\n'
fi
