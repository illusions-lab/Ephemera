package service

import (
	"cmp"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

func drainFixture(t *testing.T) (*reconciler, *fakeExecution, <-chan executionView, string) {
	t.Helper()
	fake := &fakeExecution{launches: make(chan fakeLaunch, 2)}
	r, updates, _, _ := newFakeReconciler(t, fakeStore(t), nil, fake, time.Hour)
	// macOS limits Unix socket path length. Use a short, private directory.
	root, err := os.MkdirTemp("/tmp", "ephemera-drain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	closeControl, err := control.Listen(t.Context(), root, r.serveDrain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeControl)
	return r, fake, updates, root
}

func openDrainLease(t *testing.T, root string) (func(control.Request) control.Snapshot, func()) {
	t.Helper()
	conn, err := net.Dial("unix", control.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	stop := func() { _ = conn.Close() }
	t.Cleanup(stop)
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	return func(req control.Request) control.Snapshot {
		t.Helper()
		if err := enc.Encode(req); err != nil {
			t.Fatal(err)
		}
		var snapshot control.Snapshot
		if err := dec.Decode(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}, stop
}

func jobStarted(launch fakeLaunch, request int64) *scaleset.JobStarted {
	return &scaleset.JobStarted{
		RunnerID: launch.instance.RunnerID,
		JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: request, OwnerName: "org", RepositoryName: "repo", WorkflowRunID: 123, JobDisplayName: "build",
		},
	}
}

func TestDrainLeaseWaitsForAcquisitionAndResumesIntakeOnDisconnect(t *testing.T) {
	r, fake, updates, root := drainFixture(t)
	info, err := os.Stat(control.Path(root))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("control socket is not private: %v, %v", info, err)
	}
	sendUpdate(t, r, orders.Update{TargetRunners: 1})
	launch := awaitLaunch(t, fake)
	release, ok := r.gate.begin()
	if !ok {
		t.Fatal("could not start acquisition before drain")
	}
	defer func() {
		if release != nil {
			release()
		}
		launch.exit <- nil
	}()
	exchange, stop := openDrainLease(t, root)
	view := exchange(control.Request{})
	if !view.Pending || view.Ready || len(view.Jobs) != 0 {
		t.Fatalf("drain returned job list before acquisition settled: %+v", view)
	}
	if done, ok := r.gate.begin(); ok {
		done()
		t.Fatal("drain allowed another acquisition")
	}
	sendUpdate(t, r, orders.Update{
		TargetRunners: 2,
		Started:       []*scaleset.JobStarted{jobStarted(launch, 71)},
	})
	release()
	release = nil
	view = exchange(control.Request{})
	if view.Pending || view.Ready || len(view.Jobs) != 1 {
		t.Fatalf("unexpected job list: %+v", view)
	}
	job := view.Jobs[0]
	if job.Request != 71 || job.Instance != launch.instance.ID || job.Repository != "org/repo" ||
		job.Run != 123 || job.Name != "build" || job.Runner != launch.instance.Resource || job.Started.IsZero() {
		t.Fatalf("wrong job metadata: %+v", job)
	}
	waitView(t, updates, func(view executionView) bool { return view.counts.Draining && view.counts.Running == 1 })
	if fake.registers.Load() != 1 {
		t.Fatal("reconciler launched an additional runner during drain")
	}
	stop()
	waitView(t, updates, func(view executionView) bool { return !view.counts.Draining && view.counts.Running == 1 })
	next := awaitLaunch(t, fake)
	next.exit <- nil
}

func TestDrainConfirmationRejectsChangedViewAndUnknownJobs(t *testing.T) {
	r, fake, updates, root := drainFixture(t)
	sendUpdate(t, r, orders.Update{TargetRunners: 2})
	first, second := awaitLaunch(t, fake), awaitLaunch(t, fake)
	defer func() {
		first.exit <- nil
		second.exit <- nil
	}()
	sendUpdate(t, r, orders.Update{TargetRunners: 2, Started: []*scaleset.JobStarted{jobStarted(first, 37), jobStarted(second, 51)}})
	exchange, _ := openDrainLease(t, root)
	current := exchange(control.Request{})
	firstID := control.Identity{Instance: first.instance.ID, Request: 37}
	secondID := control.Identity{Instance: second.instance.ID, Request: 51}
	view := []control.Identity{firstID, secondID}
	slices.SortFunc(view, func(a, b control.Identity) int {
		return cmp.Compare(a.Instance, b.Instance)
	})
	if len(current.Jobs) != 2 || current.Ready {
		t.Fatalf("unexpected initial snapshot: %+v", current)
	}
	sendUpdate(t, r, orders.Update{Completed: []*scaleset.JobCompleted{{RunnerID: second.instance.RunnerID}}})
	if response := exchange(control.Request{View: view, Abort: view}); !response.Changed {
		t.Fatalf("abort accepted after the job list changed: %+v", response)
	}
	waitView(t, updates, func(view executionView) bool {
		return view.counts.Running == 2 && view.counts.Reclaiming == 0
	})
	view = []control.Identity{firstID}
	if response := exchange(control.Request{View: view, Abort: view}); response.Changed {
		t.Fatalf("current job identity was rejected: %+v", response)
	}
	waitView(t, updates, func(view executionView) bool { return view.counts.Running == 1 && view.counts.Reclaiming == 0 })
	fake.mu.Lock()
	stopped := slices.Contains(fake.calls, "stop:"+first.instance.Resource)
	stoppedOther := slices.Contains(fake.calls, "stop:"+second.instance.Resource)
	fake.mu.Unlock()
	if !stopped || stoppedOther {
		t.Fatalf("stop confirmed job = %t, stop completed job = %t", stopped, stoppedOther)
	}

	unknown := control.Identity{Instance: "unknown"}
	reconcile := reconciler{instances: map[string]*instanceState{
		unknown.Instance: {Instance: vm.Instance{ID: unknown.Instance}, phase: running},
	}}
	response := reconcile.drainSnapshot(control.Request{View: []control.Identity{unknown}, Abort: []control.Identity{unknown}})
	if !response.Changed || reconcile.instances[unknown.Instance].phase != running {
		t.Fatalf("unknown job could be aborted: %+v", response)
	}
}

func TestDrainWithoutTerminalWaitsAndHoldsLeaseForServiceOperation(t *testing.T) {
	r, fake, updates, root := drainFixture(t)
	sendUpdate(t, r, orders.Update{TargetRunners: 1})
	launch := awaitLaunch(t, fake)
	defer func() {
		select {
		case launch.exit <- nil:
		default:
		}
	}()
	sendUpdate(t, r, orders.Update{TargetRunners: 1, Started: []*scaleset.JobStarted{jobStarted(launch, 76)}})

	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Create(filepath.Join(root, "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	called := make(chan bool, 1)
	go func() {
		done <- control.Drain(ctx, root, input, output, func() error {
			closed, pending := r.gate.state()
			called <- closed && !pending
			return nil
		})
	}()
	waitView(t, updates, func(view executionView) bool { return view.counts.Draining && view.counts.Running == 1 })
	select {
	case err := <-done:
		t.Fatalf("service operation returned before the job finished: %v", err)
	default:
	}
	sendUpdate(t, r, orders.Update{Completed: []*scaleset.JobCompleted{{RunnerID: launch.instance.RunnerID}}})
	launch.exit <- nil
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if !<-called {
			t.Fatal("service operation ran without the drain lease")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
