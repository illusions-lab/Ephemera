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

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/service"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ephemerad:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	flags := flag.NewFlagSet("ephemerad", flag.ContinueOnError)
	path := flags.String("config", filepath.Join(config.Dir, "config.yaml"), "configuration file")
	cacheDir := flags.String("image-cache", image.DefaultCache, "image cache directory")
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
		return errors.New("unexpected arguments to ephemerad")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return service.Run(ctx, service.Options{ConfigPath: *path, CacheDir: *cacheDir, KeyPath: *keyPath, Create: *create, Claim: *claim}, logger)
}
