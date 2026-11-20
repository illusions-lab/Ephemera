#!/usr/bin/env bash
# Run only on a disposable Ubuntu runner: this installs a system service.
set -euo pipefail
work=$(mktemp -d)
cleanup() {
  local status=$?
  if (( status != 0 )); then
    sudo journalctl -u ephemera.service -u ephemera-ci-apply.service -n 100 --no-pager || true
    systemctl cat ephemera.service || true
  fi
  if [[ -f "$work/hosts" ]]; then
    sudo systemctl stop ephemera-ci-github.service || true
    sudo cp "$work/hosts" /etc/hosts
  fi
  rm -rf "$work"
}
trap cleanup EXIT
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
real_systemctl=$(command -v systemctl)
mkdir "$work/wrappers"
cat > "$work/wrappers/systemctl" <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CI_WORK/systemctl.log"
if [[ "$*" == 'enable ephemera.service' && -f "$CI_WORK/fail-enable" ]]; then
  "$CI_SYSTEMCTL" "$@"
  "$CI_SYSTEMCTL" is-enabled --quiet ephemera.service
  sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem > "$CI_WORK/first-hashes"
  echo 'CI: returning failure after enabling service' >&2
  exit 1
fi
exec "$CI_SYSTEMCTL" "$@"
WRAPPER
chmod +x "$work/wrappers/systemctl"
cp configs/ephemera.example.yaml "$work/config.yaml"
install_cmd=(sudo env "PATH=$work/wrappers:$PATH" "CI_SYSTEMCTL=$real_systemctl" "CI_WORK=$work"
  bash scripts/install.sh --bundle "$work/v1" --non-interactive --config "$work/config.yaml" --github-app-key "$work/key.pem")
assert_first_install_rolled_back() {
  test ! -e /opt/ephemera/current && test ! -L /opt/ephemera/current || return 1
  test ! -e /etc/systemd/system/ephemera.service && test ! -L /etc/systemd/system/ephemera.service || return 1
  test ! -L /etc/systemd/system/multi-user.target.wants/ephemera.service
  if systemctl is-enabled --quiet ephemera.service || systemctl is-active --quiet ephemera.service; then return 1; fi
  sudo test ! -e /opt/ephemera/transaction
  sudo sha256sum --check "$work/first-hashes"
  test "$(sudo stat -c '%a' /etc/ephemera/config.yaml)" = 644
  test "$(sudo stat -c '%a' /etc/ephemera/credentials)" = 700
  test "$(sudo stat -c '%a' /etc/ephemera/credentials/github-app.pem)" = 600
}
touch "$work/fail-enable"
if "${install_cmd[@]}" >"$work/enable.log" 2>&1; then
  echo 'first install unexpectedly succeeded after enable failure' >&2
  exit 1
fi
cat "$work/enable.log"
grep -F 'CI: returning failure after enabling service' "$work/enable.log"
grep -F 'install: failed to enable service' "$work/enable.log"
rm "$work/fail-enable"
assert_first_install_rolled_back

# Change the candidate unit so an accidental staging operation would change the link.
cp "$work/v1/ephemera.service" "$work/working.service"
sed -i '/^ExecStart=/i ExecStartPre=/bin/false' "$work/v1/ephemera.service"
staged_before=$(readlink /opt/ephemera/staged)
cp "$work/systemctl.log" "$work/systemctl.before"
sed -i 's/max_concurrent: 2/max_concurrent: 3/' "$work/config.yaml"
if "${install_cmd[@]}" >"$work/mismatch.log" 2>&1; then
  echo 'first install unexpectedly accepted different inputs' >&2
  exit 1
fi
cat "$work/mismatch.log"
grep -F 'configuration does not match the saved configuration' "$work/mismatch.log"
test "$(readlink /opt/ephemera/staged)" = "$staged_before"
cmp "$work/systemctl.before" "$work/systemctl.log"
assert_first_install_rolled_back
cp configs/ephemera.example.yaml "$work/config.yaml"

if "${install_cmd[@]}" >"$work/restart.log" 2>&1; then
  echo 'first install unexpectedly succeeded after restart failure' >&2
  exit 1
fi
cat "$work/restart.log"
grep -F 'install: service failed to become ready' "$work/restart.log"
assert_first_install_rolled_back

cp "$work/working.service" "$work/v1/ephemera.service"
"${install_cmd[@]}"
test "$(ephe version)" = "v0.1.0 $commit"
systemctl is-enabled --quiet ephemera.service
sudo sha256sum --check "$work/first-hashes"
ephe help
ephe image prepare --help
systemctl is-active --quiet ephemera.service
ephe status --json | python3 -c 'import json, sys; report = json.load(sys.stdin); assert report["service"]["state"] == "active" and report["daemon"] is None, report'
before=$(sudo sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)
start=$(systemctl show -p ActiveEnterTimestampMonotonic --value ephemera.service)
for tag in v0.1.1 v0.1.2; do
  mkdir "$work/$tag"
  tar -C "$work/v2" -czf "$work/$tag/ephemera_linux_amd64.tar.gz" \
    ephemera ephemerad ephemera.service install.sh
  (cd "$work/$tag" && sha256sum ephemera_linux_amd64.tar.gz > checksums.txt)
done
echo corrupt >> "$work/v0.1.2/ephemera_linux_amd64.tar.gz"
echo v0.1.2 > "$work/latest"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=github.com \
  -addext 'subjectAltName=DNS:github.com' -keyout "$work/tls.key" -out "$work/tls.pem"
cp /etc/hosts "$work/hosts"
echo '127.0.0.1 github.com' | sudo tee -a /etc/hosts >/dev/null
sudo systemd-run --unit=ephemera-ci-github --property="StandardError=append:$work/server.log" \
  python3 "$PWD/scripts/ci-github.py" "$work"
export CURL_CA_BUNDLE="$work/tls.pem"
curl --retry 10 --retry-connrefused --retry-delay 1 -sS https://github.com/probe >/dev/null
if sudo env CURL_CA_BUNDLE="$CURL_CA_BUNDLE" ephe update; then
  echo 'corrupt update unexpectedly staged' >&2
  exit 1
fi
sudo test ! -e /opt/ephemera/staged
echo v0.1.1 > "$work/latest"
sudo env CURL_CA_BUNDLE="$CURL_CA_BUNDLE" ephe update
output=$(ephe update --check)
grep -Fx "Current: v0.1.0 $commit" <<< "$output"
grep -Fx "Staged: v0.1.1 $commit" <<< "$output"
grep -Fx 'Latest: v0.1.1' <<< "$output"
assets=$(grep -c '/download/v0.1.1/' "$work/server.log")
sudo env CURL_CA_BUNDLE="$CURL_CA_BUNDLE" ephe update
test "$(grep -c '/download/v0.1.1/' "$work/server.log")" = "$assets"
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

# A timed-out candidate must leave a previously stopped service stopped.
sudo systemctl stop ephemera.service
sudo install -d /var/lib/ephemera/image
printf 'cache sentinel\n' | sudo tee /var/lib/ephemera/image/ci-sentinel >/dev/null
cache_before=$(sudo sha256sum /var/lib/ephemera/image/ci-sentinel)
sudo bash scripts/install.sh --bundle "$work/interrupted"
if sudo ephe update --apply; then echo 'timed-out update unexpectedly succeeded' >&2; exit 1; fi
test "$(ephe version)" = "v0.1.0 $commit"
test "$(systemctl show -p ActiveState --value ephemera.service)" = inactive
sudo test ! -e /opt/ephemera/transaction
test "$before" = "$(sudo sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)"
test "$cache_before" = "$(sudo sha256sum /var/lib/ephemera/image/ci-sentinel)"
