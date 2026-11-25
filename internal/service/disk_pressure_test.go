package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/imagechannel"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

func TestDiskPressureEvictsOnlyEnoughRetiredImages(t *testing.T) {
	var removed []string
	u := &imageUpdates{
		path: filepath.Join(t.TempDir(), "state.json"), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		extraRetained: 10,
		baseline:      preparedImage{id: "baseline"},
		previous:      preparedImage{id: "rollback", recipe: &imagechannel.Recipe{Revision: 2}},
		state: updateState{Current: &imagechannel.Recipe{Revision: 3}, Previous: &imagechannel.Recipe{Revision: 2},
			Retired: []string{"running", "oldest", "newer", "active", "baseline"}},
		prune: func(_ context.Context, id string) error {
			removed = append(removed, id)
			return nil
		},
	}
	r := &reconciler{images: u, template: vm.Instance{Image: "active"},
		instances: map[string]*instanceState{"job": {Instance: vm.Instance{Image: "running"}}}}
	r.pruneImages(nil)
	if len(removed) != 0 {
		t.Fatalf("removed without pressure: %v", removed)
	}
	r.pruneImages(func(string) bool { return len(removed) < 1 })
	if !slices.Equal(removed, []string{"oldest"}) || u.state.Previous == nil {
		t.Fatalf("first pass removed %v; previous is %v", removed, u.state.Previous)
	}
	r.pruneImages(func(string) bool { return true })
	if !slices.Equal(removed, []string{"oldest", "newer", "rollback"}) ||
		!slices.Equal(u.state.Retired, []string{"running", "active", "baseline"}) ||
		u.state.Previous != nil || u.previous.id != u.baseline.id {
		t.Fatalf("unexpected reclamation: removed=%v, state=%+v", removed, u.state)
	}
	restarted := &imageUpdates{path: u.path, logger: u.logger}
	restarted.prepare = func(_ context.Context, recipe imagechannel.Recipe, _ bool) (preparedImage, error) {
		if recipe.Revision != 3 {
			t.Fatalf("unexpected image prepared at restart: %v", recipe.Revision)
		}
		return preparedImage{id: "active", recipe: &recipe}, nil
	}
	if got := restarted.recover(t.Context(), u.baseline); got.id != "active" || restarted.previous.id != "baseline" {
		t.Fatalf("recovered active=%q, fallback=%q", got.id, restarted.previous.id)
	}
}

func TestDiskPressureProtectsTartRollbackWithoutVerifiedSource(t *testing.T) {
	previous := tartImageUse{Source: "local:backup", Prepared: "previous"}
	current := tartImageUse{Source: "local:current", Prepared: "current"}
	path := filepath.Join(t.TempDir(), "state.json")
	ledgerPath := filepath.Join(filepath.Dir(path), "manual-tart-images.json")
	if err := writeImageState(ledgerPath, tartImageLedger{Current: current, Previous: previous}); err != nil {
		t.Fatal(err)
	}
	var removed []string
	u := &imageUpdates{path: path, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		baseline: preparedImage{id: current.Prepared}, manualSource: current.Source,
		prune: func(_ context.Context, id string) error {
			removed = append(removed, id)
			return nil
		},
		pruneSource: func(_ context.Context, id string) error {
			t.Fatalf("attempted removal of Tart source %q", id)
			return nil
		},
		checkSource: func(context.Context, string) error { return errors.New("source missing") }}
	r := &reconciler{images: u, template: vm.Instance{Image: current.Prepared}}
	pressure := func(string) bool { return true }
	r.pruneCommittedTartImages(pressure)
	if len(removed) != 0 {
		t.Fatalf("removed unique rollback: %v", removed)
	}
	u.checkSource = func(_ context.Context, ref string) error {
		if ref != previous.Source {
			t.Fatalf("checked wrong source %q", ref)
		}
		return nil
	}
	r.instances = map[string]*instanceState{"job": {Instance: vm.Instance{Image: previous.Prepared}}}
	r.pruneCommittedTartImages(pressure)
	if len(removed) != 0 {
		t.Fatalf("removed in-use rollback: %v", removed)
	}
	r.instances["job"].LocalReclaimed = true
	r.pruneCommittedTartImages(pressure)
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var ledger tartImageLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{previous.Prepared}) || ledger.Current != current ||
		ledger.Previous != (tartImageUse{}) || len(ledger.Retired) != 0 {
		t.Fatalf("unexpected reclamation: removed=%v, ledger=%+v", removed, ledger)
	}
}

func TestReconcileReclaimsDiskOnlyWhenQueuedWorkCanBenefit(t *testing.T) {
	for _, tc := range []struct {
		name                string
		optIn               bool
		available, assigned int
		reclaim             bool
	}{
		{name: "available jobs", optIn: true, available: 1, reclaim: true},
		{name: "assigned jobs", optIn: true, assigned: 1, reclaim: true},
		{name: "idle host", optIn: true},
		{name: "opted out", available: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			free, err := availableDiskBytes(root)
			if err != nil {
				t.Fatal(err)
			}
			var removed []string
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			r := &reconciler{connected: true, limit: 1, logger: logger,
				template: vm.Instance{Image: "active"},
				update:   orders.Update{AvailableJobs: tc.available, TargetRunners: tc.assigned},
				budget:   &resourceBudget{root: root, disk: free + gib, sample: availableDiskBytes},
				images: &imageUpdates{path: filepath.Join(root, "state.json"), logger: logger,
					state: updateState{Retired: []string{"old"}}, extraRetained: 1, pruneOnPressure: tc.optIn,
					imagePath: func(string) string { return root },
					prune: func(_ context.Context, id string) error {
						removed = append(removed, id)
						return nil
					},
				},
			}
			r.reconcile(t.Context(), time.Now())
			if got := slices.Equal(removed, []string{"old"}); got != tc.reclaim {
				t.Fatalf("removed %v (want reclamation: %v)", removed, tc.reclaim)
			}
		})
	}
}
