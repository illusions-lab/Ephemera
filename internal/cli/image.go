package cli

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

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
)

func prepareImage(args []string) error {
	flags := flag.NewFlagSet("image prepare", flag.ContinueOnError)
	path := flags.String("config", filepath.Join(config.Dir, "config.yaml"), "configuration file")
	cacheDir := flags.String("image-cache", image.DefaultCache, "image cache directory")
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
	imageCfg, err := cfg.ImageConfig()
	if err != nil {
		return err
	}
	prepared, err := image.Prepare(ctx, *cacheDir, imageCfg, logger)
	if err != nil {
		return err
	}
	fmt.Println(prepared.Path)
	return nil
}
