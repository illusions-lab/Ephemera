package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type registry interface {
	Register(context.Context, int, string) (int, string, error)
	Remove(context.Context, int, int, string) error
}

type runnerPhase int

const (
	starting runnerPhase = iota
	waiting
	running
	finishing
	reclaiming
)

type instanceState struct {
	vm.Instance
	job                 control.Job
	phase               runnerPhase
	deadline            time.Time
	createdAt           time.Time
	cancel              context.CancelFunc
	inFlight, launching bool
	recovered           bool
	failures            int
}

type eventKind int

const (
	statistics eventKind = iota
	disconnected
	registered
	ready
	exited
	idleRemoved
	cleaned
	localReclaimed
	imageChecked
	jobTerminal
	imageUnavailable
	imageAvailable
	imageRepaired
	drainStatus
)

type runnerEvent struct {
	kind       eventKind
	drain      control.Request
	drainReply chan control.Snapshot
	image      preparedImage
	source     *preparedImage
	checked    time.Time
	imageReply chan updateState
	update     orders.Update
	reply      chan struct{}
	id         string
	runnerID   int
	stage      string
	err        error
}

type reconciler struct {
	gate                        intakeGate
	budget                      *resourceBudget
	images                      *imageUpdates
	active                      atomic.Pointer[preparedImage]
	imageBlocked                atomic.Bool
	imageReason                 string
	nextPrune                   time.Time
	backend                     vm.Backend
	registry                    registry
	store                       instanceStore
	template                    vm.Instance
	limit                       int
	startupTimeout, idleTimeout time.Duration
	logger                      *slog.Logger
	events                      chan runnerEvent
	instances                   map[string]*instanceState
	exitedRunners               map[int]vm.Instance
	connected, draining         bool
	retryStart                  time.Time
	startFailures               int
	update                      orders.Update
	reportRows                  func([]status.Instance)
	report                      func(bool, bool, bool, orders.Update, status.Instances) error
}

func (r *reconciler) send(ctx context.Context, event runnerEvent) error {
	select {
	case r.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *reconciler) submit(ctx context.Context, event runnerEvent) error {
	event.reply = make(chan struct{})
	if err := r.send(ctx, event); err != nil {
		return err
	}
	select {
	case <-event.reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *reconciler) run(ctx context.Context, restored []vm.Instance, afterRecovery func() error, capacity func(int)) error {
	workers, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for _, instance := range restored {
		r.instances[instance.ID] = &instanceState{Instance: instance, phase: reclaiming, recovered: !instance.LocalReclaimed}
	}
	stopping := ctx.Done()
	var reportErr error
	for {
		if ctx.Err() != nil && stopping != nil {
			r.draining = true
			r.connected = false
			stopping = nil
		}
		_, acquiring := r.gate.state()
		if r.draining && !acquiring && r.localInstances() == 0 {
			return reportErr
		}
		var event runnerEvent
		select {
		case <-stopping:
			continue
		case <-ticker.C:
		case event = <-r.events:
			r.apply(event)
		}
		recovering := hasRecovery(r.instances)
		if !recovering && afterRecovery != nil && ctx.Err() == nil && !r.draining {
			if err := afterRecovery(); err != nil {
				return err
			}
			afterRecovery = nil
		}
		free, counts := r.reconcile(workers, time.Now())
		if ctx.Err() != nil {
			free = 0
		}
		if r.reportRows != nil {
			r.reportRows(r.instanceRows())
		}
		if err := r.report(r.connected, recovering, r.draining, r.update, counts); err != nil && reportErr == nil {
			reportErr = err
			free = 0
			r.draining = true
			r.connected = false
			stopping = nil
		}
		capacity(free)
		if event.reply != nil {
			close(event.reply)
		}
	}
}

func hasRecovery(instances map[string]*instanceState) bool {
	for _, i := range instances {
		if i.recovered {
			return true
		}
	}
	return false
}

func (r *reconciler) apply(event runnerEvent) {
	now := time.Now()
	switch event.kind {
	case drainStatus:
		event.drainReply <- r.drainSnapshot(event.drain)
	case statistics:
		if !r.draining {
			r.connected = true
		}
		r.update = event.update
		r.template.ScaleSetID = event.update.ScaleSetID
		for _, requestID := range event.update.Acquired {
			r.logger.Info("Job acquired", "request_id", requestID)
		}
		for _, job := range event.update.Started {
			if job == nil || job.RunnerID <= 0 {
				continue
			}
			for _, i := range r.instances {
				if i.RunnerID == job.RunnerID && (i.phase == starting || i.phase == waiting || (i.phase == running && i.job.Request == 0)) {
					i.phase, i.deadline = running, time.Time{}
					i.job = control.Job{
						Identity:   control.Identity{Instance: i.ID, Request: job.RunnerRequestID},
						Repository: job.OwnerName + "/" + job.RepositoryName, Run: job.WorkflowRunID,
						Name: job.JobDisplayName, Runner: job.RunnerName, Started: now,
					}
					if !job.RunnerAssignTime.IsZero() && !job.RunnerAssignTime.After(now) {
						i.job.Started = job.RunnerAssignTime
					}
					r.logger.Info("Job started", "instance", i.ID, "image_revision", i.ImageRevision, "image", i.Image, "request_id", job.RunnerRequestID)
				}
			}
		}
		for _, job := range event.update.Completed {
			if job == nil || job.RunnerID <= 0 {
				continue
			}
			if instance, ok := r.exitedRunners[job.RunnerID]; ok {
				delete(r.exitedRunners, job.RunnerID)
				r.imageOutcome(instance, runnerEvent{kind: jobTerminal})
			}
			for _, i := range r.instances {
				if i.RunnerID == job.RunnerID {
					if i.phase == running || i.phase == waiting || i.phase == starting {
						r.imageOutcome(i.Instance, runnerEvent{kind: jobTerminal})
						i.phase, i.deadline = finishing, now.Add(15*time.Second)
					}
					r.logger.Info("Job completed", "instance", i.ID, "request_id", job.RunnerRequestID, "result", job.Result)
				}
			}
		}
	case imageChecked:
		r.imageChecked(event)
	case imageUnavailable:
		if event.source != nil && event.source == r.active.Load() {
			r.imageBlocked.Store(true)
			r.imageReason = awaitingImageRepair
		}
	case imageAvailable:
		if event.source == r.active.Load() && r.imageReason == awaitingImageRepair {
			if r.images != nil && r.images.state.Failures >= 3 {
				r.blockFailedImage()
			} else {
				r.imageReason = ""
				r.imageBlocked.Store(false)
			}
		}
	case imageRepaired:
		r.repairImage(event)

	case disconnected:
		r.connected = false
	case registered:
		r.instances[event.id].RunnerID = event.runnerID
	case ready:
		if r.budget != nil {
			r.budget.checked = time.Time{}
		}
		i := r.instances[event.id]
		if i == nil {
			return
		}
		r.startFailures = 0
		r.retryStart = time.Time{}
		i.launching = false
		if i.cancel != nil {
			i.cancel()
		}
		if i.phase == starting {
			i.phase, i.deadline = waiting, now.Add(r.idleTimeout)
		}
	case exited:
		i := r.instances[event.id]
		if i == nil {
			return
		}
		if event.err != nil && i.phase != reclaiming {
			r.logger.Warn("Instance operation failed", "instance", i.ID, "phase", event.stage, "error", event.err)
		}
		if i.phase < finishing {
			r.imageOutcome(i.Instance, event)
			if i.RunnerID > 0 && i.phase >= waiting && !errors.Is(event.err, vm.ErrImageInfrastructure) {
				if r.exitedRunners == nil {
					r.exitedRunners = make(map[int]vm.Instance)
				}
				// A completion can follow local exit and deregistration. Keep
				// bounded attribution even after the local record is gone.
				if len(r.exitedRunners) >= 4096 {
					for id := range r.exitedRunners {
						delete(r.exitedRunners, id)
						break
					}
				}
				r.exitedRunners[i.RunnerID] = i.Instance
			}
		}
		if i.phase == starting {
			r.startFailures++
			r.retryStart = now.Add(backoff(r.startFailures))
		}
		if i.cancel != nil {
			i.cancel()
		}
		i.launching = false
		if i.phase != reclaiming {
			i.phase, i.deadline = reclaiming, time.Time{}
		}
	case idleRemoved:
		i := r.instances[event.id]
		i.inFlight = false
		if i.phase == reclaiming {
			return
		}
		if event.err == nil {
			i.phase, i.deadline = reclaiming, time.Time{}
		} else if i.phase == waiting {
			if errors.Is(event.err, orders.ErrRunnerBusy) {
				i.phase, i.deadline = running, time.Time{}
			} else {
				i.failures++
				i.deadline = now.Add(backoff(i.failures))
				r.logger.Warn("Instance operation failed", "instance", i.ID, "phase", "remove idle runner", "error", event.err)
			}
		}
	case localReclaimed:
		if r.budget != nil {
			r.budget.checked = time.Time{}
		}
		i := r.instances[event.id]
		i.LocalReclaimed, i.inFlight = true, false
		i.failures, i.deadline = 0, time.Time{}
	case cleaned:
		i := r.instances[event.id]
		if event.err == nil {
			delete(r.instances, event.id)
		} else {
			i.inFlight = false
			if i.LocalReclaimed {
				i.recovered = false
			}
			i.failures++
			i.deadline = now.Add(backoff(i.failures))
			r.logger.Warn("Instance operation failed", "instance", event.id, "phase", event.stage, "error", event.err)
		}
	}
}

func backoff(failures int) time.Duration {
	return time.Second << min(failures-1, 5)
}

func (r *reconciler) localInstances() int {
	count := 0
	for _, i := range r.instances {
		if !i.LocalReclaimed {
			count++
		}
	}
	return count
}

func (r *reconciler) reconcile(ctx context.Context, now time.Time) (int, status.Instances) {
	manual, acquiring := r.gate.state()
	for _, i := range r.instances {
		switch {
		case (r.draining || manual) && !acquiring && i.phase == waiting && i.failures == 0:
			i.deadline = time.Time{}
		case i.phase == starting && !now.Before(i.deadline):
			i.cancel()
		case i.phase == finishing && !now.Before(i.deadline):
			i.phase, i.deadline = reclaiming, time.Time{}
		}
		if i.inFlight || i.launching || now.Before(i.deadline) {
			continue
		}
		switch i.phase {
		case reclaiming:
			i.inFlight = true
			go r.cleanup(ctx, i.Instance)
		case waiting:
			i.inFlight = true
			go r.removeIdle(ctx, i.Instance)
		}
	}
	free := 0
	capacityReason := r.imageReason
	if r.connected && !r.draining && !manual && !r.imageBlocked.Load() && !hasRecovery(r.instances) {
		local := r.localInstances()
		startingCount := 0
		for _, i := range r.instances {
			if i.launching {
				startingCount++
			}
		}
		slots, reason := r.budget.slots(ctx, now, startingCount, max(0, r.limit-local))
		capacityReason = reason
		if now.Before(r.retryStart) {
			slots = 0
			capacityReason = "VM startup is backing off after a failure"
		}
		for local < r.update.TargetRunners && slots > 0 {
			id, err := uuid.NewRandom()
			if err != nil {
				r.logger.Error("Could not generate instance identity", "error", err)
				break
			}
			r.gate.mu.Lock()
			if r.gate.closed {
				r.gate.mu.Unlock()
				slots = 0
				break
			}
			i := r.template
			i.ID, i.Resource = id.String(), runnerName(id.String())
			startCtx, cancel := context.WithTimeout(ctx, r.startupTimeout)
			r.instances[i.ID] = &instanceState{Instance: i, phase: starting, createdAt: now, launching: true, cancel: cancel, deadline: now.Add(r.startupTimeout)}
			r.gate.mu.Unlock()
			go r.launch(ctx, startCtx, i, r.active.Load())
			local++
			slots--
		}
		free = slots
		for _, i := range r.instances {
			if i.phase == waiting && !i.inFlight && i.failures == 0 {
				free++
			}
		}
	}
	counts := status.Instances{Draining: manual || r.draining, CapacityReason: capacityReason}
	for _, i := range r.instances {
		if i.LocalReclaimed {
			counts.PendingDeregistration++
			continue
		}
		switch i.phase {
		case starting:
			counts.Starting++
		case waiting:
			counts.Waiting++
		case running, finishing:
			counts.Running++
		case reclaiming:
			counts.Reclaiming++
		}
	}
	if r.images != nil && !r.draining && !manual && now.After(r.nextPrune) {
		r.nextPrune = now.Add(30 * time.Second)
		r.pruneImages()
	}
	return free, counts
}

func (r *reconciler) instanceRows() []status.Instance {
	rows := make([]status.Instance, 0, len(r.instances))
	for _, instance := range r.instances {
		state := "pending-deregistration"
		if !instance.LocalReclaimed {
			switch instance.phase {
			case starting:
				state = "starting"
			case waiting:
				state = "waiting"
			case running:
				state = "running"
			case finishing:
				state = "finishing"
			case reclaiming:
				state = "reclaiming"
			}
		}
		rows = append(rows, status.Instance{ID: instance.ID, Runner: instance.Resource, State: state, CreatedAt: instance.createdAt})
	}
	return rows
}

func (r *reconciler) launch(ctx, startup context.Context, i vm.Instance, image *preparedImage) {
	var checkImage func() error
	if image != nil {
		checkImage = image.check
	}
	fail := func(stage string, err error) {
		_ = r.send(ctx, runnerEvent{kind: exited, id: i.ID, stage: stage, err: err})
	}
	if err := r.store.save(i); err != nil {
		fail("persist instance", err)
		return
	}
	if checkImage != nil {
		if err := checkImage(); err != nil {
			_ = r.send(ctx, runnerEvent{kind: imageUnavailable, source: image})
			fail("check base image", err)
			return
		}
	}
	if err := r.backend.Prepare(startup, i); err != nil {
		fail("prepare VM", err)
		return
	}
	id, jit, err := r.registry.Register(startup, i.ScaleSetID, i.Resource)
	if err != nil {
		fail("register runner", err)
		return
	}
	i.RunnerID = id
	if err := r.store.save(i); err != nil {
		fail("persist runner ID", err)
		return
	}
	if err := r.submit(ctx, runnerEvent{kind: registered, id: i.ID, runnerID: id}); err != nil {
		return
	}
	if checkImage != nil {
		if err := checkImage(); err != nil {
			_ = r.send(ctx, runnerEvent{kind: imageUnavailable, source: image})
			fail("check base image", err)
			return
		}
	}
	process, err := r.backend.Start(startup, i, jit)
	if err != nil {
		fail("start VM", err)
		return
	}
	select {
	case <-process.Ready:
		if err := startup.Err(); err != nil {
			fail("wait for readiness", err)
			return
		}
		_ = r.send(ctx, runnerEvent{kind: ready, id: i.ID})
	case err := <-process.Exited:
		fail("exit before readiness", errors.Join(errors.New("VM exited before readiness"), err))
		return
	case <-startup.Done():
		fail("wait for readiness", startup.Err())
		return
	}
	select {
	case err := <-process.Exited:
		fail("VM exit", err)
	case <-ctx.Done():
	}
}

func (r *reconciler) removeIdle(worker context.Context, i vm.Instance) {
	ctx, cancel := context.WithTimeout(worker, 10*time.Second)
	defer cancel()
	err := r.registry.Remove(ctx, i.ScaleSetID, i.RunnerID, i.Resource)
	// Use the worker lifetime for reporting even if the API deadline expired.
	_ = r.send(worker, runnerEvent{kind: idleRemoved, id: i.ID, err: err})
}

func (r *reconciler) cleanup(worker context.Context, i vm.Instance) {
	ctx, cancel := context.WithTimeout(worker, 10*time.Second)
	defer cancel()
	stage, err := r.reclaim(ctx, i)
	if err == nil && !i.LocalReclaimed {
		_ = r.send(worker, runnerEvent{kind: localReclaimed, id: i.ID})
		return
	}
	_ = r.send(worker, runnerEvent{kind: cleaned, id: i.ID, stage: stage, err: err})
}

func (r *reconciler) reclaim(ctx context.Context, i vm.Instance) (string, error) {
	if !i.LocalReclaimed {
		if err := r.backend.Stop(ctx, i); err != nil {
			return "stop VM", err
		}
		if err := r.backend.Delete(ctx, i); err != nil {
			return "delete VM", err
		}
		i.LocalReclaimed = true
		return "persist local reclamation", r.store.save(i)
	}
	if err := r.registry.Remove(ctx, i.ScaleSetID, i.RunnerID, i.Resource); err != nil {
		return "remove runner registration", err
	}
	return "remove instance record", r.store.remove(i.ID)
}

func validateRestored(instances []vm.Instance, template vm.Instance) error {
	for _, i := range instances {
		if i.Scope != template.Scope || i.ScaleSet != template.ScaleSet || i.Backend != template.Backend || i.Namespace != template.Namespace {
			return fmt.Errorf("instance %s requires its original GitHub scope, VM backend and resource directory for recovery", i.ID)
		}
	}
	return nil
}
