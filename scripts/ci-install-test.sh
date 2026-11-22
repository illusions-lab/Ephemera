#!/usr/bin/env bash
# Run only on a disposable Ubuntu runner: this installs a system service.
set -euo pipefail
work=$(mktemp -d)
archive=ephemera_linux_$(dpkg --print-architecture).tar.gz
cleanup() {
  local status=$?
  if (( status != 0 )); then
    sudo journalctl -u ephemera.service -u ephemera-recover.service -u ephemera-ci-apply.service -u ephemera-ci-control.service -n 100 --no-pager || true
    systemctl cat ephemera.service || true
  fi
  if [[ -f "$work/hosts" ]]; then
    sudo systemctl stop ephemera-ci-github.service || true
    sudo cp "$work/hosts" /etc/hosts
  fi
  sudo systemctl stop ephemera-ci-control.service || true
  sudo rm -f /var/lib/ephemera/instances/ctl.sock
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
ExecCondition=+/opt/ephemera/recover-install --guard-start
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
  tar -C "$work/v2" -czf "$work/$tag/$archive" \
    ephemera ephemerad ephemera.service install.sh
  (cd "$work/$tag" && sha256sum "$archive" > checksums.txt)
done
echo corrupt >> "$work/v0.1.2/$archive"
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
# An active old service without a drain socket must keep the candidate staged.
if { sudo ephe update --apply; } >"$work/no-drain.log" 2>&1; then
  echo 'upgrade unexpectedly stopped an unverified legacy service' >&2
  exit 1
fi
grep -F 'maintenance window' "$work/no-drain.log"
test "$(readlink /opt/ephemera/staged)" != "$(readlink /opt/ephemera/current)"
test "$(systemctl show -p ActiveEnterTimestampMonotonic --value ephemera.service)" = "$start"

# The installer fixture is a oneshot, so a separate local service supplies
# the drain lease. Its blocking file simulates a job that is still running.
sudo install -d -m 0700 /var/lib/ephemera/instances
cat > "$work/control.py" <<'PYTHON'
import json
from pathlib import Path
import socket
import sys

root = Path(sys.argv[1])
with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
    listener.bind("/var/lib/ephemera/instances/ctl.sock")
    Path("/var/lib/ephemera/instances/ctl.sock").chmod(0o600)
    listener.listen()
    while True:
        connection, _ = listener.accept()
        with connection, connection.makefile("rb") as requests:
            for line in requests:
                request = json.loads(line)
                assert not request.get("Abort"), request
                active = (root / "job-running").exists()
                reply = {"Ready": not active}
                if active:
                    reply["Jobs"] = [{
                        "Instance": "fixture-instance", "Request": 123,
                        "Repository": "fixture/project", "Name": "Long job",
                        "Runner": "ephemera-fixture", "Run": 456,
                    }]
                try:
                    connection.sendall(json.dumps(reply).encode() + b"\n")
                    if active:
                        (root / "saw-drain").touch()
                except BrokenPipeError:
                    break
PYTHON
sudo systemd-run --unit=ephemera-ci-control python3 "$work/control.py" "$work"
for _ in {1..100}; do
  if sudo test -S /var/lib/ephemera/instances/ctl.sock; then break; fi
  sleep 0.05
done
sudo test -S /var/lib/ephemera/instances/ctl.sock
touch "$work/job-running"
{ sudo ephe update --apply; } </dev/null >"$work/drain.log" 2>&1 &
apply_pid=$!
for _ in {1..100}; do
  if [[ -f "$work/saw-drain" ]]; then break; fi
  sleep 0.05
done
test -f "$work/saw-drain"
test "$(ephe version)" = "v0.1.0 $commit"
test "$(systemctl show -p ActiveEnterTimestampMonotonic --value ephemera.service)" = "$start"
rm "$work/job-running"
wait "$apply_pid"
grep -F 'repository="fixture/project" run=456 job="Long job"' "$work/drain.log"
test "$(ephe version)" = "v0.1.1 $commit"
sudo bash scripts/install.sh --bundle "$work/broken"
if sudo ephe update --apply; then echo 'broken update unexpectedly succeeded' >&2; exit 1; fi
test "$(ephe version)" = "v0.1.1 $commit"
systemctl is-active --quiet ephemera.service
test "$before" = "$(sudo sha256sum /etc/ephemera/config.yaml /etc/ephemera/credentials/github-app.pem)"

# Inject both a catchable interruption and a hard kill during activation.
cp -a "$work/broken" "$work/interrupted"
sed -i 's|ExecStart=/bin/false|ExecStart=/bin/sleep 60\nTimeoutStartSec=2s|' "$work/interrupted/ephemera.service"
sudo bash scripts/install.sh --bundle "$work/interrupted"
interrupt_install() {
  local signal=$1 wrapper=$2
  sudo systemd-run --unit="$wrapper" --property=Type=exec \
    /opt/ephemera/current/install.sh --update --apply
  for _ in {1..100}; do
    if [[ "$(systemctl show -p ActiveState --value ephemera.service)" == activating ]] &&
       sudo test -f /opt/ephemera/transaction/switching; then
      sudo systemctl kill --signal="$signal" "$wrapper"
      for _ in {1..100}; do
        case "$(systemctl show -p ActiveState --value "$wrapper")" in
          failed|inactive) return 0 ;;
        esac
        sleep 0.05
      done
      echo "interrupted installer did not exit ($signal)" >&2
      return 1
    fi
    sleep 0.05
  done
  echo "candidate did not reach the interruptible activation phase ($signal)" >&2
  return 1
}
assert_recovered() {
  test "$(ephe version)" = "v0.1.1 $commit" || return
  systemctl is-active --quiet ephemera.service || return
  sudo test ! -e /opt/ephemera/transaction || return
  cmp "$work/v2/ephemera.service" /etc/systemd/system/ephemera.service
}

interrupt_install TERM ephemera-ci-apply-term
for _ in {1..100}; do
  if assert_recovered; then break; fi
  sleep 0.05
done
assert_recovered

interrupt_install KILL ephemera-ci-apply
sudo test -d /opt/ephemera/transaction
# Recover through the independent unit without running the installer again.
sudo systemctl start ephemera-recover.service
assert_recovered
sudo systemctl start ephemera-recover.service
assert_recovered

interrupt_install KILL ephemera-ci-apply-boot
sudo systemctl stop ephemera.service
# Change the saved boot ID to exercise early boot recovery on this host.
echo previous-boot | sudo tee /opt/ephemera/transaction/boot-id >/dev/null
sudo systemctl daemon-reload
sudo systemctl start ephemera.service
assert_recovered

sudo bash scripts/install.sh --bundle "$work/v1"
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
