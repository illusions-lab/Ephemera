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
	memoryCredit                             uint64
	pressureNormal                           func(context.Context) bool
	normalPressure                           bool
	root                                     string
	memory, disk, memoryReserve, diskReserve uint64
	sample                                   func(context.Context, string) (uint64, uint64, error)
	checked                                  time.Time
	availableMemory, availableDisk           uint64
	err                                      error
}

func (b *resourceBudget) slots(ctx context.Context, now time.Time, starting int, maxSlots int) (int, string) {
	if b == nil || maxSlots <= 0 {
		return maxSlots, ""
	}
	if b.checked.IsZero() || now.Sub(b.checked) >= time.Second {
		b.availableMemory, b.availableDisk, b.err = b.sample(ctx, b.root)
		b.checked = now
		b.normalPressure = b.err == nil && b.memoryCredit > 0 && b.pressureNormal != nil && b.pressureNormal(ctx)
	}
	if b.err != nil {
		return 0, "host resources could not be measured: " + b.err.Error()
	}
	if b.memoryCredit > 0 && !b.normalPressure {
		return 0, "host memory pressure is elevated or could not be measured"
	}
	reserve := b.reserve()
	memory := max(reserve, b.availableMemory) - reserve
	if b.normalPressure && b.availableMemory > reserve {
		memory += b.memoryCredit
	}
	memSlots := memory / b.memory
	diskSlots := b.diskSlots(b.availableDisk)
	committed := uint64(starting)
	free := min(memSlots, diskSlots)
	reason := ""
	if committed+uint64(maxSlots) > free {
		if memSlots <= diskSlots {
			reason = fmt.Sprintf("available memory after the %d MiB host reserve and pending VM starts", reserve>>20)
		} else {
			reason = b.diskCapacityReason()
		}
	}
	return int(min(uint64(maxSlots), max(free, committed)-committed)), reason
}

func (b *resourceBudget) diskSlots(available uint64) uint64 {
	reserve := b.reservedDisk()
	return (max(reserve, available) - reserve) / b.disk
}

func (b *resourceBudget) diskCapacityReason() string {
	return fmt.Sprintf("available VM disk space after the %d GiB host reserve and pending VM starts", b.reservedDisk()/gib)
}

func (b *resourceBudget) reserve() uint64 {
	if b.memoryReserve == 0 {
		return gib
	}
	return b.memoryReserve
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
	view := &status.Capacity{Startable: startable, ReserveMiB: b.reserve() >> 20, VMMemoryMiB: b.memory >> 20, DiskReserveGiB: b.reservedDisk() / gib, VMDiskBudgetGiB: b.disk / gib}
	if !b.checked.IsZero() && now.Sub(b.checked) <= 2*time.Second && b.err == nil {
		available := b.availableMemory >> 20
		view.AvailableMemoryMiB = &available
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

func sampleResources(ctx context.Context, root string) (uint64, uint64, error) {
	free, err := availableDiskBytes(root)
	if err != nil {
		return 0, 0, fmt.Errorf("measure VM disk space: %w", err)
	}
	var mem uint64
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, 0, err
		}
		kib, err := linuxMemoryKiB(data, "MemAvailable:")
		return kib << 10, free, err
	case "darwin":
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		out, err := exec.CommandContext(probe, "/usr/bin/vm_stat").Output()
		if err != nil {
			return 0, 0, err
		}
		lines := strings.Split(string(out), "\n")
		_, details, _ := strings.Cut(lines[0], "page size of ")
		pageSize, err := strconv.ParseUint(strings.Fields(details + " invalid")[0], 10, 64)
		if err != nil || pageSize == 0 {
			return 0, 0, errors.New("invalid vm_stat page size")
		}
		found := false
		for _, line := range lines[1:] {
			field, value, _ := strings.Cut(line, ":")
			switch field {
			case "Pages free", "Pages inactive", "Pages speculative":
				pages, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
				if err != nil {
					return 0, 0, err
				}
				found = found || field == "Pages free"
				mem += pages * pageSize
			}
		}
		if found {
			return mem, free, nil
		}
	}
	return 0, 0, errors.New("available memory is missing")
}

// Oversubscription never uses swap as a capacity measurement. The extra budget
// is available only while the host can confirm normal memory pressure.
func normalHostMemoryPressure(ctx context.Context) bool {
	if runtime.GOOS == "darwin" {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		out, err := exec.CommandContext(probe, "/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output()
		return err == nil && strings.TrimSpace(string(out)) == "1"
	}
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return false
	}
	floors := map[string]float64{"some": 5, "full": 1}
	seen := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		floor, ok := floors[fields[0]]
		if !ok {
			continue
		}
		raw, ok := strings.CutPrefix(fields[1], "avg10=")
		value, err := strconv.ParseFloat(raw, 64)
		if !ok || err != nil || !(value >= 0 && value < floor) {
			return false
		}
		delete(floors, fields[0])
		seen++
	}
	return seen == 2
}
