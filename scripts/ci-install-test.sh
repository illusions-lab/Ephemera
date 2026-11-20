#!/usr/bin/env bash
# Run only on a disposable Ubuntu runner: this installs a system service.
set -euo pipefail
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/v1" "$work/v2" "$work/broken"
commit=$(git rev-parse HEAD)
for item in v1 v2 broken; do
  case "$item" in v1) version=v0.1.0;; v2) version=v0.1.1;; broken) version=v0.1.2;; esac
  flags="-X github.com/illusions-lab/Ephemera/internal/version.Version=$version -X github.com/illusions-lab/Ephemera/internal/version.Commit=$commit"
  go build -ldflags "$flags" -o "$work/$item/ephemera" ./cmd/ephemera
  go build -ldflags "$flags" -o "$work/$item/ephemerad" ./cmd/ephemerad
  cp scripts/install.sh "$work/$item/install.sh"
  cat > "$work/$item/ephemera.service" <<'UNIT'
[Unit]
Description=Ephemera installer CI fixture
[Service]
Type=oneshot
ExecStart=/usr/local/bin/ephemera config validate
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
UNIT
done
sed -i 's|ExecStart=/usr/local/bin/ephemera config validate|ExecStart=/bin/false|' "$work/broken/ephemera.service"
openssl genrsa -traditional -out "$work/key.pem" 2048
sudo bash scripts/install.sh --bundle "$work/v1" --non-interactive --config configs/ephemera.example.yaml --github-app-key "$work/key.pem"
ephe help
ephe image prepare --help
systemctl is-active --quiet ephemera.service
before=$(sudo sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)
start=$(systemctl show -p ActiveEnterTimestampMonotonic --value ephemera.service)
sudo bash scripts/install.sh --bundle "$work/v2"
test "$(systemctl show -p ActiveEnterTimestampMonotonic --value ephemera.service)" = "$start"
test "$(ephe version)" = "v0.1.0 $commit"
sudo ephe update --apply
test "$(ephe version)" = "v0.1.1 $commit"
sudo bash scripts/install.sh --bundle "$work/broken"
if sudo ephe update --apply; then echo 'broken update unexpectedly succeeded' >&2; exit 1; fi
test "$(ephe version)" = "v0.1.1 $commit"
systemctl is-active --quiet ephemera.service
test "$before" = "$(sudo sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)"

# Leave a real interrupted transaction after systemd begins starting the candidate.
cp -a "$work/broken" "$work/interrupted"
sed -i 's|ExecStart=/bin/false|ExecStart=/bin/sleep 60\nTimeoutStartSec=2s|' "$work/interrupted/ephemera.service"
sudo bash scripts/install.sh --bundle "$work/interrupted"
sudo systemd-run --unit=ephemera-ci-apply --property=Type=exec \
  /opt/ephemera/current/install.sh --update --apply
interrupted=false
for _ in {1..100}; do
  if [[ "$(systemctl show -p ActiveState --value ephemera.service)" == activating ]] &&
     sudo test -d /opt/ephemera/transaction; then
    sudo systemctl kill --signal=KILL ephemera-ci-apply.service
    interrupted=true
    break
  fi
  sleep 0.05
done
"$interrupted"
# Staging an offline bundle forces recovery without applying the candidate again.
sudo bash scripts/install.sh --bundle "$work/v1"
test "$(ephe version)" = "v0.1.1 $commit"
systemctl is-active --quiet ephemera.service
sudo test ! -e /opt/ephemera/transaction
sudo ephe update --apply
test "$(ephe version)" = "v0.1.0 $commit"
