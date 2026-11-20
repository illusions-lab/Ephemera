package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/orders"
)

type Options struct {
	ConfigPath, CacheDir, KeyPath string
	Create, Claim                 bool
}

func Run(ctx context.Context, opts Options, logger *slog.Logger) error {
	return run(ctx, opts, logger, prepareImage, orders.Listen)
}
func run(ctx context.Context, opts Options, logger *slog.Logger,
	prepare func(context.Context, string, image.Config, *slog.Logger) (string, func() error, error),
	listen func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error,
) (result error) {
	defer func() {
		state := "Stopping"
		if ctx.Err() != nil && errors.Is(result, ctx.Err()) {
			result = nil
		}
		if result != nil {
			state = "Degraded"
		}
		if err := reportHealth(logger, state); err != nil {
			logger.Warn("Could not report service health", "state", state, "error", err)
		}
	}()
	if err := reportHealth(logger, "Starting"); err != nil {
		return err
	}
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
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
	if err := reportHealth(logger, "PreparingImage"); err != nil {
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
	if err := reportHealth(logger, "Connecting"); err != nil {
		return err
	}
	if opts.Claim {
		logger.Warn("Acquiring test jobs without launching runners")
	}
	healthy := false
	return listen(ctx, orders.Config{
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
	}, logger, func(_ context.Context, update orders.Update) error {
		if !healthy {
			if err := checkImage(); err != nil {
				return err
			}
			if err := reportHealth(logger, "Healthy"); err != nil {
				return err
			}
			healthy = true
		}
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
