package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
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
	phase               runnerPhase
	deadline            time.Time
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
)

type runnerEvent struct {
	kind     eventKind
	update   orders.Update
	reply    chan struct{}
	id       string
	runnerID int
	stage    string
	err      error
}

type reconciler struct {
	checkImage                  func() error
	backend                     vm.Backend
	registry                    registry
	store                       instanceStore
	template                    vm.Instance
	limit                       int
	startupTimeout, idleTimeout time.Duration
	logger                      *slog.Logger
	events                      chan runnerEvent
	instances                   map[string]*instanceState
	connected, draining         bool
	stopAt                      time.Time
	retryStart                  time.Time
	startFailures               int
	update                      orders.Update
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

func (r *reconciler) run(ctx context.Context, restored []vm.Instance, recovered chan struct{}, capacity func(int)) error {
	workers, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for _, instance := range restored {
		r.instances[instance.ID] = &instanceState{Instance: instance, phase: reclaiming, recovered: true}
	}
	stopping := ctx.Done()
	var shutdownDeadline <-chan time.Time
	var reportErr error
	for {
		if ctx.Err() != nil && !r.draining {
			r.draining = true
			r.connected = false
			stopping = nil
			r.stopAt = time.Now().Add(20 * time.Second)
			timer := time.NewTimer(55 * time.Second)
			defer timer.Stop()
			shutdownDeadline = timer.C
		}
		if r.draining && len(r.instances) == 0 {
			return reportErr
		}
		var event runnerEvent
		select {
		case <-stopping:
			continue
		case <-shutdownDeadline:
			return errors.Join(reportErr, errors.New("shutdown ended with instances pending recovery"))
		case <-ticker.C:
		case event = <-r.events:
			r.apply(event)
		}
		recovering := hasRecovery(r.instances)
		if !recovering && recovered != nil {
			close(recovered)
			recovered = nil
		}
		free, counts := r.reconcile(workers, time.Now())
		if ctx.Err() != nil {
			free = 0
		}
		capacity(free)
		if event.reply != nil {
			close(event.reply)
		}
		if err := r.report(r.connected, recovering, r.draining, r.update, counts); err != nil && reportErr == nil {
			reportErr = err
			capacity(0)
			r.draining = true
			r.connected = false
			r.stopAt = time.Now()
			stopping = nil
			timer := time.NewTimer(55 * time.Second)
			defer timer.Stop()
			shutdownDeadline = timer.C
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
				if i.RunnerID == job.RunnerID && (i.phase == starting || i.phase == waiting) {
					i.phase, i.deadline = running, time.Time{}
					r.logger.Info("Job started", "instance", i.ID, "request_id", job.RunnerRequestID)
				}
			}
		}
		for _, job := range event.update.Completed {
			if job == nil || job.RunnerID <= 0 {
				continue
			}
			for _, i := range r.instances {
				if i.RunnerID == job.RunnerID && i.phase < finishing {
					i.phase, i.deadline = finishing, now.Add(15*time.Second)
					r.logger.Info("Job completed", "instance", i.ID, "request_id", job.RunnerRequestID, "result", job.Result)
				}
			}
		}
	case disconnected:
		r.connected = false
	case registered:
		r.instances[event.id].RunnerID = event.runnerID
	case ready:
		i := r.instances[event.id]
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
	case cleaned:
		i := r.instances[event.id]
		if event.err == nil {
			delete(r.instances, event.id)
		} else {
			i.inFlight = false
			i.failures++
			i.deadline = now.Add(backoff(i.failures))
			r.logger.Warn("Instance operation failed", "instance", event.id, "phase", event.stage, "error", event.err)
		}
	}
}

func backoff(failures int) time.Duration {
	return time.Second << min(failures-1, 5)
}

func (r *reconciler) reconcile(ctx context.Context, now time.Time) (int, status.Instances) {
	for _, i := range r.instances {
		switch {
		case r.draining && (i.phase == starting || !now.Before(r.stopAt)):
			if i.cancel != nil {
				i.cancel()
			}
			if i.phase != reclaiming {
				i.phase, i.deadline = reclaiming, time.Time{}
			}
		case r.draining && i.phase == waiting && i.failures == 0:
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
	if r.connected && !r.draining && !hasRecovery(r.instances) && !now.Before(r.retryStart) {
		for len(r.instances) < r.update.TargetRunners && len(r.instances) < r.limit {
			id, err := uuid.NewRandom()
			if err != nil {
				r.logger.Error("Could not generate instance identity", "error", err)
				break
			}
			i := r.template
			i.ID, i.Resource = id.String(), runnerName(id.String())
			startCtx, cancel := context.WithTimeout(ctx, r.startupTimeout)
			r.instances[i.ID] = &instanceState{Instance: i, phase: starting, launching: true, cancel: cancel, deadline: now.Add(r.startupTimeout)}
			go r.launch(ctx, startCtx, i)
		}
		free = max(0, r.limit-len(r.instances))
		for _, i := range r.instances {
			if i.phase == waiting && !i.inFlight && i.failures == 0 {
				free++
			}
		}
	}
	var counts status.Instances
	for _, i := range r.instances {
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
	return free, counts
}

func (r *reconciler) launch(ctx, startup context.Context, i vm.Instance) {
	fail := func(stage string, err error) {
		_ = r.send(ctx, runnerEvent{kind: exited, id: i.ID, stage: stage, err: err})
	}
	if err := r.store.save(i); err != nil {
		fail("persist instance", err)
		return
	}
	if r.checkImage != nil {
		if err := r.checkImage(); err != nil {
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
	if r.checkImage != nil {
		if err := r.checkImage(); err != nil {
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
	_ = r.send(worker, runnerEvent{kind: cleaned, id: i.ID, stage: stage, err: err})
}

func (r *reconciler) reclaim(ctx context.Context, i vm.Instance) (string, error) {
	if err := r.backend.Stop(ctx, i); err != nil {
		return "stop VM", err
	}
	if err := r.backend.Delete(ctx, i); err != nil {
		return "delete VM", err
	}
	if err := r.registry.Remove(ctx, i.ScaleSetID, i.RunnerID, i.Resource); err != nil {
		return "remove runner registration", err
	}
	return "remove instance record", r.store.remove(i.ID)
}

func validateRestored(instances []vm.Instance, template vm.Instance) error {
	for _, i := range instances {
		if i.Scope != template.Scope || i.Backend != template.Backend || i.Namespace != template.Namespace {
			return fmt.Errorf("instance %s requires its original GitHub scope, VM backend and resource directory for recovery", i.ID)
		}
	}
	return nil
}
