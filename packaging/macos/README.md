# macOS runner installation and acceptance

Use macOS 26 or newer on Apple Silicon. Run the installer in Terminal as the
account that will run Ephemera. The installer manages Tart 2.42.0 and stores
the release, GitHub App credentials, VM state, and logs under
`~/Library/Application Support/Ephemera`. It places `ephe` and `ephemera` in
`~/.local/bin` and installs `com.illusions-lab.ephemera` in `~/Library/LaunchAgents`.
The agent runs while that account is logged in, including after it logs
back in after a restart. FileVault may require an unlock before login.

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
Interactive setup defaults to `runner.execute: false` with a concurrency of
one. To enable execution, edit `config.yaml` and run `ephe setup` to apply it.
Use `ephe doctor`, `ephe status`, and `logs/daemon.log` to inspect the service.

Rerun the installer to upgrade, or pass `--version TAG` to select a specific
release. If activation fails, it restores the previous binary and LaunchAgent.
It retains the configuration and private key for retries. Retry an unattended
installation with the same inputs.

For a local build, run `make bundle-macos`, place Tart 2.42.0 at
`~/Library/Application Support/Ephemera/tools/tart.app`, and run
`/bin/bash scripts/install-macos.sh --bundle bin/macos-bundle`. Ensure the
Ephemera directory has mode `0700`. The bundle skips installer downloads.
Setup still needs GitHub access and may fetch the configured VM image.

## Development without the installer

Run the daemon as your logged-in macOS user on Apple Silicon. The macOS
guest uses a separate, non-admin `ephemera-runner` account for jobs. Use a
dedicated test Scale Set and trusted workflows.

Build with `make build` and put Tart 2.42.0 on your PATH. Create a private
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
[LaunchAgent](com.illusions-lab.ephemera.dev.plist) with absolute paths. Stop
the foreground daemon and register the plist with
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
   while the job is running. Check that recovery runs before work intake
   resumes and that clone and registration cleanup succeeds. Dispatch one
   more job to verify that the restarted daemon resumes work.
3. Compare the pinned source image's disk hash before and after the tests.
   Inspect the instance records, daemon logs and process arguments to
   confirm they contain no JIT credential. Keep raw runner output out of the
   host logs.

For an offline guest test, set `EPHEMERA_TART_SMOKE_HOME` to a private Tart
directory and `EPHEMERA_TART_SMOKE_IMAGE` to the pinned reference in the
example config. Use `go test ./internal/vm -run TestTartGuestSmoke -v -timeout 20m`.
This test uses a synthetic credential and cleans up the disposable clone.
