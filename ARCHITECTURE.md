# Ephemera architecture

Ephemera has two entry points. Administrators use `ephemera` (also available
as `ephe`) to configure the installation and prepare runner images. systemd
runs `ephemerad` to maintain the connection to GitHub and manage job execution.

The daemon currently starts in observation mode. Per-job VM execution is planned.

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
  vm/                  Per-job VM creation, monitoring, and cleanup (planned)
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

## Daemon lifecycle

1. Load and validate the saved configuration. Read the GitHub App private
   key through systemd's credential mechanism.
2. Verify the configured image in the cache or assemble and boot-test it.
   The configuration identifies the platform by a stable tag such as
   `ubuntu` and a separate architecture such as `amd64`. Source versions
   and checksums identify the actual build. The image cache lives under
   `/var/lib/ephemera/image/`.
3. Open a GitHub Scale Set session and read its initial state.
   Once the image and session are ready, report `Healthy` and notify
   systemd with `READY=1`. A startup failure reports `Degraded` and exits
   so systemd can supervise the restart.
4. Poll GitHub for demand. The current implementation observes job demand
   by default and supports acquisition in an explicit test mode. When
   the VM backend is ready, the service will create a writable overlay
   for each job VM, inject its one-time runner configuration, and remove
   the VM and its writable disk when the job finishes.

## Delivery and verification

`make bundle-linux` stages both executables and the systemd unit.
`make vm-test` exercises installation and restart with an offline fixture.
The service tests cover daemon readiness and failures independently of a
live GitHub account. Windows recipes and the per-job VM backend are
future work. Production releases are published through GitHub Actions.
