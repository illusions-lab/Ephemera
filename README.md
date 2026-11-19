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

**We recommend Debian with systemd. We have not tested installation on
operating systems other than Debian.**

`scripts/install.sh` is the one-command installation entry point. It
downloads the release for your architecture, checks its SHA-256 digest,
and opens the setup wizard on first install. The wizard asks for your
GitHub repository or organization URL, scale set name, GitHub App and
installation IDs, private key file, and concurrency limit. The wizard
selects the currently implemented runner image, Ubuntu amd64.
You can optionally pin the release with `--version TAG`.

```bash
sudo bash scripts/install.sh
```

For automated deployment, supply your own configuration and private key.
Use `--bundle DIR` to install an unpacked release instead of downloading it.

```bash
sudo bash scripts/install.sh --non-interactive \
  --config /path/to/config.yaml --github-app-key /path/to/github-app.pem
```

Repeat installs reuse the installed configuration and credential. They
restart the service, so schedule upgrades between jobs. To change settings,
edit `/etc/ephemera/config.yaml` and run `ephemera config validate` before
restarting the service.

Release installation will be available once the control-plane daemon and
its production unit are published. The installer expects a GitHub Actions
release containing `checksums.txt` and an
`ephemera_linux_<arch>.tar.gz` with `ephemera` and `ephemera.service` at
its root. The setup CLI and installer can already be exercised locally
with the Debian smoke test below.

## Development

Run `make` for the target list. `make build` builds the local CLI,
`make build-linux` builds a Linux amd64 binary, and `make check` runs
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
built bundle, pass `--bundle DIR` containing a Linux amd64 `ephemera`
binary, a test `ephemera.service`, and a secret-free `config.yaml`.

### Test GitHub job intake

Install a GitHub App with access to a dedicated test repository and
populate the GitHub fields in a copy of the example configuration.
Build the CLI and start an observation session against that Scale Set:

```bash
make build
bin/ephemera orders listen --config ./test-config.yaml \
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
both source pins under `images.ubuntu.amd64` with `base_url`,
`base_sha256`, `runner_version`, and `runner_sha256`. The image tag and
the Scale Set name can stay the same after a source update.

On a Debian amd64 host, install the build tools, then prepare and test
the image independently of GitHub:

```bash
sudo apt install qemu-system-x86 qemu-utils guestfs-tools cloud-image-utils
make image-prepare
```

The preparation step verifies both downloads, assembles a standalone
qcow2, and boots a disposable overlay to check the runner binary and
systemd unit. Each job VM will start the guest unit after receiving its
one-time JIT config at `/run/ephemera/jitconfig`. The listener caches validated disks under
`/var/lib/ephemera/images`. The Make target uses `bin/images` for local
development. Override its directory with `IMAGE_CACHE=/path/to/cache`.

`orders listen` prepares the selected image before opening the GitHub
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
