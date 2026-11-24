package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/service"
)

// CapacityPreview reports an empty-host upper bound and a current disk estimate.
func CapacityPreview(cfg config.Config) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data := cfg.VMDataPath(host.StateDir())
	total, err := service.HostMemoryMiB(ctx)
	reserve := uint64(cfg.VM.HostReserveMiB)
	if reserve == 0 {
		reserve = 1024
	}
	diskReserve := uint64(cfg.VM.HostDiskReserveGiB)
	if diskReserve == 0 {
		diskReserve = 5
	}
	memorySlots := uint64(cfg.Runner.MaxConcurrent)
	var lines []string
	if err == nil {
		memorySlots = (max(total, reserve) - reserve) / uint64(cfg.VM.MemoryMiB)
		lines = append(lines, fmt.Sprintf("Host memory: %d MiB total · %d MiB reserve", total, reserve))
	}
	if cfg.VM.MemoryOversubscribeMiB > 0 {
		lines = append(lines, fmt.Sprintf("Extra memory budget with normal pressure: %d MiB", cfg.VM.MemoryOversubscribeMiB))
	}
	slots := min(uint64(cfg.Runner.MaxConcurrent), memorySlots)
	if free, err := service.AvailableDiskGiB(data); err == nil {
		slots = min(slots, (max(free, diskReserve)-diskReserve)/uint64(max(12, cfg.VM.DiskGiB)))
		lines = append(lines, fmt.Sprintf("VM storage: %d GiB available · %d GiB reserve", free, diskReserve))
	}
	lines = append(lines, fmt.Sprintf("Estimated slots without oversubscription: up to %d / %d", slots, cfg.Runner.MaxConcurrent))
	if cfg.VM.Backend == "tart" {
		if local, ok := strings.CutPrefix(cfg.VM.ImageRef, "local:"); ok && filepath.Base(local) == local {
			var metadata struct {
				CPUCountMin   int
				MemorySizeMin uint64
			}
			raw, err := os.ReadFile(filepath.Join(data, "vms", local, "config.json"))
			if err == nil && json.Unmarshal(raw, &metadata) == nil && metadata.CPUCountMin > 0 && metadata.MemorySizeMin > 0 {
				memory := 1 + ((metadata.MemorySizeMin - 1) >> 20)
				lines = append(lines, fmt.Sprintf("Image minimum: %d vCPUs · %d MiB", metadata.CPUCountMin, memory))
				if cfg.VM.VCPUs < metadata.CPUCountMin || uint64(cfg.VM.MemoryMiB) < memory {
					lines = append(lines, "Current VM settings are below the image minimum.")
				}
				return strings.Join(lines, "\n")
			}
		}
		lines = append(lines, "Image minimum is checked during Tart image preparation.")
	}
	return strings.Join(lines, "\n")
}
