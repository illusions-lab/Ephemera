package service

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/illusions-lab/Ephemera/internal/control"
)

type intakeGate struct {
	mu                sync.Mutex
	closed, acquiring bool
}

func (g *intakeGate) begin() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.acquiring {
		return nil, false
	}
	g.acquiring = true
	return func() {
		g.mu.Lock()
		g.acquiring = false
		g.mu.Unlock()
	}, true
}

func (g *intakeGate) state() (closed, acquiring bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed, g.acquiring
}

func (r *reconciler) serveDrain(ctx context.Context, requests <-chan control.Request, replies chan<- control.Snapshot) {
	r.gate.mu.Lock()
	r.gate.closed = true
	r.gate.mu.Unlock()
	defer func() {
		r.gate.mu.Lock()
		r.gate.closed = false
		r.gate.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-requests:
			reply := make(chan control.Snapshot, 1)
			if r.send(ctx, runnerEvent{kind: drainStatus, drain: request, drainReply: reply}) != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case snapshot := <-reply:
				select {
				case <-ctx.Done():
					return
				case replies <- snapshot:
				}
			}
		}
	}
}

func (r *reconciler) drainSnapshot(request control.Request) control.Snapshot {
	_, pending := r.gate.state()
	snapshot := control.Snapshot{Pending: pending}
	if pending {
		snapshot.Changed = len(request.Abort) > 0
		return snapshot
	}
	for _, instance := range r.instances {
		if instance.LocalReclaimed || instance.phase >= finishing {
			continue
		}
		job := instance.job
		job.Instance = instance.ID
		if job.Runner == "" {
			job.Runner = instance.Resource
		}
		snapshot.Jobs = append(snapshot.Jobs, job)
	}
	sort.Slice(snapshot.Jobs, func(i, j int) bool { return snapshot.Jobs[i].Instance < snapshot.Jobs[j].Instance })
	snapshot.Ready = r.localInstances() == 0
	if len(request.Abort) == 0 {
		return snapshot
	}
	view := make([]control.Identity, 0, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		view = append(view, job.Identity)
	}
	if !slices.Equal(view, request.View) {
		snapshot.Changed = true
		return snapshot
	}
	for _, id := range request.Abort {
		if id.Request <= 0 || !slices.Contains(view, id) {
			snapshot.Changed = true
			return snapshot
		}
	}
	for _, id := range request.Abort {
		i := r.instances[id.Instance]
		i.phase = reclaiming
		i.deadline = time.Time{}
		if i.cancel != nil {
			i.cancel()
		}
	}
	return snapshot
}
