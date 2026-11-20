# Ephemera

**Ephemeral runners. Zero hassle.**

An ultra-lightweight, self-hosted control plane for ephemeral, stateless GitHub Actions runners.

All you need are two things: **a GitHub App and a server to run your runners.**

Ephemera takes care of the rest: provisioning virtual machines, starting GitHub Actions runners, managing their lifecycle, and destroying them when jobs finish.

**Every job starts fresh. Nothing to clean up.**

> **Project Status:** Under active development. Features described below represent the intended architecture and may not yet be available.

## Why Ephemera?

Self-hosted GitHub Actions runners shouldn't be complicated.

You shouldn't need a Kubernetes cluster, a fleet of permanently running machines, or a complicated infrastructure setup just to execute a few build jobs.

Ephemera is designed to make self-hosted runners as effortless as possible.

- **Zero hassle** — Connect a GitHub App, configure your host, and let Ephemera handle runner provisioning.
- **Truly ephemeral** — Every job runs in a fresh virtual machine that is destroyed after execution.
- **Cross-platform** — Support for Linux, Windows, and macOS, across AMD64 and ARM64 where supported by the host and virtualization backend.
- **Lightweight by design** — No Kubernetes or heavyweight orchestration platform required.
- **Automatic scaling** — Provision runners on demand, based on GitHub Actions workloads.
- **Golden images** — Reuse preconfigured VM images for fast, reproducible environments.
- **Self-hosted** — Your machines, your infrastructure, your control.
- **GitHub-native** — Built around GitHub Runner Scale Sets and GitHub App authentication.

## How It Works

```text
                    GitHub Actions
                          |
                          v
                  GitHub Runner Scale Set
                          |
                          v
                  +------------------+
                  |     Ephemera     |
                  |   Control Plane  |
                  +------------------+
                          |
                  +-------+-------+
                  |               |
                  v               v
             Linux Host       macOS Host
                  |               |
               QEMU/KVM          Tart
                  |               |
             +----+----+     +----+----+
             |         |     |         |
           Linux    Windows macOS    Linux
            VM        VM      VM       VM
             |         |       |       |
             +---------+-------+-------+
                          |
                    Execute Job
                          |
                    Destroy VM
```

1. **A job is requested.** GitHub Actions assigns a job to an Ephemera-managed Runner Scale Set.
2. **A VM is provisioned.** Ephemera selects an available host and creates a fresh VM from a golden image.
3. **A runner is registered.** A short-lived, just-in-time GitHub Actions runner configuration is injected into the VM.
4. **The job runs.** The runner executes the assigned GitHub Actions job.
5. **The VM disappears.** Once the job finishes, Ephemera destroys the VM and its temporary state.

No persistent runner environments. No leftover build artifacts. No manual cleanup.

## Platform Support

Ephemera is designed to support multiple operating systems and CPU architectures through interchangeable virtualization backends.

| Guest OS | AMD64 | ARM64 | Backend |
|---|---|---|---|
| Linux | Planned | Planned | QEMU/KVM, Tart |
| Windows | Planned | Experimental target | QEMU |
| macOS | Not currently targeted | Planned | Tart |

**Notes:**

- QEMU/KVM provides hardware-accelerated virtualization when the guest and host architectures are compatible.
- Cross-architecture execution is possible through QEMU emulation, but performance may be significantly lower.
- macOS virtualization through Tart requires an Apple Silicon Mac.
- Platform support depends on available host hardware, guest images, and backend capabilities.

## Architecture

Ephemera follows a minimal, modular architecture with two primary components.

### Control Plane

Responsible for:

- GitHub App authentication
- Runner Scale Set management
- Job demand monitoring
- Runner scheduling and capacity management
- VM lifecycle coordination
- Cleanup and failure recovery

### Host Agent

A lightweight agent running on each virtualization host.

Responsible for:

- Managing local virtualization backends
- Provisioning VMs from golden images
- Injecting runner configurations
- Monitoring VM state
- Destroying completed or abandoned runner environments

The control plane and host agent are designed to run on the same machine or across multiple hosts.

### Virtualization Backends

**QEMU/KVM**

Provides Linux and Windows virtual machines, including disk copy-on-write support and cross-architecture emulation.

**Tart**

Provides lightweight macOS and Linux ARM64 virtual machines on Apple Silicon.

The backend architecture is designed to remain extensible as additional virtualization technologies become available.

## Design Philosophy

Ephemera follows a few simple principles:

**Keep it small.** A runner manager should not require an entire infrastructure platform.

**Keep it disposable.** Runner environments are temporary by default.

**Keep it reproducible.** Every job should start from a known, clean image.

**Keep it compatible.** Use existing virtualization tools and GitHub's official APIs instead of reinventing them.

**Keep it yours.** Run your workloads on your own hardware without depending on a third-party compute provider.

## Built On

Ephemera builds upon established open-source technologies:

- [GitHub Actions Runner Scale Set](https://github.com/actions/scaleset) — GitHub Actions runner orchestration and autoscaling APIs.
- [QEMU](https://github.com/qemu/qemu) — Full-system virtualization and hardware emulation.
- [Tart](https://github.com/cirruslabs/tart) — macOS and Linux virtualization on Apple Silicon.

## Installation

**Ubuntu 24.04 amd64 with systemd is the primary supported host.** Other
Linux distributions have not been verified to the same level.

Install a release and register the systemd service:

```bash
sudo bash scripts/install.sh
sudo ephe setup
```

The installer verifies the release checksum and installs both binaries and the
`ephe` alias. Releases are stored under `/opt/ephemera/releases`; the active
version is selected by `/opt/ephemera/current`. Check the installed version
with `ephe version` (or `ephemerad --version`).

```sh
ephe update --check       # show current, staged, and latest versions
sudo ephe update          # download and stage a newer release
sudo ephe update --apply  # switch and wait for the service to become ready
```

Staging leaves the service running. If activation fails, the installer restores
the previous version and service state. An interrupted activation is recovered
on the next installation or update operation. Configuration, the GitHub App key,
and the image cache are retained. To stage a specific release, including an older
one, use `sudo bash install.sh --version v0.1.0` and follow the printed apply
command. `--bundle DIR` supports offline and development installations.

Use `--version TAG` to pin a release or `--bundle DIR` to use
a local bundle. For an Ubuntu amd64 bundle from source, run
`make bundle-linux` and pass `--bundle bin/bundle`. The service currently
observes GitHub job demand. Running jobs in ephemeral VMs is still pending.

For automated deployment, provide a configuration and private key. This
mode applies the configuration and starts the service during installation:

```bash
sudo bash scripts/install.sh --non-interactive \
  --config /path/to/config.yaml --github-app-key /path/to/github-app.pem
```

On repeat installs, omit the configuration and key arguments to reuse
the installed files. Interactive installs leave service activation to
the setup wizard. Non-interactive upgrades restart the service.

## Setup and image selection

`sudo ephe setup` and `sudo ephemera setup` open the same wizard.

- **Edit configuration** creates or edits `/etc/ephemera/config.yaml`.
  Review the settings summary before saving.
- **Prepare a golden image** builds and checks an image independently of
  the running service. After success, choose whether to use it in your
  saved configuration.
- **Apply configuration and start or restart service** activates the
  saved configuration after confirmation.

The image menu starts with **Ephemera presets**. On first use it
preselects Ubuntu amd64. Later visits start with your configured image
and architecture. The presets include Ubuntu and Windows for amd64 and
arm64. Ubuntu amd64 is the implemented recipe.

Open **GitHub runner images** to browse the bundled snapshot of the
official image catalog, including aliases for Ubuntu, Windows, macOS and
Xcode. Select an entry to see whether it can be prepared. The Ubuntu
24.04 amd64 entry and its `ubuntu-latest` alias use Ephemera's Ubuntu
recipe, which installs the GitHub runner and its dependencies. Matching
the full hosted runner toolset requires a separate recipe.
[GitHub's official image catalog](https://github.com/actions/runner-images#available-images)

The image tag remains version-free, for example `ubuntu` with
`arch: amd64`. When you save the GitHub catalog selection, Ephemera
records the resolved profile and pinned source in the configuration.
Preparing or saving an image keeps the current service running.

For command-line image preparation, run `sudo ephemera image prepare`
on an Ubuntu amd64 host with the [image build tools](#runner-images-and-readiness)
installed.

## Development

Run `make` for the target list. `make build` builds the local CLI and daemon,
`make build-linux` builds both Linux amd64 binaries, and `make check` runs
Go tests, vet, shell syntax checks, ShellCheck, and configuration
validation. Use `make config-check CONFIG=/path/to/config.yaml` to
validate a specific file.

The [example configuration](configs/ephemera.example.yaml) describes the
intended Linux service layout. The service will read
`/etc/ephemera/config.yaml` by default, store persistent state under
`/var/lib/ephemera`, and write logs to the systemd journal. Keep the GitHub App
key in `/etc/ephemera/credentials/github-app.pem` with root-only access for
systemd to load.

The Debian smoke test requires `qemu-system-x86_64`, `qemu-img`,
`cloud-localds` (from `cloud-image-utils` on Debian), Python 3, and the
OpenSSH client. HTTPS downloads also require curl. Use a versioned
[Debian genericcloud amd64 qcow2 image](https://cloud.debian.org/images/cloud/)
and the matching SHA-512 from its checksum file.

```bash
make vm-boot DEBIAN_IMAGE="$DEBIAN_IMAGE" DEBIAN_SHA512="$DEBIAN_SHA512"
make vm-test DEBIAN_IMAGE="$DEBIAN_IMAGE" DEBIAN_SHA512="$DEBIAN_SHA512"
make vm-debug DEBIAN_IMAGE="$DEBIAN_IMAGE" DEBIAN_SHA512="$DEBIAN_SHA512"
```

`DEBIAN_IMAGE` can be a local path or an HTTPS URL. The script caches the
verified base image and creates a fresh writable disk and SSH key per run.
It checks Debian boot, cloud-init, SSH, and systemd. On hosts without usable
x86-64 KVM, QEMU uses software emulation. Pass `--keep` to retain the VM
for debugging, or use `make vm-debug` to run the installer smoke test and
print the SSH command for the retained VM.

`make vm-test` builds the local setup CLI and exercises the
installer with a systemd fixture unit. The test generates a disposable
RSA key, installs twice, checks that the config and key are preserved,
and verifies that systemd can restart the unit. For an externally
built bundle, pass `--bundle DIR` containing Linux amd64 `ephemera` and
`ephemerad` binaries, a test `ephemera.service`, and a secret-free `config.yaml`.

### Test GitHub job intake

Install a GitHub App with access to a dedicated test repository and
populate the GitHub fields in a copy of the example configuration.
Build the CLI and start an observation session against that Scale Set:

```bash
make build
bin/ephemerad --config ./test-config.yaml \
  --github-app-key /path/to/github-app.pem --create
```

`--create` registers the configured Scale Set in the default runner group
if it is missing. Later runs can reuse it. The listener uses outbound HTTPS
and stops on Ctrl+C. Observation advertises zero runner capacity and leaves
job offers unclaimed. Add `--claim` only for a dedicated test Scale Set with
`runner.max_concurrent: 1`. Trigger a workflow whose `runs-on` matches
`github.scale_set_name` and look for `Job acquired` and `Runner demand` in the
logs. Cancel the test workflow after confirming the intake because this
command has no runner backend.

### Runner images and readiness

Set `vm.image` to `ubuntu` or `windows`, and `vm.arch` to `amd64` or
`arm64`. The first implemented recipe is `ubuntu/amd64`. The remaining
combinations are recognized and stop startup with an explicit error until
their build and boot recipes are available. For older configs, add
`arch: amd64` and replace the old image ID with `ubuntu` to use the
managed image.

The recipe uses pinned Ubuntu 24.04 and GitHub runner releases. Override
both source pins under `image.ubuntu.amd64` with `base_url`,
`base_sha256`, `runner_version`, and `runner_sha256`. The image tag and
the Scale Set name can stay the same after a source update.

On an Ubuntu amd64 host, install the build tools, then prepare and test
the image independently of GitHub:

```bash
sudo apt install qemu-system-x86 qemu-utils guestfs-tools cloud-image-utils
make image-prepare
```

The preparation step verifies both downloads, assembles a standalone
qcow2, and boots a disposable overlay to check the runner binary and
systemd unit. Each job VM will start the guest unit after receiving its
one-time JIT config at `/run/ephemera/jitconfig`. The listener caches validated disks under
`/var/lib/ephemera/image`. The Make target uses `bin/image` for local
development. Override its directory with `IMAGE_CACHE=/path/to/cache`.

`ephemerad` prepares the selected image before opening the GitHub
session. It reports `Healthy` after reading the initial session
statistics and checking that the disk is available. Under a systemd
`Type=notify` service it also sends `READY=1` at that point. Allow more
than the 30-minute image preparation deadline in `TimeoutStartSec`. A failed
build or a missing cached disk reports `Degraded` and stops the listener
so a service supervisor can restart it. The `--claim` option still
requires an isolated test Scale Set.

The default Scale Set name is `ephemera-ubuntu-amd64`. GitHub documents
multiple labels for Scale Sets and separately calls out that ARC does
not support the standard `self-hosted` label. Existing
`runs-on: self-hosted` workflows should be tested against Ephemera's
Scale Set before relying on that routing.
[GitHub runner selection documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-where-workflows-run/choose-the-runner-for-a-job)

## Roadmap

- [x] GitHub App authentication
- [x] GitHub Runner Scale Set integration for job intake
- [ ] Ephemeral runner lifecycle management
- [ ] QEMU/KVM backend (image boot check implemented; job VM lifecycle pending)
- [ ] Linux AMD64 runner support
- [ ] Windows AMD64 runner support
- [ ] Golden image management (Ubuntu AMD64 preparation and caching implemented; end-to-end boot verification pending)
- [ ] Copy-on-write VM provisioning
- [ ] Automatic cleanup and crash recovery
- [ ] Remote host agents
- [ ] Tart backend
- [ ] macOS ARM64 runner support
- [ ] Linux ARM64 runner support
- [ ] Multi-host scheduling
- [x] Configurable concurrency for job intake
- [ ] Runner pools
- [ ] Optional VM snapshot and restore acceleration

GitHub authentication and job intake still need verification against a live Scale Set.

## License

Apache License 2.0.

See [LICENSE](LICENSE) for details.

---

**Ephemera — Run. Finish. Vanish.**

Before the first production release, verify the release bundle on Ubuntu amd64 with a real systemd service and Ubuntu image preparation, then set the GitHub Actions repository variable `EPHEMERA_SYSTEMD_VERIFIED` to `true`. The release job remains blocked until this acceptance test is complete. Production tags and releases are created only by GitHub Actions.
