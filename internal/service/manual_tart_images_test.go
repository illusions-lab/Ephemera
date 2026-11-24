package service

import (
	"context"
	"reflect"
	"testing"

	"github.com/illusions-lab/Ephemera/internal/vm"
)

func TestManualTartImageRetentionKeepsRollbackAndRunningImages(t *testing.T) {
	first := tartImageUse{Source: "source-a", Prepared: "first"}
	second := tartImageUse{Source: "source-b", Prepared: "second"}
	third := tartImageUse{Source: "source-b", Prepared: "third"}
	var ledger tartImageLedger
	for _, image := range []tartImageUse{first, second, third} {
		if !ledger.record(image) {
			t.Fatal("new image was not recorded")
		}
	}
	if ledger.record(third) || ledger.Previous != second || !reflect.DeepEqual(ledger.Retired, []tartImageUse{first}) {
		t.Fatalf("unexpected retention ledger %+v", ledger)
	}

	var removed []string
	prune := func(_ context.Context, id string) error {
		removed = append(removed, id)
		return nil
	}
	r := &reconciler{
		images: &imageUpdates{prune: prune, pruneSource: prune},
		instances: map[string]*instanceState{
			"runner": {Instance: vm.Instance{Image: first.Prepared}},
		},
	}
	got, err := r.removeRetiredTartImage(ledger, first, false)
	if err != nil || got || len(removed) != 0 {
		t.Fatalf("pruned image that was still in use: %v, %v, %v", got, err, removed)
	}
	r.instances["runner"].LocalReclaimed = true
	got, err = r.removeRetiredTartImage(ledger, first, false)
	if err != nil || !got || !reflect.DeepEqual(removed, []string{"first", "source-a"}) {
		t.Fatalf("retired image cleanup = %v, %v, %v", got, err, removed)
	}
	got, err = r.removeRetiredTartImage(ledger, tartImageUse{Source: "source-b", Prepared: "older"}, false)
	if err != nil || !got || !reflect.DeepEqual(removed, []string{"first", "source-a", "older"}) {
		t.Fatalf("shared source cleanup = %v, %v, %v", got, err, removed)
	}
}
