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
func (r *reconciler) pruneManualTartImages() {
	u := r.images
	if u.manualSource == "" || u.baseline.id == "" || !r.connected {
		return
	}
	if err := macos.WithInactiveActivation(r.pruneCommittedTartImages); err != nil {
		u.logger.Warn("Check Tart image retention", "error", err)
	}
}

func (r *reconciler) pruneCommittedTartImages() {
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
		if retained[image.Prepared] {
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
	if len(remaining) != len(ledger.Retired) {
		ledger.Retired = remaining
		if err := save(); err != nil {
			u.logger.Warn("Persist Tart image cleanup", "error", err)
		}
	}
}

func (r *reconciler) removeRetiredTartImage(ledger tartImageLedger, image tartImageUse, keepSource bool) (bool, error) {
	u := r.images
	if image == ledger.Current || image == ledger.Previous {
		return true, nil
	}
	if image.Prepared == r.template.Image || image.Prepared == u.previous.id {
		return false, nil
	}
	for _, instance := range r.instances {
		if !instance.LocalReclaimed && instance.Image == image.Prepared {
			return false, nil
		}
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
