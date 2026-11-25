package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

func TestResourceBudgetReservesDiskAndPendingStarts(t *testing.T) {
	var disk uint64
	var sampleErr error
	budget := &resourceBudget{disk: 20 * gib, sample: func(string) (uint64, error) {
		return disk, sampleErr
	}}
	now := time.Now()
	for _, tc := range []struct {
		name                  string
		disk                  uint64
		starting, limit, want int
		reason                string
		err                   error
	}{
		{name: "insufficient disk", disk: 25*gib - 1, limit: 2, reason: "disk"},
		{name: "one runner is admitted with enough disk", disk: 25 * gib, limit: 1, want: 1},
		{name: "pending start holds its slot", disk: 25 * gib, starting: 1, limit: 1, reason: "disk"},
		{name: "disk measurement failure", disk: 65 * gib, limit: 2, reason: "measurement failed", err: errors.New("measurement failed")},
		{name: "recovery respects concurrency limit", disk: 65 * gib, limit: 2, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now = now.Add(time.Second)
			disk, sampleErr = tc.disk, tc.err
			got, why := budget.slots(now, tc.starting, tc.limit)
			if got != tc.want || (tc.reason == "" && why != "") || (tc.reason != "" && !strings.Contains(why, tc.reason)) {
				t.Fatalf("slots = %d (%q), want %d (%q)", got, why, tc.want, tc.reason)
			}
		})
	}
}

func TestResourceExhaustionLeavesReadyRunnerEligible(t *testing.T) {
	now := time.Now()
	r := &reconciler{
		connected: true, limit: 2, update: orders.Update{TargetRunners: 2},
		budget: &resourceBudget{disk: 12 * gib, sample: func(string) (uint64, error) {
			return 5 * gib, nil
		}},
		instances: map[string]*instanceState{
			"id": {Instance: vm.Instance{ID: "id"}, phase: waiting, deadline: now.Add(time.Hour)},
		},
	}
	free, counts := r.reconcile(t.Context(), now)
	if free != 1 || counts.Waiting != 1 || counts.Starting != 0 || counts.Capacity == nil ||
		counts.Capacity.Startable != 0 || !strings.Contains(counts.CapacityReason, "disk") {
		t.Fatalf("acquisition slots = %d, instances = %+v", free, counts)
	}
}

// Registration cleanup can retry after local deletion without retaining a VM slot.
func TestDeregistrationBacklogDoesNotRetainLocallyReclaimedCapacity(t *testing.T) {
	store := fakeStore(t)
	fake := &fakeExecution{launches: make(chan fakeLaunch, 2)}
	r, updates, _, capacity := newFakeReconciler(t, store, nil, fake, time.Hour)
	sendUpdate(t, r, orders.Update{TargetRunners: 1})
	launch := awaitLaunch(t, fake)
	sendUpdate(t, r, orders.Update{TargetRunners: 1, Started: []*scaleset.JobStarted{jobStarted(launch, 200)}})
	if err := r.submit(t.Context(), runnerEvent{kind: disconnected}); err != nil {
		t.Fatal(err)
	}
	fake.busy.Store(true)
	launch.exit <- nil
	waitView(t, updates, func(view executionView) bool {
		return !view.connected && view.counts.PendingDeregistration == 1 && view.counts.Reclaiming == 0
	})
	records, err := store.load()
	if err != nil || len(records) != 1 || records[0].ID != launch.instance.ID || !records[0].LocalReclaimed {
		t.Fatalf("deregistration was not persisted: %+v, %v", records, err)
	}
	if capacity.Load() != 0 {
		t.Fatal("offline reconciler advertised unused local capacity")
	}
	sendUpdate(t, r, orders.Update{})
	if capacity.Load() != 2 {
		t.Fatalf("reconnected capacity = %d, want 2", capacity.Load())
	}
	fake.busy.Store(false)
	waitView(t, updates, func(view executionView) bool { return view.counts.PendingDeregistration == 0 })
}
