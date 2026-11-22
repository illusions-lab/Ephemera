package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type fakeExecution struct {
	mu               sync.Mutex
	calls            []string
	launches         chan fakeLaunch
	registers        atomic.Int32
	registerFailures atomic.Int32
	deleteFailures   atomic.Int32
	busy             atomic.Bool
}

type fakeLaunch struct {
	instance vm.Instance
	exit     chan error
}

func (f *fakeExecution) record(stage string, i vm.Instance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stage+":"+i.Resource)
}

func (f *fakeExecution) Check(context.Context) error { return nil }
func (f *fakeExecution) Prepare(_ context.Context, i vm.Instance) error {
	f.record("prepare", i)
	return nil
}

func (f *fakeExecution) Start(ctx context.Context, i vm.Instance, jit string) (vm.Process, error) {
	if jit != "test-jit" {
		return vm.Process{}, errors.New("missing JIT configuration")
	}
	f.record("start", i)
	ready, exit := make(chan struct{}), make(chan error, 1)
	close(ready)
	select {
	case f.launches <- fakeLaunch{i, exit}:
		return vm.Process{Ready: ready, Exited: exit}, nil
	case <-ctx.Done():
		return vm.Process{}, ctx.Err()
	}
}

func (f *fakeExecution) Stop(_ context.Context, i vm.Instance) error {
	f.record("stop", i)
	return nil
}

func (f *fakeExecution) Delete(_ context.Context, i vm.Instance) error {
	f.record("delete", i)
	if f.deleteFailures.Add(-1) >= 0 {
		return errors.New("storage temporarily unavailable")
	}
	return nil
}

func (f *fakeExecution) Register(_ context.Context, _ int, name string) (int, string, error) {
	f.record("register", vm.Instance{Resource: name})
	if f.registerFailures.Add(-1) >= 0 {
		return 0, "", errors.New("registration response lost")
	}
	return int(f.registers.Add(1)), "test-jit", nil
}

func (f *fakeExecution) Remove(_ context.Context, _ int, id int, name string) error {
	f.record(fmt.Sprintf("remove-%d", id), vm.Instance{Resource: name})
	if f.busy.Load() {
		return orders.ErrRunnerBusy
	}
	return nil
}

type executionView struct {
	connected, recovering bool
	counts                status.Instances
}

func newFakeReconciler(t *testing.T, store instanceStore, restored []vm.Instance, fake *fakeExecution, idle time.Duration) (*reconciler, <-chan executionView, <-chan struct{}, *atomic.Int32) {
	t.Helper()
	updates := make(chan executionView, 1)
	recovered := make(chan struct{})
	capacity := new(atomic.Int32)
	r := &reconciler{
		backend: fake, registry: fake, store: store, limit: 2, startupTimeout: 2 * time.Second, idleTimeout: idle,
		template: vm.Instance{Backend: "qemu", Image: "/image.qcow2", Scope: "https://github.com/test/project", ScaleSet: "test"},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)), events: make(chan runnerEvent),
		instances: make(map[string]*instanceState),
		report: func(connected, recovering, _ bool, _ orders.Update, counts status.Instances) error {
			select {
			case <-updates:
			default:
			}
			updates <- executionView{connected, recovering, counts}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.run(ctx, restored, recovered, func(free int) { capacity.Store(int32(free)) }) }()
	t.Cleanup(func() {
		fake.busy.Store(false)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("reconciler did not stop")
		}
	})
	return r, updates, recovered, capacity
}

func fakeStore(t *testing.T) instanceStore {
	t.Helper()
	store, close, err := openInstances(t.TempDir() + "/instances")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	return store
}

func waitView(t *testing.T, updates <-chan executionView, predicate func(executionView) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case view := <-updates:
			if predicate(view) {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for reconciler state")
		}
	}
}

func awaitLaunch(t *testing.T, fake *fakeExecution) fakeLaunch {
	t.Helper()
	select {
	case launch := <-fake.launches:
		return launch
	case <-time.After(5 * time.Second):
		t.Fatal("VM was not launched")
		return fakeLaunch{}
	}
}

func sendUpdate(t *testing.T, r *reconciler, update orders.Update) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	update.ScaleSetID = 42
	if err := r.submit(ctx, runnerEvent{kind: statistics, update: update}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionOverlapReplayAndDisconnect(t *testing.T) {
	store := fakeStore(t)
	fake := &fakeExecution{launches: make(chan fakeLaunch, 4)}
	r, updates, _, capacity := newFakeReconciler(t, store, nil, fake, time.Hour)
	sendUpdate(t, r, orders.Update{TargetRunners: 2})
	first, second := awaitLaunch(t, fake), awaitLaunch(t, fake)
	if first.instance.ID == second.instance.ID || first.instance.RunnerID == second.instance.RunnerID {
		t.Fatal("jobs received the same VM or runner")
	}
	waitView(t, updates, func(view executionView) bool { return view.counts.Waiting == 2 })
	for _, launch := range []fakeLaunch{first, second} {
		record, err := os.ReadFile(store.path(launch.instance.ID))
		if err != nil || len(record) == 0 {
			t.Fatalf("instance was started without a record: %v", err)
		}
	}
	started := orders.Update{TargetRunners: 2, Started: []*scaleset.JobStarted{
		{RunnerID: first.instance.RunnerID}, {RunnerID: second.instance.RunnerID},
	}}
	sendUpdate(t, r, started)
	sendUpdate(t, r, started)
	waitView(t, updates, func(view executionView) bool { return view.counts.Running == 2 })
	if err := r.submit(context.Background(), runnerEvent{kind: disconnected}); err != nil {
		t.Fatal(err)
	}
	if capacity.Load() != 0 {
		t.Fatal("disconnected reconciler advertised capacity")
	}
	first.exit <- nil
	waitView(t, updates, func(view executionView) bool {
		return !view.connected && view.counts.Running == 1 && view.counts.Reclaiming == 0
	})
	sendUpdate(t, r, orders.Update{TargetRunners: 0, Completed: []*scaleset.JobCompleted{{RunnerID: second.instance.RunnerID}}})
	second.exit <- nil
	waitView(t, updates, func(view executionView) bool { return view.connected && view.counts == (status.Instances{}) })
	if fake.registers.Load() != 2 || capacity.Load() != 2 {
		t.Fatalf("unexpected replacements or occupied slots, registrations %d capacity %d", fake.registers.Load(), capacity.Load())
	}
}

func TestExecutionRecoveryAndRegistrationRetry(t *testing.T) {
	store := fakeStore(t)
	fake := &fakeExecution{launches: make(chan fakeLaunch, 2)}
	fake.deleteFailures.Store(1)
	fake.registerFailures.Store(1)
	id := uuid.NewString()
	old := vm.Instance{ID: id, Resource: runnerName(id), Backend: "qemu", Scope: "https://github.com/test/project",
		ScaleSet: "test", ScaleSetID: 42, Image: "/old-image.qcow2"}
	if err := store.save(old); err != nil {
		t.Fatal(err)
	}
	restored, err := store.load()
	if err != nil || len(restored) != 1 || restored[0] != old {
		t.Fatalf("load interrupted instance: %v %v", restored, err)
	}
	r, updates, recovered, capacity := newFakeReconciler(t, store, restored, fake, time.Hour)
	waitView(t, updates, func(view executionView) bool { return view.recovering && view.counts.Reclaiming == 1 })
	if capacity.Load() != 0 {
		t.Fatal("advertised capacity during recovery")
	}
	select {
	case <-recovered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery retry did not complete")
	}
	if _, err := os.Stat(store.path(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left the old record: %v", err)
	}
	fake.mu.Lock()
	calls := append([]string(nil), fake.calls...)
	fake.mu.Unlock()
	want := []string{"stop:" + old.Resource, "delete:" + old.Resource, "stop:" + old.Resource, "delete:" + old.Resource, "remove-0:" + old.Resource}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("cleanup order = %v, want %v", calls, want)
	}
	sendUpdate(t, r, orders.Update{TargetRunners: 1})
	launch := awaitLaunch(t, fake)
	sendUpdate(t, r, orders.Update{})
	launch.exit <- nil
	waitView(t, updates, func(view executionView) bool { return view.connected && view.counts == (status.Instances{}) })
}

func TestIdleRemovalKeepsBusyVM(t *testing.T) {
	fake := &fakeExecution{launches: make(chan fakeLaunch, 1)}
	fake.busy.Store(true)
	r, updates, _, _ := newFakeReconciler(t, fakeStore(t), nil, fake, time.Millisecond)
	sendUpdate(t, r, orders.Update{TargetRunners: 1})
	launch := awaitLaunch(t, fake)
	waitView(t, updates, func(view executionView) bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.calls) >= 4 && view.counts.Running == 1
	})
	sendUpdate(t, r, orders.Update{Started: []*scaleset.JobStarted{{RunnerID: launch.instance.RunnerID}}})
	waitView(t, updates, func(view executionView) bool { return view.counts.Running == 1 })
	fake.busy.Store(false)
	sendUpdate(t, r, orders.Update{Completed: []*scaleset.JobCompleted{{RunnerID: launch.instance.RunnerID}}})
	launch.exit <- nil
	waitView(t, updates, func(view executionView) bool { return view.counts == (status.Instances{}) })
}

func TestInstanceStateDirectoryIsExclusive(t *testing.T) {
	store := fakeStore(t)
	_, close, err := openInstances(string(store))
	if err == nil {
		close()
		t.Fatal("a second daemon acquired the instance state directory")
	}
}

func TestExecutionListenerReconnectsWithFreshDemand(t *testing.T) {
	fake := &fakeExecution{launches: make(chan fakeLaunch, 1)}
	r, updates, _, _ := newFakeReconciler(t, fakeStore(t), nil, fake, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	reconnect := make(chan struct{})
	sessions := 0
	cfg := orders.Config{
		Disconnected: func() { _ = r.submit(ctx, runnerEvent{kind: disconnected}) },
	}
	go func() {
		defer close(done)
		listenForExecution(ctx, cfg, r.logger, func(ctx context.Context, _ orders.Config, _ *slog.Logger, update func(context.Context, orders.Update) error) error {
			sessions++
			if sessions == 1 {
				if err := update(ctx, orders.Update{ScaleSetID: 42, TargetRunners: 1}); err != nil {
					return err
				}
				return errors.New("ambiguous acquisition")
			}
			select {
			case <-reconnect:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := update(ctx, orders.Update{ScaleSetID: 42, TargetRunners: 0}); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}, r)
	}()
	launch := awaitLaunch(t, fake)
	waitView(t, updates, func(view executionView) bool { return !view.connected })
	launch.exit <- nil
	waitView(t, updates, func(view executionView) bool { return !view.connected && view.counts == (status.Instances{}) })
	close(reconnect)
	waitView(t, updates, func(view executionView) bool { return view.connected && view.counts == (status.Instances{}) })
	cancel()
	select {
	case <-done:
		if sessions != 2 || fake.registers.Load() != 1 {
			t.Fatalf("opened %d sessions and registered %d runners", sessions, fake.registers.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not stop on cancellation")
	}
}

func TestRecoveryRequiresOriginalTartHome(t *testing.T) {
	i := vm.Instance{Scope: "https://github.com/example", Backend: "tart", Namespace: "/original/tart"}
	template := i
	template.Image = "new-immutable-image"
	if err := validateRestored([]vm.Instance{i}, template); err != nil {
		t.Fatal(err)
	}
	template.Namespace = "/other/tart"
	if err := validateRestored([]vm.Instance{i}, template); err == nil {
		t.Fatal("accepted recovery with a different Tart home")
	}
	i.Backend, i.Namespace = "qemu", ""
	if err := validateRestored([]vm.Instance{i}, i); err != nil {
		t.Fatalf("legacy QEMU record rejected: %v", err)
	}
}
