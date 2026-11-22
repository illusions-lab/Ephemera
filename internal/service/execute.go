package service

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type listenFunc func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error

func execute(ctx context.Context, cfg config.Config, github orders.Config, namespace string, store instanceStore,
	backend vm.Backend, runners registry, logger *slog.Logger, listen listenFunc,
	prepare func() (string, func() error, error),
	report func(bool, bool, bool, orders.Update, status.Instances) error,
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
		logger: logger, events: make(chan runnerEvent), instances: make(map[string]*instanceState), report: report,
	}
	listenCtx, stop := context.WithCancel(ctx)
	defer stop()
	var capacity atomic.Int64
	github.Capacity = func() int {
		if listenCtx.Err() != nil {
			return 0
		}
		return int(capacity.Load())
	}
	github.Disconnected = func() {
		capacity.Store(0)
		_ = r.submit(listenCtx, runnerEvent{kind: disconnected})
	}
	var done chan struct{}
	err = r.run(ctx, restored, func() error {
		imageID, checkImage, err := prepare()
		if err != nil {
			return err
		}
		r.template.Image, r.checkImage, github.CheckImage = imageID, checkImage, checkImage
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
	for ctx.Err() == nil {
		err := listen(ctx, cfg, logger, func(ctx context.Context, update orders.Update) error {
			if update.ScaleSetID <= 0 {
				return errors.New("GitHub returned an invalid Scale Set ID")
			}
			failures = 0
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
