package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"log/syslog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/service"
	"github.com/illusions-lab/Ephemera/internal/version"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ephemerad:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(version.String())
		return nil
	}
	if len(args) == 1 && args[0] == "--tart-manifest" {
		fmt.Print(macos.TartManifest())
		return nil
	}
	flags := flag.NewFlagSet("ephemerad", flag.ContinueOnError)
	path := flags.String("config", host.ConfigPath(), "configuration file")
	cacheDir := flags.String("image-cache", host.ImageCache(), "image cache directory")
	stateDir := flags.String("state-dir", host.StateDir(), "private runner instance directory")
	statusSocket := flags.String("status-socket", host.DaemonStatusSocket(), "Unix socket path for the read-only status endpoint")
	keyPath := flags.String("github-app-key", "", "override the GitHub App PEM path")
	activationID := flags.String("activation-id", "", "LaunchAgent activation identifier")
	channelFile := flags.String("image-channel-file", "", "read approved image recipes from this local file instead of the stable channel")
	logFile := flags.String("log-file", "", "write daemon logs to this rotating file")
	create := flags.Bool("create", false, "create the configured Scale Set if absent")
	claim := flags.Bool("claim", false, "acquire jobs for a test Scale Set; this command does not launch runners")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to ephemerad")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *logFile != "" {
		if err := os.MkdirAll(filepath.Dir(*logFile), 0700); err != nil {
			reportLogError(err)
			return err
		}
		output := &lumberjack.Logger{Filename: *logFile, MaxSize: 10, MaxBackups: 5, MaxAge: 14, Compress: true}
		defer output.Close()
		if _, err := output.Write(nil); err != nil {
			err = fmt.Errorf("open daemon log %s: %w", *logFile, err)
			reportLogError(err)
			return err
		}
		logger = slog.New(slog.NewTextHandler(logOutput{output}, nil))
	}
	err := service.Run(ctx, service.Options{ActivationID: *activationID, ImageChannelFile: *channelFile, ConfigPath: *path, CacheDir: *cacheDir, KeyPath: *keyPath, StatusSocket: *statusSocket, StateDir: *stateDir, Create: *create, Claim: *claim}, logger)
	if err != nil && *logFile != "" {
		logger.Error("Daemon stopped", "error", err)
	}
	return err
}

type logOutput struct {
	*lumberjack.Logger
}

func (output logOutput) Write(p []byte) (int, error) {
	n, err := output.Logger.Write(p)
	if err != nil {
		reportLogError(fmt.Errorf("write daemon log %s: %w", output.Filename, err))
	}
	return n, err
}

func reportLogError(err error) {
	fmt.Fprintln(os.Stderr, "ephemerad:", err)
	if systemLog, dialErr := syslog.New(syslog.LOG_DAEMON|syslog.LOG_ERR, "ephemerad"); dialErr == nil {
		defer systemLog.Close()
		_ = systemLog.Err(err.Error())
	}
}
