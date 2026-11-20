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

sudo env LIBGUESTFS_BACKEND=direct "$bundle/ephemera" image prepare --config configs/ephemera.example.yaml
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
  --config configs/ephemera.example.yaml --github-app-key "$work/key.pem"
systemctl is-active --quiet ephemera.service
test "$(systemctl show -p StatusText --value ephemera.service)" = Healthy
sudo systemctl stop ephemera.service
test "$(systemctl show -p ActiveState --value ephemera.service)" = inactive
