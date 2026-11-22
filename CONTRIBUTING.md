# Contributing to Ephemera

Ephemera has two entry points. The `ephe` CLI handles setup and diagnostics, and `ephemerad` talks to GitHub and manages ephemeral job VMs. See the [architecture guide](ARCHITECTURE.md) for how the daemon, image recipes, and VM backends fit together.

## Set up your checkout

Install the Go version specified in [go.mod](go.mod), Make, and ShellCheck. Then run:

```sh
make build
make check
```

The binaries are in `bin/`. `make check` runs Go tests, vet, shell linting, and example configuration validation. Format changed Go files with `gofmt` before submitting.

## Choose the checks for your change

| Change | Check |
| --- | --- |
| CLI, configuration, or daemon behavior | Run the affected package's Go tests, then `make check`. |
| Linux installer, images, or QEMU | Build a matching bundle with `make bundle-linux LINUX_ARCH=amd64` or `LINUX_ARCH=arm64`. Follow the [Linux test instructions](README.md#test-qemu-execution). |
| macOS installer or Tart | Run `make bundle-macos` on Apple Silicon and follow the [macOS test guide](packaging/macos/README.md). |
| Live job execution or recovery | Use a dedicated test Scale Set and the [acceptance workflow](.github/workflows/execution-acceptance.yml). Record the candidate commit, host architecture, and workflow run. |

Use the [source install instructions](README.md#install-a-candidate-from-source) to test an installer change. Release publishing runs through GitHub Actions.

## Report an issue

Open a [GitHub issue](https://github.com/illusions-lab/Ephemera/issues/new/choose). For a bug, tell us the host OS and architecture, Ephemera version, backend, and commands that reproduce it. If relevant, include sanitized output from `ephe status` or `ephe doctor`. Remove private keys, tokens, and GitHub runner JIT configuration from anything you share.

## Open a pull request

Explain the behavior you changed and the test results. Include the affected host platforms for platform-specific changes. For a change to provisioning, shutdown, or recovery, describe what happens when a job is interrupted or the daemon restarts. Update the README or architecture guide when the user-facing behavior changes.
