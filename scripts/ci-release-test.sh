#!/usr/bin/env bash
# Use only on a disposable runner.
set -euo pipefail
bundle=$(realpath "$1")
work=$(mktemp -d)
cleanup() {
  status=$?
  if (( status != 0 )); then
    sudo journalctl -u ephemera -n 100 --no-pager || true
    cat "$work/server.log" 2>/dev/null || true
  fi
  sudo systemctl stop ephemera.service ephemera-ci-github.service 2>/dev/null || true
  if [[ -f "$work/hosts" ]]; then sudo cp "$work/hosts" /etc/hosts; fi
  rm -rf "$work"
}
trap cleanup EXIT

arch=$(dpkg --print-architecture)
awk -v arch="$arch" '
  /^  arch: / { $0 = "  arch: " arch }
  /^  scale_set_name: / { $0 = "  scale_set_name: ephemera-ubuntu-" arch }
  { print }
  /^  memory_mib:/ { print "  disk_gib: 16" }
' configs/ephemera.example.yaml > "$work/config.yaml"
disk=$(sudo env LIBGUESTFS_BACKEND=direct EPHEMERA_OFFLINE_BOOT_DEBUG=1 \
  "$bundle/ephemera" image prepare --config "$work/config.yaml")
test -n "$disk"
sudo qemu-img info --output=json "$disk" | python3 -c \
  'import json, sys; assert json.load(sys.stdin)["virtual-size"] == 16 * 2**30'
guestfish_cmd=(sudo env LIBGUESTFS_BACKEND=direct guestfish --ro --format=qcow2 -a "$disk")
root_partition=$("${guestfish_cmd[@]}" run : inspect-os)
[[ "$root_partition" =~ ^/dev/sda[0-9]+$ ]]
root_size=$("${guestfish_cmd[@]}" run : blockdev-getsize64 "$root_partition")
[[ "$root_size" =~ ^[0-9]+$ ]]
printf 'Guest root partition %s has %s bytes\n' "$root_partition" "$root_size"
# Leave room for the Ubuntu boot, EFI and reserved partitions.
(( root_size >= 14 * 2**30 && root_size < 16 * 2**30 ))
openssl genrsa -traditional -out "$work/key.pem" 2048
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=github.com \
  -addext 'subjectAltName=DNS:github.com,DNS:api.github.com' \
  -keyout "$work/tls.key" -out "$work/tls.pem"
# The daemon uses a scoped CA bundle. Ordinary CI tools keep their normal trust.
cat /etc/ssl/certs/ca-certificates.crt "$work/tls.pem" > "$work/ca.pem"
chmod 0755 "$work"
chmod 0644 "$work/ca.pem"
sudo mkdir -p /etc/systemd/system/ephemera.service.d
printf '[Service]\nEnvironment=SSL_CERT_FILE=%s\n' "$work/ca.pem" |
  sudo tee /etc/systemd/system/ephemera.service.d/ci.conf >/dev/null
cp /etc/hosts "$work/hosts"
echo '127.0.0.1 github.com api.github.com' | sudo tee -a /etc/hosts >/dev/null
sudo systemd-run --unit=ephemera-ci-github --property="StandardError=append:$work/server.log" \
  python3 "$PWD/scripts/ci-github.py" "$work"
for _ in {1..20}; do
  if curl -s --cacert "$work/tls.pem" https://api.github.com/probe >/dev/null; then break; fi
  sleep 1
done
sudo bash "$bundle/install.sh" --bundle "$bundle" --non-interactive \
  --config "$work/config.yaml" --github-app-key "$work/key.pem"
systemctl is-active --quiet ephemera.service
test "$(systemctl show -p StatusText --value ephemera.service)" = Healthy
sudo ephe service restart
systemctl is-active --quiet ephemera.service
test "$(systemctl show -p StatusText --value ephemera.service)" = Healthy
sudo ephe service stop
test "$(systemctl show -p ActiveState --value ephemera.service)" = inactive
