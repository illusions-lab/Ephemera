package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

func TestExecutionPreparesImageAfterRecovery(t *testing.T) {
	imageFailure := errors.New("image validation failed")
	for _, test := range []struct {
		name     string
		imageErr error
	}{
		{name: "initial statistics open capacity"},
		{name: "image failure keeps listener closed", imageErr: imageFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			store := fakeStore(t)
			fake := &fakeExecution{}
			fake.deleteFailures.Store(1)
			registry := &rejectFirstRemoval{fakeExecution: fake, store: store}
			github := orders.Config{ScopeURL: "https://github.com/test/project", ScaleSetName: "test", MaxConcurrent: 2}
			id := uuid.NewString()
			old := vm.Instance{ID: id, Resource: runnerName(id), Backend: "tart", Namespace: string(store) + "/tart", Scope: github.ScopeURL,
				ScaleSet: github.ScaleSetName, ScaleSetID: 42, RunnerID: 123, Image: "/old.qcow2"}
			if err := store.save(old); err != nil {
				t.Fatal(err)
			}
			cfg := config.MacDefaults()
			prepare := func() (string, func() error, error) {
				records, err := store.load()
				if err != nil || len(records) != 1 || !records[0].LocalReclaimed {
					return "", nil, fmt.Errorf("expected durable deregistration record: %v, %w", records, err)
				}
				fake.record("prepare-current-image", old)
				return "/new.qcow2", func() error { return nil }, test.imageErr
			}
			var healthy atomic.Bool
			var recovered bool
			listen := func(ctx context.Context, cfg orders.Config, _ *slog.Logger, handle func(context.Context, orders.Update) error) error {
				if test.imageErr != nil {
					t.Error("listener opened after image failure")
					cancel()
					return nil
				}
				if got := cfg.Capacity(); got != 0 {
					t.Errorf("capacity before initial statistics = %d", got)
				}
				if err := handle(ctx, orders.Update{ScaleSetID: 42, DemandObservedAt: time.Now()}); err != nil {
					t.Error(err)
				}
				if got := cfg.Capacity(); !healthy.Load() || got != 2 {
					t.Errorf("after initial statistics: healthy = %t, capacity = %d", healthy.Load(), got)
				}
				cancel()
				return nil
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			err := execute(ctx, cfg, github, old.Namespace, store, fake, registry, logger, listen, prepare,
				func(connected, recovering, _ bool, _ orders.Update, counts status.Instances) error {
					if connected {
						healthy.Store(true)
					}
					recovered = recovered || (recovering && counts.Reclaiming == 1)
					return nil
				})
			if !errors.Is(err, test.imageErr) || !recovered || (test.imageErr == nil && !healthy.Load()) {
				t.Fatalf("execute = %v, recovery reported = %t, healthy = %t", err, recovered, healthy.Load())
			}
			want := []string{"stop:" + old.Resource, "delete:" + old.Resource, "stop:" + old.Resource,
				"delete:" + old.Resource, "remove-123:" + old.Resource, "prepare-current-image:" + old.Resource}
			if !reflect.DeepEqual(fake.calls, want) {
				t.Errorf("execution order = %v, want %v", fake.calls, want)
			}
		})
	}
}

type rejectFirstRemoval struct {
	*fakeExecution
	store    instanceStore
	rejected atomic.Bool
}

func (r *rejectFirstRemoval) Remove(ctx context.Context, set, id int, name string) error {
	if r.rejected.CompareAndSwap(false, true) {
		r.record(fmt.Sprintf("remove-%d", id), vm.Instance{Resource: name})
		return orders.ErrRunnerBusy
	}
	if _, err := os.Stat(r.store.path(strings.TrimPrefix(name, "ephemera-"))); err != nil {
		return fmt.Errorf("recovery record missing before GitHub accepted deletion: %w", err)
	}
	return r.fakeExecution.Remove(ctx, set, id, name)
}
