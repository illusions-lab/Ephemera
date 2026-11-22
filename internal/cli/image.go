package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/illusions-lab/Ephemera/internal/backend"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
)

func prepareImage(args []string) error {
	flags := flag.NewFlagSet("image prepare", flag.ContinueOnError)
	path := flags.String("config", host.ConfigPath(), "configuration file")
	cacheDir := flags.String("image-cache", host.ImageCache(), "image cache directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to image prepare")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	prepared, err := backend.PrepareImage(ctx, cfg, host.StateDir(), *cacheDir, logger, os.Stderr)
	if err != nil {
		return err
	}
	fmt.Println(prepared)
	return nil
}
