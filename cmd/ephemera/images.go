package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/illusions-lab/Ephemera/internal/images"
)

const defaultImageCache = "/var/lib/ephemera/images"

func prepareImage(args []string) error {
	flags := flag.NewFlagSet("images prepare", flag.ContinueOnError)
	path := flags.String("config", filepath.Join(configDir, "config.yaml"), "configuration file")
	cacheDir := flags.String("image-cache", defaultImageCache, "image cache directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to images prepare")
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	image, err := cfg.prepareImage(ctx, *cacheDir, logger)
	if err != nil {
		return err
	}
	fmt.Println(image.Path)
	return nil
}

func (cfg config) prepareImage(ctx context.Context, cacheDir string, logger *slog.Logger) (images.Image, error) {
	platform := images.Platform{Tag: cfg.VM.Image, Arch: cfg.VM.Arch}
	if cfg.VM.Arch == "" {
		return images.Image{}, errors.New("configure vm.image and vm.arch before starting managed image preparation")
	}
	source, configured := cfg.Images[platform.Tag][platform.Arch]
	if !configured {
		var err error
		source, err = images.DefaultSource(platform)
		if err != nil {
			return images.Image{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return images.Prepare(ctx, cacheDir, images.Config{
		Platform: platform, Source: source, VCPUs: cfg.VM.VCPUs, MemoryMiB: cfg.VM.MemoryMiB,
	}, logger)
}

func reportHealth(logger *slog.Logger, state string) error {
	if socket := os.Getenv("NOTIFY_SOCKET"); socket != "" {
		conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
		if err != nil {
			return fmt.Errorf("notify systemd: %w", err)
		}
		defer conn.Close()
		message := "STATUS=" + state
		switch state {
		case "Healthy":
			message += "\nREADY=1"
		case "Stopping":
			message += "\nSTOPPING=1"
		}
		if _, err := conn.Write([]byte(message)); err != nil {
			return fmt.Errorf("notify systemd: %w", err)
		}
	}
	logger.Info("Service health", "state", state)
	return nil
}
