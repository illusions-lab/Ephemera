package service

import (
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/vm"
)

func TestInstanceRowsIncludeLocalAndPendingCleanup(t *testing.T) {
	now := time.Now()
	r := &reconciler{instances: map[string]*instanceState{
		"live": {
			Instance: vm.Instance{ID: "live", Resource: "runner-live"},
			phase:    waiting, createdAt: now,
		},
		"recovered": {
			Instance: vm.Instance{ID: "recovered", Resource: "runner-recovered"},
			phase:    reclaiming,
		},
		"pending": {
			Instance: vm.Instance{ID: "pending", Resource: "runner-pending", LocalReclaimed: true},
			phase:    reclaiming,
		},
	}}
	rows := r.instanceRows()
	want := map[string]string{"live": "waiting", "recovered": "reclaiming", "pending": "pending-deregistration"}
	if len(rows) != len(want) {
		t.Fatalf("rows: %+v", rows)
	}
	for _, row := range rows {
		if row.State != want[row.ID] || row.Runner != "runner-"+row.ID {
			t.Errorf("unexpected row %+v", row)
		}
		if row.ID == "live" && !row.CreatedAt.Equal(now) || row.ID != "live" && !row.CreatedAt.IsZero() {
			t.Errorf("unexpected creation time in %+v", row)
		}
	}
	r.instances["live"].phase = running
	for _, row := range rows {
		if row.ID == "live" && row.State != "waiting" {
			t.Errorf("an already published instance changed: %+v", row)
		}
	}
}
