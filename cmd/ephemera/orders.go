package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/illusions-lab/Ephemera/internal/orders"
)

func listenOrders(args []string) (result error) {
	flags := flag.NewFlagSet("orders listen", flag.ContinueOnError)
	path := flags.String("config", filepath.Join(configDir, "config.yaml"), "configuration file")
	cacheDir := flags.String("image-cache", defaultImageCache, "image cache directory")
	keyPath := flags.String("github-app-key", "", "override the GitHub App PEM path")
	create := flags.Bool("create", false, "create the configured Scale Set if absent")
	claim := flags.Bool("claim", false, "acquire jobs for a test Scale Set; this command does not launch runners")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to orders listen")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
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
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if *keyPath == "" {
		dir := os.Getenv("CREDENTIALS_DIRECTORY")
		if dir != "" {
			*keyPath = filepath.Join(dir, "github-app-key")
		} else {
			*keyPath = filepath.Join(configDir, "credentials", "github-app.pem")
		}
	}
	key, err := readKey(*keyPath)
	if err != nil {
		return fmt.Errorf("load GitHub App credential: %w", err)
	}
	owner, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("determine session owner: %w", err)
	}
	if err := reportHealth(logger, "PreparingImages"); err != nil {
		return err
	}
	image, err := cfg.prepareImage(ctx, *cacheDir, logger)
	if err != nil {
		return fmt.Errorf("prepare runner image: %w", err)
	}
	logger.Info("Runner image ready", "path", image.Path)
	if err := reportHealth(logger, "Connecting"); err != nil {
		return err
	}
	if *claim {
		logger.Warn("Acquiring test jobs without launching runners")
	}
	healthy := false
	return orders.Listen(ctx, orders.Config{
		ScopeURL:       cfg.GitHub.ScopeURL,
		ScaleSetName:   cfg.GitHub.ScaleSetName,
		AppID:          cfg.GitHub.AppID,
		InstallationID: cfg.GitHub.InstallationID,
		PrivateKey:     key,
		MaxConcurrent:  cfg.Runner.MaxConcurrent,
		Owner:          owner,
		Create:         *create,
		Claim:          *claim,
		CheckImage:     image.Check,
	}, logger, func(_ context.Context, update orders.Update) error {
		if !healthy {
			if err := image.Check(); err != nil {
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
