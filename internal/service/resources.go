package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/status"

	"golang.org/x/sys/unix"
)

const gib = uint64(1 << 30)

type resourceBudget struct {
	root                           string
	memory, disk, memoryReserve    uint64
	sample                         func(context.Context, string) (uint64, uint64, error)
	checked                        time.Time
	availableMemory, availableDisk uint64
	err                            error
}

func (b *resourceBudget) slots(ctx context.Context, now time.Time, starting int, maxSlots int) (int, string) {
	if b == nil || maxSlots <= 0 {
		return maxSlots, ""
	}
	if b.checked.IsZero() || now.Sub(b.checked) >= time.Second {
		b.availableMemory, b.availableDisk, b.err = b.sample(ctx, b.root)
		b.checked = now
	}
	if b.err != nil {
		return 0, "host resources could not be measured: " + b.err.Error()
	}
	reserve := b.reserve()
	memSlots := (max(reserve, b.availableMemory) - reserve) / b.memory
	diskSlots := (max(5*gib, b.availableDisk) - 5*gib) / b.disk
	committed := uint64(starting)
	free := min(memSlots, diskSlots)
	reason := ""
	if committed+uint64(maxSlots) > free {
		if memSlots <= diskSlots {
			reason = fmt.Sprintf("available memory after the %d MiB host reserve and pending VM starts", reserve>>20)
		} else {
			reason = "available VM disk space after the 5 GiB host reserve and pending VM starts"
		}
	}
	return int(min(uint64(maxSlots), max(free, committed)-committed)), reason
}

func (b *resourceBudget) reserve() uint64 {
	if b.memoryReserve == 0 {
		return gib
	}
	return b.memoryReserve
}

func (b *resourceBudget) capacity(now time.Time, startable int) *status.Capacity {
	if b == nil {
		return nil
	}
	view := &status.Capacity{Startable: startable, ReserveMiB: b.reserve() >> 20, VMMemoryMiB: b.memory >> 20}
	if !b.checked.IsZero() && now.Sub(b.checked) <= 2*time.Second && b.err == nil {
		available := b.availableMemory >> 20
		view.AvailableMemoryMiB = &available
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

func sampleResources(ctx context.Context, root string) (uint64, uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
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
		return kib << 10, uint64(fs.Bavail) * uint64(fs.Bsize), err
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
			return mem, uint64(fs.Bavail) * uint64(fs.Bsize), nil
		}
	}
	return 0, 0, errors.New("available memory is missing")
}
