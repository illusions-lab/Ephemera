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

## Roadmap

- [ ] GitHub App authentication
- [ ] GitHub Runner Scale Set integration
- [ ] Ephemeral runner lifecycle management
- [ ] QEMU/KVM backend
- [ ] Linux AMD64 runner support
- [ ] Windows AMD64 runner support
- [ ] Golden image management
- [ ] Copy-on-write VM provisioning
- [ ] Automatic cleanup and crash recovery
- [ ] Remote host agents
- [ ] Tart backend
- [ ] macOS ARM64 runner support
- [ ] Linux ARM64 runner support
- [ ] Multi-host scheduling
- [ ] Runner pools and configurable concurrency
- [ ] Optional VM snapshot and restore acceleration

## License

Apache License 2.0.

See [LICENSE](LICENSE) for details.

---

**Ephemera — Run. Finish. Vanish.**
