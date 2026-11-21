// Package qemuplatform describes the guest hardware shared by execution and image verification.
package qemuplatform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Platform struct {
	Binary, Serial string
	arm            bool
}

func ForArch(arch string) (Platform, error) {
	switch arch {
	case "amd64":
		return Platform{Binary: "qemu-system-x86_64", Serial: "/dev/ttyS1"}, nil
	case "arm64":
		return Platform{Binary: "qemu-system-aarch64", Serial: "/dev/hvc0", arm: true}, nil
	default:
		return Platform{}, fmt.Errorf("unsupported QEMU architecture %q", arch)
	}
}

const (
	firmwareCode = "/usr/share/AAVMF/AAVMF_CODE.fd"
	firmwareVars = "/usr/share/AAVMF/AAVMF_VARS.fd"
)

func (p Platform) CheckFirmware() error {
	if !p.arm {
		return nil
	}
	for _, path := range []string{firmwareCode, firmwareVars} {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("check ARM64 UEFI firmware (install qemu-efi-aarch64): %w", err)
		}
		file.Close()
	}
	return nil
}

func (p Platform) BootArgs(dir string, kvm bool) ([]string, error) {
	accel, cpu := "tcg", "max"
	if kvm {
		accel, cpu = "kvm", "host"
	}
	if !p.arm {
		return []string{"-machine", "accel=" + accel, "-cpu", cpu, "-boot", "order=c"}, nil
	}
	if !kvm {
		cpu = "cortex-a72"
	}
	data, err := os.ReadFile(firmwareVars)
	if err != nil {
		return nil, err
	}
	vars := filepath.Join(dir, "uefi-vars.fd")
	if err := os.WriteFile(vars, data, 0600); err != nil {
		return nil, err
	}
	return []string{"-machine", "virt,gic-version=3,accel=" + accel, "-cpu", cpu,
		"-drive", "if=pflash,format=raw,readonly=on," + FileOption(firmwareCode),
		"-drive", "if=pflash,format=raw," + FileOption(vars)}, nil
}

func FileOption(path string) string { return "file=" + strings.ReplaceAll(path, ",", ",,") }

// StatusArgs reserves a separate host channel for runner status.
func (p Platform) StatusArgs(path string) []string {
	if !p.arm {
		target := "stdio"
		if path != "" {
			target = "file:" + path
		}
		return []string{"-serial", "null", "-serial", target}
	}
	char := "stdio,id=runnerstatus"
	if path != "" {
		char = "file,id=runnerstatus,path=" + strings.ReplaceAll(path, ",", ",,")
	}
	return []string{"-serial", "null", "-device", "virtio-serial-pci",
		"-chardev", char, "-device", "virtconsole,chardev=runnerstatus"}
}
