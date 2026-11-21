package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/qemuplatform"
)

const DefaultCache = "/var/lib/ephemera/image"

type Config struct {
	Platform  Platform
	Source    Source
	VCPUs     int
	MemoryMiB int
}

// Prepare returns an immutable disk only after a successful guest boot check.
func Prepare(ctx context.Context, root string, cfg Config, logger *slog.Logger) (Image, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := cfg.Platform.Validate(); err != nil {
		return Image{}, err
	}
	if !supports(cfg.Platform) {
		return Image{}, fmt.Errorf("no verified image recipe for %s", cfg.Platform)
	}
	if err := cfg.Source.Validate(); err != nil {
		return Image{}, err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != cfg.Platform.Arch {
		return Image{}, fmt.Errorf("%s image preparation requires a Linux %s host", cfg.Platform, cfg.Platform.Arch)
	}
	platform, _ := qemuplatform.ForArch(cfg.Platform.Arch)
	if err := platform.CheckFirmware(); err != nil {
		return Image{}, err
	}
	if _, err := exec.LookPath(platform.Binary); err != nil {
		return Image{}, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Image{}, err
	}
	cfg.Source = cfg.Source.normalized()
	c := cache{
		root: root, logger: logger,
		client: &http.Client{CheckRedirect: func(req *http.Request, previous []*http.Request) error {
			if req.URL.Scheme != "https" || len(previous) >= 10 {
				return errors.New("refusing an insecure or excessive download redirect")
			}
			return nil
		}},
	}
	key, err := imageCacheKey(cfg)
	if err != nil {
		return Image{}, err
	}
	return c.prepare(ctx, key, func(ctx context.Context, work string) error {
		return c.buildUbuntu(ctx, work, cfg)
	})
}

// CheckCached verifies the cache entry for the current recipe without building
// an image or writing to the cache.
func CheckCached(ctx context.Context, root string, cfg Config) (Image, error) {
	if err := cfg.Platform.Validate(); err != nil {
		return Image{}, err
	}
	if !supports(cfg.Platform) {
		return Image{}, fmt.Errorf("no verified image recipe for %s", cfg.Platform)
	}
	if err := cfg.Source.Validate(); err != nil {
		return Image{}, err
	}
	cfg.Source = cfg.Source.normalized()
	key, err := imageCacheKey(cfg)
	if err != nil {
		return Image{}, err
	}
	return verifiedImage(ctx, filepath.Join(root, key))
}

func imageCacheKey(cfg Config) (string, error) {
	recipe, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	// Recipe revisions also cover changes to commands and dependency setup.
	hash := sha256.New()
	io.WriteString(hash, "ubuntu-image-recipe-v4\x00")
	hash.Write(recipe)
	if cfg.Platform.Arch == "arm64" {
		io.WriteString(hash, "arm64-uefi-v3\x00")
	}
	io.WriteString(hash, runnerUnit+runnerLauncher+bootConfig+RunnerCloudConfig(""))
	return cfg.Platform.Tag + "-" + cfg.Platform.Arch + "-" + hex.EncodeToString(hash.Sum(nil)), nil
}

const runnerUnit = `[Unit]
Description=Ephemera GitHub Actions runner
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=ephemera-runner
WorkingDirectory=/opt/actions-runner
LoadCredential=jitconfig:/run/ephemera/jitconfig
ExecStart=/usr/local/bin/ephemera-runner
StandardOutput=tty
StandardError=tty
TTYPath=/dev/ttyS1
ExecStopPost=+/usr/bin/systemctl --no-block poweroff
`

const runnerLauncher = `#!/bin/sh
set -eu
ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat "$CREDENTIALS_DIRECTORY/jitconfig")
if [ "${EPHEMERA_BOOT_SMOKE:-}" = 1 ]; then
    test "$ACTIONS_RUNNER_INPUT_JITCONFIG" = ephemera-offline-smoke
    ./bin/Runner.Listener --version
    printf '%s\n' '` + bootMarker + `'
    exit 0
fi
export ACTIONS_RUNNER_INPUT_JITCONFIG
exec ./bin/Runner.Listener run
`

const bootMarker = "EPHEMERA_IMAGE_BOOT_VALIDATED"

// RunnerCloudConfig installs a runtime credential and starts the guest service.
func RunnerCloudConfig(jit string) string {
	return runnerCloudConfig(jit, false)
}

func runnerCloudConfig(jit string, smoke bool) string {
	config := `#cloud-config
write_files:
  - path: /run/ephemera/jitconfig
    permissions: '0600'
    content: ` + strconv.Quote(jit) + "\n"
	if smoke {
		config += `  - path: /etc/systemd/system/ephemera-runner.service.d/smoke.conf
    content: |
      [Service]
      Environment=EPHEMERA_BOOT_SMOKE=1
`
	}
	return config + `runcmd:
  - [systemctl, daemon-reload]
  - [systemctl, start, ephemera-runner.service]
  - [rm, -f, /run/ephemera/jitconfig]
`
}

var bootConfig = runnerCloudConfig("ephemera-offline-smoke", true)

func (c cache) buildUbuntu(ctx context.Context, work string, cfg Config) error {
	platform, _ := qemuplatform.ForArch(cfg.Platform.Arch)
	rootPartition := "/dev/sda1"
	bootInstall := "grub-install --target=i386-pc /dev/sda && update-grub"
	for _, name := range []string{"qemu-img", "virt-resize", "virt-customize", "cloud-localds"} {
		if _, err := exec.LookPath(name); err != nil {
			return err
		}
	}
	c.logger.Info("Preparing Ubuntu sources", "platform", cfg.Platform.String(), "runner_version", cfg.Source.RunnerVersion)
	base, err := c.download(ctx, cfg.Source.BaseURL, cfg.Source.BaseSHA256)
	if err != nil {
		return fmt.Errorf("prepare base disk: %w", err)
	}
	if cfg.Platform.Arch == "arm64" {
		out, err := exec.CommandContext(ctx, "guestfish", "--ro", "--format=qcow2", "-a", base, "run", ":", "inspect-os").Output()
		if err != nil {
			return fmt.Errorf("inspect ARM64 root partition: %w", err)
		}
		rootPartition = strings.TrimSpace(string(out))
		if !regexp.MustCompile(`^/dev/sda[0-9]+$`).MatchString(rootPartition) {
			return errors.New("expected one ARM64 root partition")
		}
		bootInstall = "mountpoint -q /boot/efi && (test -f /boot/efi/EFI/BOOT/BOOTAA64.EFI || grub-install --target=arm64-efi --efi-directory=/boot/efi --removable --no-nvram) && systemctl mask serial-getty@hvc0.service"
	}
	runner, err := c.download(ctx, cfg.Source.runnerURL(cfg.Platform.Arch), cfg.Source.RunnerSHA256)
	if err != nil {
		return fmt.Errorf("prepare GitHub runner: %w", err)
	}
	temp, err := os.MkdirTemp(work, "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	for name, content := range map[string]string{
		"runner.service": strings.Replace(runnerUnit, "/dev/ttyS1", platform.Serial, 1), "runner-launcher": runnerLauncher,
		"user-data": bootConfig, "meta-data": "instance-id: ephemera-image-validation\n",
	} {
		if err := os.WriteFile(filepath.Join(temp, name), []byte(content), 0600); err != nil {
			return err
		}
	}
	disk := filepath.Join(work, "image.qcow2")
	c.logger.Info("Assembling Ubuntu image")
	// Grow the guest filesystem before installing packages, using Ubuntu's
	// partition layout rather than relying on cloud-init at runner boot.
	if err := command(ctx, "qemu-img", "create", "-f", "qcow2", disk, "12G"); err != nil {
		return err
	}
	if err := command(ctx, "virt-resize", "--format", "qcow2", "--output-format", "qcow2", "--align-first", "never",
		"--expand", rootPartition, base, disk); err != nil {
		return err
	}
	if err := command(ctx, "virt-customize", "--format", "qcow2", "-a", disk, "--no-logfile",
		"--upload", runner+":/tmp/ephemera-runner.tar.gz",
		"--upload", filepath.Join(temp, "runner.service")+":/etc/systemd/system/ephemera-runner.service",
		"--upload", filepath.Join(temp, "runner-launcher")+":/usr/local/bin/ephemera-runner",
		// libguestfs bind-mounts its resolver into the guest for commands.
		// Ubuntu's normal resolver symlink needs the guest's running systemd.
		"--delete", "/etc/resolv.conf",
		"--write", "/etc/resolv.conf:",
		"--install", "ca-certificates,curl,git",
		"--run-command", "useradd --create-home --shell /bin/bash ephemera-runner && install -d /opt/actions-runner",
		"--run-command", "tar -xzf /tmp/ephemera-runner.tar.gz -C /opt/actions-runner && /opt/actions-runner/bin/installdependencies.sh",
		"--run-command", "chown -R ephemera-runner:ephemera-runner /opt/actions-runner && chmod 0755 /usr/local/bin/ephemera-runner && chmod 0644 /etc/systemd/system/ephemera-runner.service",
		"--delete", "/tmp/ephemera-runner.tar.gz",
		// virt-resize can move the BIOS boot partition, invalidating the
		// original GRUB core image location recorded in the boot sector.
		"--run-command", bootInstall,
		"--run-command", "cloud-init clean --logs --machine-id && touch /etc/machine-id",
		"--delete", "/etc/resolv.conf",
		"--link", "/run/systemd/resolve/stub-resolv.conf:/etc/resolv.conf"); err != nil {
		return err
	}
	if err := command(ctx, "qemu-img", "check", "-f", "qcow2", disk); err != nil {
		return err
	}
	c.logger.Info("Booting Ubuntu image for validation")
	return bootCheck(ctx, temp, disk, cfg)
}

func bootCheck(ctx context.Context, temp, disk string, cfg Config) error {
	timeout := 5 * time.Minute
	if cfg.Platform.Arch == "arm64" {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	overlay, seed := filepath.Join(temp, "boot.qcow2"), filepath.Join(temp, "seed.img")
	if err := command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", disk, overlay); err != nil {
		return err
	}
	if err := command(ctx, "cloud-localds", seed, filepath.Join(temp, "user-data"), filepath.Join(temp, "meta-data")); err != nil {
		return err
	}
	console := filepath.Join(temp, "console.log")
	platform, err := qemuplatform.ForArch(cfg.Platform.Arch)
	if err != nil {
		return err
	}
	boot, err := platform.BootArgs(temp, false)
	if err != nil {
		return err
	}
	args := []string{
		"-m", strconv.Itoa(cfg.MemoryMiB), "-smp", strconv.Itoa(cfg.VCPUs),
		"-display", "none", "-monitor", "none", "-no-reboot",
		"-nic", "user,restrict=on,model=virtio-net-pci",
		"-drive", qemuplatform.FileOption(overlay) + ",format=qcow2,if=virtio",
		"-drive", qemuplatform.FileOption(seed) + ",format=raw,if=virtio,readonly=on"}
	statusArgs := platform.StatusArgs(console)
	var bootLog string
	if os.Getenv("EPHEMERA_OFFLINE_BOOT_DEBUG") == "1" {
		bootLog = filepath.Join(temp, "boot-kernel.log")
		statusArgs[1] = "file:" + bootLog
	}
	args = append(args, statusArgs...)
	err = command(ctx, platform.Binary, append(args, boot...)...)
	data, readErr := os.ReadFile(console)
	if err == nil && readErr == nil {
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.Equal(bytes.TrimSpace(line), []byte(bootMarker)) {
				return nil
			}
		}
	}
	failure := errors.Join(err, readErr, errors.New("guest did not complete validation"))
	if bootLog != "" {
		kernel, _ := os.ReadFile(bootLog)
		return fmt.Errorf("Ubuntu boot check failed: %w\nGuest boot tail:\n%s\nRunner console tail:\n%s",
			failure, kernel[max(0, len(kernel)-4096):], data[max(0, len(data)-2048):])
	}
	return fmt.Errorf("Ubuntu boot check failed: %w", failure)
}

func command(ctx context.Context, name string, args ...string) error {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if len(output) > 4096 {
		output = output[len(output)-4096:]
	}
	return fmt.Errorf("%s: %w\n%s", name, err, output)
}
