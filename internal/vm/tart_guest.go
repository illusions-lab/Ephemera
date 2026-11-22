package vm

// The pinned base image supplies the Guest Agent and its provisioning account.
const tartProvision = `
set -eu
name=ephemera-runner
root=/opt/ephemera-runner
if ! /usr/bin/id "$name" >/dev/null 2>&1; then
  /usr/sbin/sysadminctl -addUser "$name" -fullName "Ephemera runner" \
    -password "$(/usr/bin/openssl rand -hex 32)" -home "$root" -shell /bin/sh
fi
/usr/bin/id "$name" >/dev/null
if /usr/sbin/dseditgroup -o checkmember -m "$name" admin | /usr/bin/grep -q '^yes'; then
  exit 1
fi
/usr/bin/install -d -o "$name" -g staff -m 0700 "$root"
archive=$(/usr/bin/mktemp)
trap 'rm -f "$archive"' EXIT
/usr/bin/curl -fsSL --retry 2 -o "$archive" https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-osx-arm64-2.338.2.tar.gz
printf '%s  %s\n' 25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388 "$archive" | /usr/bin/shasum -a 256 -c -
/usr/bin/tar -xzf "$archive" -C "$root"
/usr/sbin/chown -R "$name":staff "$root"
`

const tartLaunch = `
set -eu
IFS= read -r ACTIONS_RUNNER_INPUT_JITCONFIG
test -n "$ACTIONS_RUNNER_INPUT_JITCONFIG"
export ACTIONS_RUNNER_INPUT_JITCONFIG
export HOME=/opt/ephemera-runner
cd "$HOME"
exec ./bin/Runner.Listener run
`
