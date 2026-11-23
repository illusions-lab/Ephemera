package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/illusions-lab/Ephemera/internal/backend"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
)

type Options struct {
	ConfigPath, CacheDir, KeyPath, StatusSocket, StateDir string
	ImageChannelFile                                      string
	ActivationID                                          string
	Create, Claim                                         bool
}

func Run(ctx context.Context, opts Options, logger *slog.Logger) error {
	return run(ctx, opts, logger, backend.BuildImage, orders.Listen)
}
func run(ctx context.Context, opts Options, logger *slog.Logger,
	prepare backend.ImageBuilder,
	listen func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error,
) (result error) {
	current := status.Snapshot{ActivationID: opts.ActivationID, Phase: "Starting", GitHub: "pending", Intake: "observing"}
	if opts.Claim {
		current.Intake = "test-claim"
	}
	var published atomic.Pointer[status.Snapshot]
	publish := func() {
		snapshot := current
		published.Store(&snapshot)
	}
	publish()
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
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	current.ConfigDigest, err = config.Digest(cfg)
	if err != nil {
		return err
	}
	current.Scope, current.ScaleSet = cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName
	current.MaxConcurrent = cfg.Runner.MaxConcurrent
	current.Backend = cfg.VM.Backend
	current.ImageSource = cfg.VM.ImageRef
	if cfg.Runner.Execute && opts.Claim {
		return errors.New("runner.execute cannot be combined with --claim")
	}
	root := opts.StateDir
	if root == "" {
		root = host.StateDir()
	}
	var store instanceStore
	if cfg.Runner.Execute {
		current.Intake = "executing"
	}
	if cfg.Runner.Execute || opts.StatusSocket != "" {
		var unlock func()
		store, unlock, err = openInstances(root)
		if err != nil {
			return err
		}
		defer unlock()
	}
	selected, err := backend.Select(cfg, root, opts.CacheDir, logger, prepare, opts.Claim)
	if err != nil {
		return err
	}
	if opts.StatusSocket != "" {
		closeStatus, err := host.ListenStatus(opts.StatusSocket, string(store), func() status.Snapshot { return *published.Load() })
		if err != nil {
			return err
		}
		defer closeStatus()
	}
	if err := report("Starting"); err != nil {
		return err
	}
	keyPath := opts.KeyPath
	if keyPath == "" {
		keyPath = host.DaemonKeyPath()
	}
	key, err := config.ReadKey(keyPath)
	if err != nil {
		return fmt.Errorf("load GitHub App credential: %w", err)
	}
	owner, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("determine session owner: %w", err)
	}
	prepareCurrent := func() (string, func() error, error) {
		if cfg.Runner.Execute {
			current.Instances = &status.Instances{}
		}
		if err := report("PreparingImage"); err != nil {
			return "", nil, err
		}
		id, check, err := selected.PrepareImage(ctx)
		if err == nil && cfg.Runner.Execute {
			err = check()
		}
		if err != nil {
			return "", nil, fmt.Errorf("prepare runner image: %w", err)
		}
		current.GitHub = "connecting"
		return id, check, report("Connecting")
	}
	if opts.Claim {
		logger.Warn("Acquiring test jobs without launching runners")
	}
	orderConfig := orders.Config{
		ScopeURL:       cfg.GitHub.ScopeURL,
		ScaleSetName:   cfg.GitHub.ScaleSetName,
		RunnerGroup:    cfg.RunnerGroup(),
		Labels:         cfg.ScaleSetLabels(),
		ReplaceLabels:  cfg.GitHub.Labels != nil,
		AppID:          cfg.GitHub.AppID,
		InstallationID: cfg.GitHub.InstallationID,
		PrivateKey:     key,
		MaxConcurrent:  cfg.Runner.MaxConcurrent,
		Owner:          owner,
		Create:         opts.Create,
		Claim:          opts.Claim,
	}
	if cfg.Runner.Execute {
		if err := report("Recovering"); err != nil {
			return err
		}
		runners, err := orders.NewRunners(orderConfig)
		if err != nil {
			return err
		}
		var pendingHealth bool
		var nextHealthAttempt time.Time
		var managedCache []string
		if root == host.StateDir() && opts.CacheDir == host.ImageCache() {
			managedCache = append(managedCache, opts.CacheDir)
		}
		updates := newImageUpdates(cfg, root, selected, logger, prepare, opts.ImageChannelFile, managedCache...)
		if runtime.GOOS == "darwin" && opts.ActivationID != "" && root == host.StateDir() && cfg.VM.Backend == "tart" {
			updates.manualSource = cfg.VM.ImageRef
		}
		return execute(ctx, cfg, orderConfig, selected.Namespace, store, selected.VM, runners, logger, listen, prepareCurrent,
			func(connected, recovering, stopping bool, update orders.Update, counts status.Instances) error {
				current.Instances = &counts
				current.ImageUpdate = updates.snapshot()
				current.GitHub = "disconnected"
				if connected {
					current.GitHub = "connected"
				}
				if !update.DemandObservedAt.IsZero() {
					current.Demand = snapshotDemand(update)
					current.RunnerGroup, current.Labels = update.RunnerGroup, update.Labels
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
					current.Phase = phase
					pendingHealth = true
					nextHealthAttempt = time.Time{}
				}
				publish()
				if !pendingHealth || time.Now().Before(nextHealthAttempt) {
					return nil
				}
				if err := reportHealth(logger, phase, ""); err != nil {

					logger.Warn("Retry service health notification", "phase", phase, "error", err)
					nextHealthAttempt = time.Now().Add(5 * time.Second)
					return nil
				}
				pendingHealth = false
				return nil
			}, func(rows []status.Instance) { current.InstanceRows = rows }, updates)
	}
	if store != "" && !opts.Claim {
		closeControl, err := control.Listen(ctx, string(store), func(ctx context.Context, requests <-chan control.Request, replies chan<- control.Snapshot) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-requests:
					select {
					case <-ctx.Done():
						return
					case replies <- control.Snapshot{Ready: true}:
					}
				}
			}
		})
		if err != nil {
			return err
		}
		defer closeControl()
	}
	_, checkImage, err := prepareCurrent()
	if err != nil {
		return err
	}
	orderConfig.CheckImage = checkImage
	healthy := false
	return listen(ctx, orderConfig, logger, func(_ context.Context, update orders.Update) error {
		current.Demand = snapshotDemand(update)
		current.RunnerGroup, current.Labels = update.RunnerGroup, update.Labels
		if !healthy {
			if checkImage != nil {
				if err := checkImage(); err != nil {
					return err
				}
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

func snapshotDemand(update orders.Update) *status.Demand {
	return &status.Demand{
		Available: update.AvailableJobs, Assigned: update.AssignedJobs, Running: update.RunningJobs,
		TargetRunners: update.TargetRunners, UpdatedAt: update.DemandObservedAt,
	}
}
