package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/status"

	"golang.org/x/sys/unix"
)

const gib = uint64(1 << 30)

type resourceBudget struct {
	root              string
	disk, diskReserve uint64
	sample            func(string) (uint64, error)
	checked           time.Time
	availableDisk     uint64
	err               error
}

func (b *resourceBudget) slots(now time.Time, starting int, maxSlots int) (int, string) {
	if b == nil || maxSlots <= 0 {
		return maxSlots, ""
	}
	if b.checked.IsZero() || now.Sub(b.checked) >= time.Second {
		b.availableDisk, b.err = b.sample(b.root)
		b.checked = now
	}
	if b.err != nil {
		return 0, "host disk space could not be measured: " + b.err.Error()
	}
	free := b.diskSlots(b.availableDisk)
	committed := uint64(starting)
	if free < committed+uint64(maxSlots) {
		return int(max(free, committed) - committed), b.diskCapacityReason()
	}
	return maxSlots, ""
}

func (b *resourceBudget) diskSlots(available uint64) uint64 {
	reserve := b.reservedDisk()
	return (max(reserve, available) - reserve) / b.disk
}

func (b *resourceBudget) diskCapacityReason() string {
	return fmt.Sprintf("available VM disk space after the %d GiB host reserve and pending VM starts", b.reservedDisk()/gib)
}

func (b *resourceBudget) reservedDisk() uint64 {
	if b.diskReserve == 0 {
		return 5 * gib
	}
	return b.diskReserve
}

func (b *resourceBudget) capacity(now time.Time, startable int) *status.Capacity {
	if b == nil {
		return nil
	}
	view := &status.Capacity{Startable: startable, DiskReserveGiB: b.reservedDisk() / gib, VMDiskBudgetGiB: b.disk / gib}
	if !b.checked.IsZero() && now.Sub(b.checked) <= 2*time.Second && b.err == nil {
		disk := b.availableDisk / gib
		view.AvailableDiskGiB = &disk
	}
	return view
}

// HostMemoryMiB returns the memory visible to the host kernel.
func HostMemoryMiB(ctx context.Context) (uint64, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, err
		}
		bytes, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		return bytes >> 20, err
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	kib, err := linuxMemoryKiB(data, "MemTotal:")
	return kib >> 10, err
}

func linuxMemoryKiB(data []byte, field string) (uint64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == field && fields[2] == "kB" {
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%s missing from /proc/meminfo", field)
}

// AvailableDiskGiB resolves a not-yet-created configuration directory to
// its existing filesystem for the capacity preview.
func AvailableDiskGiB(path string) (uint64, error) {
	for {
		free, err := availableDiskBytes(path)
		parent := filepath.Dir(path)
		if !errors.Is(err, os.ErrNotExist) || parent == path {
			return free / gib, err
		}
		path = parent
	}
}

func availableDiskBytes(path string) (uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}
