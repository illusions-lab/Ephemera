package service

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type backendSelection struct {
	backend      vm.Backend
	namespace    string
	prepareImage func(context.Context) (string, func() error, error)
}

func selectBackend(cfg config.Config, root, cache string, logger *slog.Logger,
	prepare func(context.Context, string, image.Config, *slog.Logger) (string, func() error, error),
) (backendSelection, error) {
	if cfg.VM.Backend == "tart" {
		if !cfg.Runner.Execute {
			return backendSelection{prepareImage: func(context.Context) (string, func() error, error) {
				return cfg.VM.ImageRef, nil, nil
			}}, nil
		}
		home := filepath.Join(root, "tart")
		backend, err := vm.NewTart(home, cfg.VM.VCPUs, cfg.VM.MemoryMiB)
		if err != nil {
			return backendSelection{}, err
		}
		return backendSelection{backend: backend, namespace: home, prepareImage: func(ctx context.Context) (string, func() error, error) {
			return backend.ResolveImage(ctx, cfg.VM.ImageRef)
		}}, nil
	}

	result := backendSelection{prepareImage: func(ctx context.Context) (string, func() error, error) {
		imageCfg, err := cfg.ImageConfig()
		if err != nil {
			return "", nil, err
		}
		path, check, err := prepare(ctx, cache, imageCfg, logger)
		if err != nil {
			return "", nil, err
		}
		path, err = filepath.Abs(path)
		return path, check, err
	}}
	if cfg.Runner.Execute {
		backend, err := vm.NewQEMU(vm.QEMUConfig{Root: root, VCPUs: cfg.VM.VCPUs, MemoryMiB: cfg.VM.MemoryMiB})
		if err != nil {
			return backendSelection{}, err
		}
		result.backend = backend
	}
	return result, nil
}
