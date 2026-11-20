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
	"runtime"
	"strconv"
	"strings"
	"time"
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
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return Image{}, errors.New("ubuntu/amd64 image preparation requires a Linux amd64 host")
	}
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
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
	recipe, err := json.Marshal(cfg)
	if err != nil {
		return Image{}, err
	}
	// Recipe revisions also cover changes to commands and dependency setup.
	hash := sha256.New()
	io.WriteString(hash, "ubuntu-image-recipe-v1\x00")
	hash.Write(recipe)
	io.WriteString(hash, runnerUnit+runnerLauncher+bootConfig)
	key := cfg.Platform.Tag + "-" + cfg.Platform.Arch + "-" + hex.EncodeToString(hash.Sum(nil))
	return c.prepare(ctx, key, func(ctx context.Context, work string) error {
		return c.buildUbuntu(ctx, work, cfg)
	})
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
`

const runnerLauncher = `#!/bin/sh
set -eu
ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat "$CREDENTIALS_DIRECTORY/jitconfig")
export ACTIONS_RUNNER_INPUT_JITCONFIG
exec ./run.sh
`

const bootMarker = "EPHEMERA_IMAGE_BOOT_VALIDATED"

const bootConfig = `#cloud-config
runcmd:
  - [sh, -ec, 'systemd-analyze verify /etc/systemd/system/ephemera-runner.service && runuser -u ephemera-runner -- /opt/actions-runner/bin/Runner.Listener --version && printf "%s\n" ` + bootMarker + ` >/dev/ttyS0 && shutdown -P now']
`

func (c cache) buildUbuntu(ctx context.Context, work string, cfg Config) error {
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
	runner, err := c.download(ctx, cfg.Source.runnerURL(), cfg.Source.RunnerSHA256)
	if err != nil {
		return fmt.Errorf("prepare GitHub runner: %w", err)
	}
	temp, err := os.MkdirTemp(work, "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	for name, content := range map[string]string{
		"runner.service": runnerUnit, "runner-launcher": runnerLauncher,
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
		"--expand", "/dev/sda1", base, disk); err != nil {
		return err
	}
	if err := command(ctx, "virt-customize", "--format", "qcow2", "-a", disk, "--no-logfile",
		"--upload", runner+":/tmp/ephemera-runner.tar.gz",
		"--upload", filepath.Join(temp, "runner.service")+":/etc/systemd/system/ephemera-runner.service",
		"--upload", filepath.Join(temp, "runner-launcher")+":/usr/local/bin/ephemera-runner",
		"--install", "ca-certificates,curl,git",
		"--run-command", "useradd --create-home --shell /bin/bash ephemera-runner && install -d /opt/actions-runner",
		"--run-command", "tar -xzf /tmp/ephemera-runner.tar.gz -C /opt/actions-runner && /opt/actions-runner/bin/installdependencies.sh",
		"--run-command", "chown -R ephemera-runner:ephemera-runner /opt/actions-runner && chmod 0755 /usr/local/bin/ephemera-runner && chmod 0644 /etc/systemd/system/ephemera-runner.service",
		"--delete", "/tmp/ephemera-runner.tar.gz",
		"--run-command", "cloud-init clean --logs --machine-id"); err != nil {
		return err
	}
	if err := command(ctx, "qemu-img", "check", "-f", "qcow2", disk); err != nil {
		return err
	}
	c.logger.Info("Booting Ubuntu image for validation")
	return bootCheck(ctx, temp, disk, cfg)
}

func bootCheck(ctx context.Context, temp, disk string, cfg Config) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	overlay, seed := filepath.Join(temp, "boot.qcow2"), filepath.Join(temp, "seed.img")
	if err := command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", disk, overlay); err != nil {
		return err
	}
	if err := command(ctx, "cloud-localds", seed, filepath.Join(temp, "user-data"), filepath.Join(temp, "meta-data")); err != nil {
		return err
	}
	console := filepath.Join(temp, "console.log")
	err := command(ctx, "qemu-system-x86_64", "-machine", "accel=kvm:tcg",
		"-m", strconv.Itoa(cfg.MemoryMiB), "-smp", strconv.Itoa(cfg.VCPUs),
		"-display", "none", "-monitor", "none", "-serial", "file:"+console, "-no-reboot",
		"-nic", "user,restrict=on,model=virtio-net-pci",
		"-drive", diskOption(overlay)+",format=qcow2,if=virtio",
		"-drive", diskOption(seed)+",format=raw,if=virtio,readonly=on")
	data, readErr := os.ReadFile(console)
	if err == nil && readErr == nil {
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.Equal(bytes.TrimSpace(line), []byte(bootMarker)) {
				return nil
			}
		}
	}
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	return fmt.Errorf("Ubuntu boot check failed: %w\n%s", errors.Join(err, readErr, errors.New("guest did not complete validation")), data)
}

func diskOption(path string) string {
	return "file=" + strings.ReplaceAll(path, ",", ",,")
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
