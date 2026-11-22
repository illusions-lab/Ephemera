# macOS runner installation and acceptance

Use macOS 26 or newer on Apple Silicon. Run the installer in Terminal as the
account that will run Ephemera. The installer uses the [bundled Tart manifest](../../internal/macos/tart-manifest.tsv)
and stores
the release, GitHub App credentials, and VM state under
`~/Library/Application Support/Ephemera`. It places `ephe` and `ephemera` in
`~/.local/bin` and installs `com.illusions-lab.ephemera` in `~/Library/LaunchAgents`.
The agent runs while that account is logged in, including after it logs
back in after a restart. FileVault may require an unlock before login.

During interactive setup over SSH, Ephemera uses an existing `cloudflared`
or downloads a verified copy for that session. Open the public URL between
the divider lines in your local browser. Setup prints SSH port-forwarding
instructions as a fallback.

The first interactive setup asks whether the Mac is dedicated to runners.
**No** is the default. **Yes** applies the AC settings shown in the prompt.
They disable idle system sleep on AC and enable restart after power loss on
hardware that supports it. You may be prompted for your administrator
password. The settings are based on the host setup in [mac-mini-ci](https://github.com/diegog/mac-mini-ci).

To restore the previously recorded power settings:

```sh
"$HOME/Library/Application Support/Ephemera/current/server-config.sh" --restore
```

To use credentials for an existing GitHub App, pass `--existing-app`. For
unattended setup, pass `--non-interactive --config /path/to/config.yaml
--github-app-key /path/to/github-app.pem` with a valid Tart configuration.
Unattended setup uses the supplied execution setting and skips power changes.
Interactive setup asks whether to observe or execute jobs and lets you
choose the runner limit, Tart image, CPU count, and memory. Use `ephe config`
to edit these settings later, including over SSH. Run `ephe config apply`
to review and activate the saved change. The service account needs an active
macOS graphical login session for activation. It can be locked while the
command is run over SSH. Scripts can pass `--yes` to confirm the apply and
wait for current jobs without a terminal.
Use `ephe doctor`, `ephe status`, and
`tail -F "$HOME/Library/Logs/Ephemera/daemon.log"` to inspect the service.

Rerun the installer to upgrade, or pass `--version TAG` to select a specific
release. Installation and direct `ephe setup` use the same activation
transaction. The active LaunchAgent reads the YAML and PEM from its private
activation snapshot. Ordinary service restarts use that snapshot until you
apply a saved edit. Check `ephe status` for pending changes and `ephe ps -a`
for local runners and cleanup. If the candidate fails to become healthy, setup restores the previous
service and keeps the editable inputs for a retry. If setup is killed during
the switch, the next LaunchAgent startup checks the recovery journal and
restores the previous service. Rerunning setup or the installer can also
recover the transaction. If recovery fails, use the reported journal path
when investigating the failure.

For a local build, run `make bundle-macos`, place the version listed in
`bin/macos-bundle/tart-manifest.tsv` at
`~/Library/Application Support/Ephemera/tools/tart.app`, and run
`/bin/bash scripts/install-macos.sh --bundle bin/macos-bundle`. Ensure the
Ephemera directory has mode `0700`. The installer stages a separate copy
of the prebuilt dependency, so it remains available after switching the
prebuilt app for another version. Setup still needs GitHub access and may
fetch the configured VM image.

## Development without the installer

Run the daemon as your logged-in macOS user on Apple Silicon. The macOS
guest uses a separate, non-admin `ephemera-runner` account for jobs. Use a
dedicated test Scale Set and trusted workflows.

Build with `make build` and put the manifest version of Tart on your PATH. Create a private
directory with `mkdir -m 700 "$HOME/Library/Application Support/Ephemera"`. Place a copy of
`config.dev.yaml` named `config.yaml` and the test GitHub App key named
`github-app.pem` there. Fill in the GitHub values and set the key mode to
`0600`. The daemon keeps its Tart cache and clone state in that directory.

Run the daemon from the checkout:

```sh
state="$HOME/Library/Application Support/Ephemera"
bin/ephemerad --config "$state/config.yaml" --state-dir "$state" \
  --github-app-key "$state/github-app.pem" --status-socket "$state/status.sock" --create
```

Set `--create` when the dedicated Scale Set needs to be registered. Run
`bin/ephemera status --socket "$state/status.sock"` to inspect the daemon.
The pinned image is downloaded on first startup.

For restart testing without the installer, replace the placeholders in a copy of the
[LaunchAgent](com.illusions-lab.ephemera.dev.plist) with absolute paths.
The development agent writes to `~/Library/Logs/Ephemera/daemon-dev.log`.
Stop the foreground daemon and register the plist with
`launchctl bootstrap "gui/$(id -u)" /absolute/path/to/the.plist`. Use
`launchctl bootout "gui/$(id -u)/com.illusions-lab.ephemera.dev"` to stop the
test agent, or boot it out and register it again after changing its plist.
With an installed runner, use `com.illusions-lab.ephemera` in the acceptance
steps below.

1. Dispatch [Dedicated Scale Set acceptance](../../.github/workflows/execution-acceptance.yml)
   with `guest_os=macos`, `arch=arm64`, and the dedicated Scale Set label.
   Verify the recorded runner and boot IDs. Check that the daemon's private
   `TART_HOME` has no local clone and GitHub has no stale runner registration.
2. Run another job. Use `launchctl kill SIGKILL "gui/$(id -u)/com.illusions-lab.ephemera.dev"`
   while the job is running. Check logs for `Recovering`, `PreparingImage`,
   `Connecting`, and `Healthy` in order. Verify clone and registration
   cleanup before image preparation. Dispatch another job and verify that
   the restarted daemon resumes work.
3. Compare the pinned source image's disk hash before and after the tests.
   Inspect the instance records, daemon logs and process arguments to
   confirm they contain no JIT credential. Keep raw runner output out of the
   host logs.

For an offline guest test, set `EPHEMERA_TART_SMOKE_HOME` to a private Tart
directory and `EPHEMERA_TART_SMOKE_IMAGE` to the pinned reference in the
example config. Use `go test ./internal/vm -run TestTartGuestSmoke -v -timeout 20m`.
This test uses a synthetic credential and cleans up the disposable clone.
