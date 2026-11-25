package service

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type listenFunc func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error

func execute(ctx context.Context, cfg config.Config, github orders.Config, namespace string, store instanceStore,
	backend vm.Backend, runners registry, logger *slog.Logger, listen listenFunc,
	prepare func() (string, func() error, error),
	report func(bool, bool, bool, orders.Update, status.Instances) error, reportRows func([]status.Instance), updates ...*imageUpdates,
) error {
	template := vm.Instance{
		Backend: cfg.VM.Backend, Namespace: namespace,
		Scope: github.ScopeURL, ScaleSet: github.ScaleSetName,
	}
	restored, err := store.load()
	if err != nil {
		return err
	}
	if err := validateRestored(restored, template); err != nil {
		return err
	}
	r := &reconciler{
		backend: backend, registry: runners, store: store, template: template,
		limit: github.MaxConcurrent, startupTimeout: cfg.Runner.StartupTimeout, idleTimeout: cfg.Runner.IdleTimeout,
		logger: logger, events: make(chan runnerEvent), instances: make(map[string]*instanceState), report: report, reportRows: reportRows,
	}
	if len(updates) > 0 {
		r.images = updates[0]
	}
	listenCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	var capacity atomic.Int64
	if github.Owner != "" {
		disk := uint64(max(12, cfg.VM.DiskGiB)) * gib
		root := cfg.VMDataPath(string(store))
		r.budget = &resourceBudget{root: root, disk: disk, diskReserve: uint64(cfg.VM.HostDiskReserveGiB) * gib, sample: availableDiskBytes}
		closeControl, err := control.Listen(listenCtx, string(store), r.serveDrain)
		if err != nil {
			return err
		}
		defer closeControl()
	}
	github.BeginAcquire = func() (func(), bool) {
		if ctx.Err() != nil || capacity.Load() == 0 || r.imageBlocked.Load() {
			return nil, false
		}
		return r.gate.begin()
	}
	github.Capacity = func() int {
		closed, _ := r.gate.state()
		if closed || ctx.Err() != nil || listenCtx.Err() != nil {
			return 0
		}
		return int(capacity.Load())
	}
	github.Disconnected = func() {
		capacity.Store(0)
		_ = r.submit(listenCtx, runnerEvent{kind: disconnected})
	}
	var done, updateDone, repairDone chan struct{}
	err = r.run(ctx, restored, func() error {
		imageID, checkImage, err := prepare()
		active := preparedImage{id: imageID, check: checkImage}
		if r.images != nil {
			active = r.images.recover(ctx, active)
		}
		if err != nil && active.recipe == nil {
			return err
		}
		if err != nil {
			logger.Warn("Use recovered runner image after bundled image preparation failed", "error", err)
		}
		r.setImage(active)
		if r.images != nil && r.images.state.Failures > 0 {
			state := r.images.state
			if state.FailureImage != "" && state.FailureImage != active.id {
				state.Failures, state.FailureImage = 0, ""
				if err := r.images.save(state); err != nil {
					logger.Error("Reset failures for a different base image", "error", err)
				}
			} else if state.Failures >= 3 {
				r.blockFailedImage()
			}
		}
		github.CheckImage = func() error {
			if r.imageBlocked.Load() {
				return errors.New("runner image is unavailable")
			}
			active := r.active.Load()
			if active.check == nil {
				return nil
			}
			err := active.check()
			if err != nil {
				_ = r.submit(listenCtx, runnerEvent{kind: imageUnavailable, source: active})
			}
			return err
		}
		if r.images != nil && r.images.rebuild != nil {
			repairDone = make(chan struct{})
			go func() {
				defer close(repairDone)
				r.monitorImage(listenCtx, r.images.rebuild)
			}()
		}
		if r.images != nil && r.images.enabled {
			updateDone = make(chan struct{})
			initial := r.images.state
			go func() { defer close(updateDone); r.images.watch(listenCtx, r, initial) }()
		}
		done = make(chan struct{})
		go func() {
			defer close(done)
			listenForExecution(listenCtx, github, logger, listen, r)
		}()
		return nil
	}, func(free int) { capacity.Store(int64(free)) })
	if done == nil {
		return err
	}
	stop()
	if repairDone != nil {
		<-repairDone
	}
	if updateDone != nil {
		<-updateDone
	}
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	return err
}

func listenForExecution(ctx context.Context, cfg orders.Config, logger *slog.Logger, listen listenFunc, r *reconciler) {
	failures := 0
	cfg.Acquisitions = &orders.Acquisitions{}
	cfg.Polled = func() { failures = 0 }
	for ctx.Err() == nil {
		err := listen(ctx, cfg, logger, func(ctx context.Context, update orders.Update) error {
			if update.ScaleSetID <= 0 {
				return errors.New("GitHub returned an invalid Scale Set ID")
			}
			return r.submit(ctx, runnerEvent{kind: statistics, update: update})
		})
		cfg.Disconnected()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Warn("GitHub Scale Set session ended", "error", err)
		}
		failures++
		timer := time.NewTimer(backoff(failures))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
