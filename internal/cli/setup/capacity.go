package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/service"
)

// CapacityPreview reports an estimated capacity based on concurrency and disk space.
func CapacityPreview(cfg config.Config) string {
	data := cfg.VMDataPath(host.StateDir())
	diskReserve := uint64(cfg.VM.HostDiskReserveGiB)
	if diskReserve == 0 {
		diskReserve = 5
	}
	slots := uint64(cfg.Runner.MaxConcurrent)
	var lines []string
	if free, err := service.AvailableDiskGiB(data); err == nil {
		slots = min(slots, (max(free, diskReserve)-diskReserve)/uint64(max(12, cfg.VM.DiskGiB)))
		lines = append(lines, fmt.Sprintf("VM storage: %d GiB available · %d GiB reserve", free, diskReserve))
	}
	lines = append(lines, fmt.Sprintf("Estimated runner slots: up to %d / %d", slots, cfg.Runner.MaxConcurrent))
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
