package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type Options struct {
	ConfigPath, CacheDir, KeyPath, StatusSocket, StateDir string
	Create, Claim                                         bool
}

func Run(ctx context.Context, opts Options, logger *slog.Logger) error {
	return run(ctx, opts, logger, prepareImage, orders.Listen)
}
func run(ctx context.Context, opts Options, logger *slog.Logger,
	prepare func(context.Context, string, image.Config, *slog.Logger) (string, func() error, error),
	listen func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error,
) (result error) {
	current := status.Snapshot{Phase: "Starting", GitHub: "pending", Intake: "observing"}
	if opts.Claim {
		current.Intake = "test-claim"
	}
	var published atomic.Pointer[status.Snapshot]
	publish := func() {
		snapshot := current
		published.Store(&snapshot)
	}
	publish()
	if opts.StatusSocket != "" {
		closeStatus, err := status.Listen(opts.StatusSocket, func() status.Snapshot {
			return *published.Load()
		})
		if err != nil {
			return err
		}
		defer closeStatus()
	}
	report := func(phase string) error {
		current.Phase = phase
		publish()
		return reportHealth(logger, phase, "")
	}
	defer func() {
		state := "Stopping"
		if ctx.Err() != nil && errors.Is(result, ctx.Err()) {
			result = nil
		}
		if result != nil {
			state = "Degraded"
			current.FailedPhase = current.Phase
			if current.Phase == "Connecting" || current.Phase == "Healthy" {
				current.GitHub = "disconnected"
			}
			if current.Phase == "Healthy" {
				current.FailedPhase = "Polling"
			}
		}
		if current.Phase == state {
			return
		}
		current.Phase = state
		publish()
		if err := reportHealth(logger, state, current.FailedPhase); err != nil {
			logger.Warn("Could not report service health", "state", state, "error", err)
		}
	}()
	if err := report("Starting"); err != nil {
		return err
	}
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	current.Scope, current.ScaleSet = cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName
	current.MaxConcurrent = cfg.Runner.MaxConcurrent
	current.Backend = cfg.VM.Backend
	if cfg.Runner.Execute && opts.Claim {
		return errors.New("runner.execute cannot be combined with --claim")
	}
	if cfg.Runner.Execute && runtime.GOOS == "linux" && cfg.VM.Arch != runtime.GOARCH {
		return fmt.Errorf("execution guest architecture %s must match host architecture %s", cfg.VM.Arch, runtime.GOARCH)
	}
	var store instanceStore
	var backend vm.Backend
	if cfg.Runner.Execute {
		current.Intake = "executing"
		root := opts.StateDir
		if root == "" {
			root = DefaultStateDir
		}
		var unlock func()
		store, unlock, err = openInstances(root)
		if err != nil {
			return err
		}
		defer unlock()
		backend, err = vm.NewQEMU(vm.QEMUConfig{Root: root, VCPUs: cfg.VM.VCPUs, MemoryMiB: cfg.VM.MemoryMiB})
		if err != nil {
			return err
		}
		if err := backend.Check(ctx); err != nil {
			return fmt.Errorf("check QEMU backend: %w", err)
		}
	}
	publish()
	keyPath := opts.KeyPath
	if keyPath == "" {
		dir := os.Getenv("CREDENTIALS_DIRECTORY")
		if dir != "" {
			keyPath = filepath.Join(dir, "github-app-key")
		} else {
			keyPath = filepath.Join(config.Dir, "credentials", "github-app.pem")
		}
	}
	key, err := config.ReadKey(keyPath)
	if err != nil {
		return fmt.Errorf("load GitHub App credential: %w", err)
	}
	owner, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("determine session owner: %w", err)
	}
	if err := report("PreparingImage"); err != nil {
		return err
	}
	imageCfg, err := cfg.ImageConfig()
	if err != nil {
		return fmt.Errorf("prepare runner image: %w", err)
	}
	imagePath, checkImage, err := prepare(ctx, opts.CacheDir, imageCfg, logger)
	if err != nil {
		return fmt.Errorf("prepare runner image: %w", err)
	}
	logger.Info("Runner image ready", "path", imagePath)
	current.GitHub = "connecting"
	if err := report("Connecting"); err != nil {
		return err
	}
	if opts.Claim {
		logger.Warn("Acquiring test jobs without launching runners")
	}
	orderConfig := orders.Config{
		ScopeURL:       cfg.GitHub.ScopeURL,
		ScaleSetName:   cfg.GitHub.ScaleSetName,
		AppID:          cfg.GitHub.AppID,
		InstallationID: cfg.GitHub.InstallationID,
		PrivateKey:     key,
		MaxConcurrent:  cfg.Runner.MaxConcurrent,
		Owner:          owner,
		Create:         opts.Create,
		Claim:          opts.Claim,
		CheckImage:     checkImage,
	}
	if cfg.Runner.Execute {
		if err := checkImage(); err != nil {
			return err
		}
		return execute(ctx, cfg, orderConfig, imagePath, store, backend, logger, listen,
			func(connected, recovering, stopping bool, update orders.Update, counts status.Instances) error {
				current.Instances = &counts
				current.GitHub = "disconnected"
				if connected {
					current.GitHub = "connected"
				}
				if !update.DemandObservedAt.IsZero() {
					current.Demand = snapshotDemand(update)
				}
				phase := "Connecting"
				switch {
				case stopping:
					phase = "Stopping"
				case recovering:
					phase = "Recovering"
				case connected:
					phase = "Healthy"
				case current.Demand != nil:
					phase = "Reconnecting"
				}
				if current.Phase != phase {
					return report(phase)
				}
				publish()
				return nil
			})
	}
	healthy := false
	return listen(ctx, orderConfig, logger, func(_ context.Context, update orders.Update) error {
		current.Demand = snapshotDemand(update)
		if !healthy {
			if err := checkImage(); err != nil {
				return err
			}
			current.GitHub = "connected"
			if err := report("Healthy"); err != nil {
				return err
			}
			healthy = true
		}
		publish()
		logger.Info("Runner demand",
			"available_jobs", update.AvailableJobs,
			"assigned_jobs", update.AssignedJobs,
			"running_jobs", update.RunningJobs,
			"target_runners", update.TargetRunners,
		)
		for _, id := range update.Acquired {
			logger.Info("Job acquired", "request_id", id)
		}
		for _, job := range update.Assigned {
			logger.Info("Job assigned", "request_id", job.RunnerRequestID)
		}
		for _, job := range update.Started {
			logger.Info("Job started", "request_id", job.RunnerRequestID, "runner", job.RunnerName)
		}
		for _, job := range update.Completed {
			logger.Info("Job completed", "request_id", job.RunnerRequestID, "runner", job.RunnerName, "result", job.Result)
		}
		return nil
	})
}

func prepareImage(ctx context.Context, cache string, cfg image.Config, logger *slog.Logger) (string, func() error, error) {
	prepared, err := image.Prepare(ctx, cache, cfg, logger)
	return prepared.Path, prepared.Check, err
}

func snapshotDemand(update orders.Update) *status.Demand {
	return &status.Demand{
		Available: update.AvailableJobs, Assigned: update.AssignedJobs, Running: update.RunningJobs,
		TargetRunners: update.TargetRunners, UpdatedAt: update.DemandObservedAt,
	}
}
