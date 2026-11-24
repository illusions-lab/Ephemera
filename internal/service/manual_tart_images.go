package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/illusions-lab/Ephemera/internal/imagechannel"
	"github.com/illusions-lab/Ephemera/internal/macos"
)

type tartImageUse struct {
	Source   string
	Prepared string
}

type tartImageLedger struct {
	Current  tartImageUse
	Previous tartImageUse
	Retired  []tartImageUse
}

func (l *tartImageLedger) record(image tartImageUse) bool {
	if image == l.Current {
		return false
	}
	if l.Previous.Prepared != "" && !slices.Contains(l.Retired, l.Previous) {
		l.Retired = append(l.Retired, l.Previous)
	}
	l.Previous, l.Current = l.Current, image
	return true
}

// Track committed configurations only. A candidate can report healthy while
// its installer still owns the activation journal and may need to roll back.
func (r *reconciler) pruneManualTartImages(needsDisk imageDiskPressure) {
	u := r.images
	if u.manualSource == "" || u.baseline.id == "" || !r.connected {
		return
	}
	if err := macos.WithInactiveActivation(func() { r.pruneCommittedTartImages(needsDisk) }); err != nil {
		u.logger.Warn("Check Tart image retention", "error", err)
	}
}

func (r *reconciler) pruneCommittedTartImages(needsDisk imageDiskPressure) {
	u := r.images
	path := filepath.Join(filepath.Dir(u.path), "manual-tart-images.json")
	var ledger tartImageLedger
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &ledger)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		u.logger.Warn("Read Tart image retention state", "error", err)
		return
	}
	save := func() error {
		return writeImageState(path, ledger)
	}
	if ledger.record(tartImageUse{Source: u.manualSource, Prepared: u.baseline.id}) {
		if err := save(); err != nil {
			u.logger.Warn("Record committed Tart image", "error", err)
			return
		}
	}
	remaining := make([]tartImageUse, 0, len(ledger.Retired))
	candidates := make([]string, 0, len(ledger.Retired))
	for _, image := range ledger.Retired {
		candidates = append(candidates, image.Prepared)
	}
	retained := u.retainExtra(candidates, ledger.Current.Prepared, ledger.Previous.Prepared)
	keptSources := make(map[string]bool)
	for _, image := range ledger.Retired {
		if retained[image.Prepared] {
			keptSources[image.Source] = true
		}
	}
	for _, image := range ledger.Retired {
		if retained[image.Prepared] && !needsDisk.forImage(image.Prepared) {
			remaining = append(remaining, image)
			continue
		}
		removed, err := r.removeRetiredTartImage(ledger, image, keptSources[image.Source])
		if err != nil {
			u.logger.Warn("Remove retired Tart image", "image", image.Prepared, "error", err)
		}
		if !removed {
			remaining = append(remaining, image)
		}
	}
	previous := ledger.Previous
	if previous.Source != "" && previous.Prepared != ledger.Current.Prepared && !r.imageInUse(previous.Prepared) &&
		previous.Prepared != u.previous.id && needsDisk.forImage(previous.Prepared) && u.hasTartSource(previous.Source) {
		ledger.Previous = tartImageUse{}
		ledger.Retired = append(remaining, previous)
		if err := save(); err != nil {
			u.logger.Warn("Retire Tart rollback image under disk pressure", "error", err)
			return
		}
		u.logger.Info("Reclaim Tart rollback image under disk pressure", "image", previous.Prepared)
		removed, err := r.removeRetiredTartImage(ledger, previous, true)
		if err != nil {
			u.logger.Warn("Remove Tart rollback image", "error", err)
		}
		if !removed {
			return
		}
	}
	if len(remaining) != len(ledger.Retired) {
		ledger.Retired = remaining
		if err := save(); err != nil {
			u.logger.Warn("Persist Tart image cleanup", "error", err)
		}
	}
}

func (u *imageUpdates) hasTartSource(ref string) bool {
	if u.checkSource == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return u.checkSource(ctx, ref) == nil
}

func (r *reconciler) removeRetiredTartImage(ledger tartImageLedger, image tartImageUse, keepSource bool) (bool, error) {
	u := r.images
	if image == ledger.Current || image == ledger.Previous {
		return true, nil
	}
	if image.Prepared == ledger.Current.Prepared || image.Prepared == ledger.Previous.Prepared ||
		image.Prepared == u.previous.id || r.imageInUse(image.Prepared) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := u.prune(ctx, image.Prepared); err != nil {
		return false, err
	}
	if keepSource || image.Source == ledger.Current.Source || image.Source == ledger.Previous.Source {
		return true, nil
	}
	for _, recipe := range []*imagechannel.Recipe{u.state.Current, u.state.Previous} {
		if recipe != nil && recipe.TartRef == image.Source {
			return true, nil
		}
	}
	err := u.pruneSource(ctx, image.Source)
	return err == nil, err
}
