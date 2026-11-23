# Local trial of GitHub's official Runner image recipes

Use a disposable Apple Silicon build VM and a private, separate Tart home.
Leave the installed Ephemera service and its active image out of this trial.
A locally rebuilt image can become an Ephemera candidate after the full
upstream recipe, software inventory, and workload checks pass.

**Image source policy.** For macOS and Linux guests, use the official GitHub
[`actions/runner-images`](https://github.com/actions/runner-images) repository
as the source of the image build recipe. Pin the upstream revision and build
the golden image on the machine that will run the VMs. Third-party prebuilt
images and third-party image recipes are prohibited. OS installation media
and vendor tools required by the GitHub recipe must come from their official
publishers.

## macOS 26 ARM64

The reference is GitHub's [macOS 26 ARM64 20261117.0516 release](https://github.com/actions/runner-images/releases/tag/macos-26-arm64/20261117.0516)
at commit `6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab`. Use its
[software inventory](https://github.com/actions/runner-images/blob/6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab/images/macos/macos-26-arm64-Readme.md)
to select the macOS release and compare the finished guest. Retain the upstream
commit, Apple download provenance, archive hashes, and build and test logs.

1. Obtain the Apple macOS 26.7.2 (25G332) restore image listed in this
   release and the Xcode archives in the upstream ARM64 toolset. If Apple
   supplies this build only as an update, install a compatible official IPSW,
   apply the official update inside the VM, and verify `sw_vers -buildVersion`
   before provisioning. For each Xcode, put Apple's original archive in a
   private directory with the filename `Xcode-<version+build>.xip`
   from the toolset. Check all archives before booting a build VM.
   [Apple restore image instructions](https://developer.apple.com/documentation/virtualization/installing-macos-on-a-virtual-machine)
   · [Apple developer downloads](https://developer.apple.com/download/all/)
   · [GitHub Xcode version and hash list](https://github.com/actions/runner-images/blob/6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab/images/macos/toolsets/toolset-26.json)

   ```sh
   bash scripts/check-official-macos-inputs.sh /path/to/Apple.ipsw /path/to/xcode-archives
   ```

2. Use a separately installed, verified Tart binary. Set `TART_HOME` to a fresh
   mode-0700 directory. Create a vanilla VM with
   `tart create github-macos26-provision --from-ipsw /path/to/Apple.ipsw --disk-size <GB>`.
   Size guest and host storage for all seven Xcode installations, simulator
   runtimes, and a clean rebuild. Run it with `tart run github-macos26-provision`.
   Complete Setup Assistant, create a disposable admin account, enable Remote
   Login for that account, and confirm the host can reach it over SSH and that
   the guest can reach required download endpoints. Match the guest OS version
   and build to the upstream inventory. Keep its SSH access private and use
   only disposable credentials.
3. Check out the pinned GitHub commit in a separate directory. Run Packer
   against `images/macos/templates/macOS-26.arm64.anka.pkr.hcl`. Use
   `builder_type=null` to provision the running Tart guest over SSH. Supply
   `source_vm_name` as the guest IP, `source_vm_port` as its SSH port,
   `vm_username` and `vm_password` as its disposable credentials, and
   `build_id` as a unique trial identifier. Supply
   `xcode_install_storage_url` as a private HTTP(S) endpoint reachable from the
   guest, with a trailing slash, serving the verified filenames. For a private
   endpoint with no query token, supply an empty `xcode_install_sas`.
   Use Packer's `PKR_VAR_...` environment variables for sensitive inputs.
   Run `packer init`, `packer validate`, then `packer build` against the same
   pinned template. Save the full build log. Resolve any service-specific
   dependency before proceeding if an upstream provisioner fails.
   [GitHub Packer template](https://github.com/actions/runner-images/blob/6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab/images/macos/templates/macOS-26.arm64.anka.pkr.hcl)
4. Save the upstream-generated software inventory and compare it with the
   reference. Investigate missing tools and version differences. Keep any
   Ephemera guest integration as a separate provisioning step, including
   support for `tart exec` and the official Actions Runner. Validate both
   `xcrun --sdk macosx --show-sdk-path` and
   `xcrun --sdk iphoneos --show-sdk-path` as the Runner account. Shut down
   the provisioned guest and clone it for tests. Keep the base image private.

### Build on the Ephemera host

Run the packaging module as the Ephemera service account on the deployment
host. It uses the account's private Tart home, creates a unique VM from the
Apple restore image, and runs GitHub's pinned template against that VM.

```sh
bash scripts/build-official-tart-image.sh create "$APPLE_IPSW" "$XCODE_ARCHIVES" "$GUEST_DISK_GIB"
```

Start the named guest with the same `TART_HOME`. Enable SSH and install the
[Tart Guest Agent](https://github.com/openai/tart-guest-agent) with RPC and
non-interactive sudo support. Use only temporary provisioning credentials
and keep the archive endpoint on a private network. Stage only the verified
archives at that endpoint and set the Packer inputs described above.

```sh
bash scripts/build-official-tart-image.sh provision "$GUEST_NAME" "$APPLE_IPSW" "$XCODE_ARCHIVES" "$UPSTREAM_CHECKOUT"
```

The module records the pinned GitHub revision after Packer and the SDK
checks pass and shuts down the template. After inventory and acceptance
review, enter the reported `local:github-macos26-...` reference in
`ephe config`, enable Xcode and iOS SDK verification and run
`ephe config apply`. This reference uses a local image and disables the
bundled image channel for that configuration. Confirm the active
configuration, run a real job and verify its image before retiring older
third-party templates.

### Trial with an installed Apple Xcode

When the build will use Xcode already installed on its host, add
`--host-xcode auto` or `--host-xcode /Applications/Xcode.app` to the
Ephemera macOS installer. This preflight prints the installed version and
build and stops before activation if it cannot verify a complete Apple
Xcode installation. Pass `--xcode-version` and `--xcode-build` when the trial
requires an exact version. Existing verified images can be installed without
this option.

Start a disposable `xcode-trial-*` macOS VM in a separate Tart home with a
working guest agent and non-interactive sudo. With the intended Apple Xcode
version and build, run the copy step as part of preparing the VM.

```sh
ephe image check-host-xcode --version 27.2 --build 27B52
TART_HOME="$TRIAL_TART_HOME" bash scripts/copy-host-xcode-to-tart.sh xcode-trial-27 27.2 27B52
```

The copy streams the verified bundle to guest storage, verifies its signature
and build there, runs first-launch provisioning, and checks that both SDK
paths exist. It leaves a standalone installation at `/Applications/Xcode.app`
inside the guest. The trial is suitable for testing Apple build compatibility.
The full GitHub image recipe still validates its own complete toolset and
archive checksums before it records a GitHub build receipt.

## Ubuntu ARM64 under the same Mac

Use GitHub's [Ubuntu 24.04 ARM64 20261116.188 release](https://github.com/actions/runner-images/releases/tag/ubuntu24-arm64/20261116.188)
at commit `74456c0b4eff7aa68e7352cc4fd3e123b62df26f`. Its published assets contain
inventory and SBOM data. The pinned [Packer build](https://github.com/actions/runner-images/blob/74456c0b4eff7aa68e7352cc4fd3e123b62df26f/images/ubuntu/templates/build.ubuntu-24_04-arm64.pkr.hcl)
uses the Azure ARM builder. The macOS template's SSH switch is unavailable in
that build.

Start the Ubuntu trial by tracing the pinned build's provisioners and base OS
requirements. On a disposable native ARM64 Ubuntu guest, prototype a Packer
target that preserves their order, inputs and tests. Verify the results against
the official inventory, including differences tied to Azure such as the kernel.
An equivalent local VM and an Ephemera backend for Linux guests on macOS both
remain to be validated. The existing QEMU execution path requires a Linux host.

## Stability acceptance

- Provision twice from fresh guests with the same pinned inputs. Both builds
  must complete the upstream tests, and any differences from the published
  inventory must be explained before adopting the recipe.
- Cold-boot at least three fresh clones per guest type. Record boot time, Runner
  ready time, job duration, total host memory pressure and swap, and cleanup
  completion. Verify failed jobs and failed boots leave no VM or stale Runner.
- Run a representative macOS and iOS build, and a representative Ubuntu ARM64
  build, through a dedicated test runner. Retain logs and the inventory with
  the image digest. Adopt the image only after that guest type passes.

## Local preflight results

- Tart 2.42.0 executed on the Apple Silicon host in an isolated Tart home.
- Packer 1.16.2 initialized the pinned macOS template with Anka plugin 4.7.0.
  Both syntax validation and configuration validation passed with
  `builder_type=null` and disposable placeholder inputs. These commands did
  not connect to a guest.
- The Xcode preflight detected missing archives and rejected a deliberately
  mismatched archive. Apple redirected the unauthenticated archive request
  to an authorization page. An Apple Account session is needed to obtain the
  official Xcode archives. The exact guest build and archive hashes still
  need verification.
- Ubuntu's pinned release provides inventory and SBOM data. Its ARM64 build
  points to Azure ARM. A local Tart adapter and a booted test image have yet
  to be validated.

Neither trial has produced a validated base image.

## CI coverage

The independent [official runner image validation workflow](../../.github/workflows/official-runner-images.yml)
runs on GitHub-hosted macOS 26 ARM64 and Ubuntu 24.04 ARM64. It compiles and runs
a macOS and an Ubuntu smoke program, type-checks an iOS target, validates
the pinned Packer template in SSH mode, and checks the Xcode archive
preflight. The full image build and cold-clone tests require the deployment
host and the inputs described above.
