# Ephemera architecture

Ephemera has two entry points. Administrators use `ephemera` (also available
as `ephe`) to configure the installation and prepare runner images. systemd
runs `ephemerad` to maintain the connection to GitHub and manage job execution.

The daemon starts in observation mode by default. Linux amd64 and arm64 hosts with KVM can opt in to ephemeral Ubuntu runner execution.

## Repository layout

```text
cmd/
  ephemera/            CLI entry point
  ephemerad/           Daemon entry point, startup flags, and process signals
internal/
  cli/                 Command dispatch and one file per command group
    setup/             Interactive wizard and its image catalog
  config/              Configuration schema, defaults, validation, and file I/O
  service/             Daemon startup, readiness, and coordination
  image/              Image sources, assembly, boot verification, and cache
  orders/              GitHub Scale Set authentication, polling, and job acquisition
  status/              Read-only daemon snapshots over a local Unix socket
  vm/                  Per-job VM creation, monitoring, and cleanup
configs/
  ephemera.example.yaml
packaging/
  ephemera.service     systemd unit with ephemerad as ExecStart
scripts/
  install.sh           Installs both programs and registers the unit
  dev-vm.sh            Debian installation smoke test
```

The entry points call into their respective packages. The CLI uses
`config` and `image` and asks systemd to apply service changes.
The daemon uses `service`, which coordinates `config`, `image`, and
`orders`, with `vm` joining when job execution is implemented.
The shared packages serve both entry points independently of menus,
command-line flags, and systemd. For example, an image built through the
setup wizard follows the same verification and caching rules as an image
prepared at daemon startup.

## Administrator workflow

1. The installer puts both binaries in `/usr/local/bin`, creates the
   `ephe` alias, registers `ephemera.service`, and reloads systemd.
2. `sudo ephe setup` opens the same wizard as `sudo ephemera setup`.
   Administrators can edit existing settings, review them before saving,
   and build a golden image from the presets or GitHub image catalog.
3. Preparing an image verifies it and saves it to the local cache.
   Saving settings updates `/etc/ephemera/config.yaml`. An explicit
   apply action starts or restarts the service through systemd, so
   administrators can choose when to interrupt a running service.

`ephe config validate` and `ephe image prepare` expose the same validation
and image preparation for scripted use.

On Apple Silicon, `install-macos.sh` verifies and stages the release, then
runs the staged `ephemera setup`. Both the installer and direct setup take
the same activation lock and recover any previous interrupted switch.
Setup verifies the GitHub App, validates the configuration, and prepares
the image before stopping the previous agent. A private per-activation
directory supplies the YAML and PEM used by the LaunchAgent.

The transaction records the previous plist, `current` target, and loaded
state before switching the agent. The candidate plist runs the release's
absolute daemon path. Setup commits `current` after the candidate reports
`Healthy` with the activation ID, GitHub target, and intake mode. Bootstrap
failure, cancellation, and timeout restore the previous service. An
interrupted rollback leaves the recovery journal available to the next setup.

Each Darwin archive includes the Tart manifest embedded in both binaries.
The installer keeps verified downloads by version and digest. Release
directories bind their Tart binary so the restored agent uses its own
dependency. The power profile is managed independently by
`server-config.sh`.

## Daemon lifecycle

1. Load the configuration. In execution mode, lock the instance directory
   and enter `Recovering`. The daemon checks persisted instance identities
   against the backend, resource directory, and GitHub scope and Scale Set.
   It stops old VMs, removes their local resources and GitHub registrations,
   and deletes each record only after all cleanup steps succeed. Failed
   cleanup is retried with backoff while intake is closed.
2. Enter `PreparingImage`. Check the backend and prepare and validate the
   configured runner image. A failure in this stage reports
   `Degraded during PreparingImage`. Linux images are keyed by platform and
   architecture, with source versions and checksums identifying the build.
3. Enter `Connecting` and open the GitHub Scale Set session. After GitHub
   returns initial statistics, report `Healthy`, notify systemd with
   `READY=1`, and open execution capacity. Observation skips instance
   recovery and continues to advertise zero capacity.

The host layer owns configuration, credential, cache and state defaults,
service status, and socket lifecycle. On macOS the daemon takes the state
lock before checking for and reclaiming a stale socket, including in
observation mode. On Linux the packaged systemd unit supplies the socket at
`/run/ephemera/status.sock`. The read-only socket reports startup phases
and the latest GitHub demand after the first statistics update.

## Runner execution design

The first execution release runs one daemon, one Scale Set, and one selected
backend per host. `orders` owns the GitHub session, acquisition, and runner
registration APIs. A reconciler in `service` owns desired capacity and local
runner lifecycles. `vm` owns host-specific provisioning and cleanup. Different
guest profiles can be exposed through separate Scale Sets and `runs-on` names.
A remote host agent can use the same lifecycle contract in a later release.

### Scaling and assignment

1. Verify the selected base image, backend availability, and local capacity.
   Recover resources from previous daemon runs before opening capacity to
   GitHub. Advertise locally available capacity within
   `runner.max_concurrent`.
2. Acquire eligible job offers within that capacity where the Scale Set
   protocol requires it. Bound acquisitions using GitHub's current occupied
   counts and any acquisitions still awaiting confirmation from newer
   statistics. Use initial session statistics and subsequent
   `TotalAssignedJobs` to compute the desired runner count, capped at the
   effective capacity. GitHub assigns pending jobs to registered runners in
   the Scale Set. An acquired request ID identifies an offer, and the local
   VM is keyed by its own runner instance ID.
3. Record the latest demand and runner events before acknowledging the
   message. Reconcile asynchronously so slow VM operations cannot stall
   polling. Reconcile again on local lifecycle changes and after session
   recovery. Handle duplicate updates without starting duplicate VMs.
4. Count provisioning, waiting, and busy instances against capacity. Keep
   a slot reserved during cleanup until the guest is confirmed stopped.
   On lower demand, allow busy runners to finish. For the first release,
   let waiting runners expire on a bounded idle timeout rather than
   racing GitHub assignment with an immediate stop.

[GitHub Scale Set client v0.4.0](https://github.com/actions/scaleset/tree/v0.4.0)

### VM and runner lifecycle

1. Reserve a slot and persist the instance ID, backend, base image identity,
   and intended resource names before creating guest resources.
2. Prepare private writable guest storage from the verified base. Generate
   a fresh JIT configuration close to guest startup. Persist the GitHub
   runner ID returned by the API when available, and keep the JIT payload
   out of durable instance records, status, and logs.
3. Deliver the JIT payload to the guest through the backend's bootstrap
   mechanism. The guest starts a single GitHub runner under a dedicated
   account. Keep the GitHub App key on the host. Use bounded deadlines
   for provisioning, boot, registration, and idle waiting.
4. Track the VM process and correlate GitHub runner events by runner
   identity. Record workflow outcomes from GitHub. Use guest exit or a
   terminal runner event followed by a bounded grace period to initiate
   teardown. A lost VM triggers cleanup and reconciliation against fresh
   demand.
5. Stop the VM, remove its writable storage and bootstrap material, and
   reconcile any stale GitHub runner registration. Cleanup must tolerate
   already-removed resources. Keep failed cleanup records for retry and
   scope recovery to resources owned by this daemon. On shutdown, stop
   taking new work and allow a bounded drain before terminating remaining
   guests. On restart, the first release cleans up leftover instances
   before admitting work.

### Backend contract

The shared lifecycle needs operations to check host and image compatibility,
prepare a named instance, boot it with one-runner bootstrap data, observe its
termination, and stop and delete it by stable identity after a restart.
Backend operations must be cancellable or time-bounded. Each adapter owns its
storage format, launch mechanism, and secret delivery. Images declare the
backend, guest OS, architecture, and pinned source that they support.

| Backend | First target | Storage and bootstrap |
|---|---|---|
| QEMU | Linux amd64 or arm64 host with KVM and a matching Ubuntu guest | Create a qcow2 overlay and a private per-instance cloud-init seed. Reuse the Ubuntu image's systemd runner bootstrap. Remove the seed with the instance. ARM64 uses read-only AAVMF firmware, private per-instance UEFI variables, and a dedicated virtio console for readiness. |
| Tart | Apple Silicon macOS host, macOS arm64 guest | Clone a pinned Tart image into the daemon’s private Tart home. Provision the runner account through the Guest Agent and deliver JIT on stdin. Emit ready when the runner begins listening. The development LaunchAgent runs as the logged-in macOS user. |

Place bootstrap artifacts in private per-instance storage and expose only
what the guest needs. Guest networking must reach GitHub and respect the
host's isolation policy. Tart Linux arm64 and additional QEMU guests can use
the same lifecycle once their images and bootstrap paths are verified.
Backend selection centralizes host and guest compatibility and image operations.
The daemon, setup, and `ephe image prepare` share preparation logic.
`ephe doctor` checks availability without pulling or rebuilding images.
The reconciler works through the existing `vm.Backend` interface.

[Tart VM management](https://tart.run/quick-start/) · [Tart Guest Agent](https://tart.run/blog/2025/06/01/bridging-the-gaps-with-the-tart-guest-agent/)

## Implementation and verification

1. `make check` runs the fake GitHub session and backend tests, including
   overlapping jobs, replay, disconnects, registration failures, and recovery.
2. The Ubuntu VM CI workflow builds the image and verifies its offline
   cloud-init and systemd boot path. Use the dedicated Scale Set workflow
   and the [host verification steps](README.md#test-qemu-execution) for live
   job execution and cleanup acceptance.
3. On Apple Silicon, use the intended daemon account and a private
   `TART_HOME` to test a pinned Tart version and OCI image digest.
   Clone, start headless, wait for the Guest Agent, deliver JIT through
   `tart exec -i` via stdin, wait for runner exit, then stop and delete the
   clone. Run the lifecycle tests with a macOS workflow and check that a
   launchd restart reclaims a leftover clone before enabling Tart support.

`make bundle-linux` stages both executables and the systemd unit.
`make vm-test` exercises installation and restart with an offline fixture.
Production releases are published through GitHub Actions.
