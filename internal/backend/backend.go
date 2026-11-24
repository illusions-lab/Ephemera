// Package backend selects a VM implementation and its image operations.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

type ImageBuilder func(context.Context, string, image.Config, *slog.Logger) (string, func() error, error)

type Selection struct {
	VM           vm.Backend
	Namespace    string
	PrepareImage func(context.Context) (string, func() error, error)
}

func Select(cfg config.Config, root, cache string, logger *slog.Logger, build ImageBuilder, claim bool) (Selection, error) {
	cache = cfg.VMImageCachePath(cache)
	switch cfg.VM.Backend {
	case "tart":
		if claim {
			return Selection{}, errors.New("--claim requires the QEMU test path")
		}
		if !cfg.Runner.Execute {
			return Selection{PrepareImage: func(context.Context) (string, func() error, error) {
				return cfg.VM.ImageRef, nil, nil
			}}, nil
		}
		tart, err := newTart(cfg, root)
		if err != nil {
			return Selection{}, err
		}
		return Selection{VM: tart, Namespace: cfg.VMDataPath(root), PrepareImage: func(ctx context.Context) (string, func() error, error) {
			return prepareTart(ctx, tart, root, cfg.VM.ImageRef, cfg.VM.AppleToolchain, true, nil)
		}}, nil
	case "qemu":
		var qemu vm.Backend
		dataDir := cfg.VMDataPath(root)
		namespace := ""
		if dataDir != root {
			namespace = dataDir
		}
		if cfg.Runner.Execute {
			if err := checkQEMUHost(runtime.GOOS, runtime.GOARCH, cfg.VM.Arch); err != nil {
				return Selection{}, err
			}
			if cfg.VM.DataDir != "" {
				if err := checkPrivateStorage(dataDir); err != nil {
					return Selection{}, err
				}
			}
			var err error
			qemu, err = vm.NewQEMU(vm.QEMUConfig{Root: dataDir, VCPUs: cfg.VM.VCPUs, MemoryMiB: cfg.VM.MemoryMiB})
			if err != nil {
				return Selection{}, err
			}
		}
		return Selection{VM: qemu, Namespace: namespace, PrepareImage: func(ctx context.Context) (string, func() error, error) {
			if qemu != nil {
				if err := qemu.Check(ctx); err != nil {
					return "", nil, fmt.Errorf("check QEMU backend: %w", err)
				}
			}
			return prepareQEMU(ctx, cfg, cache, logger, build)
		}}, nil
	default:
		return Selection{}, fmt.Errorf("unknown VM backend %q", cfg.VM.Backend)
	}
}

func BuildImage(ctx context.Context, cache string, cfg image.Config, logger *slog.Logger) (string, func() error, error) {
	prepared, err := image.Prepare(ctx, cache, cfg, logger)
	return prepared.Path, prepared.Check, err
}

func prepareQEMU(ctx context.Context, cfg config.Config, cache string, logger *slog.Logger, build ImageBuilder) (string, func() error, error) {
	if cfg.VM.ImageCacheDir != "" {
		if err := checkPrivateStorage(cache); err != nil {
			return "", nil, err
		}
	}
	imageCfg, err := cfg.ImageConfig()
	if err != nil {
		return "", nil, err
	}
	path, check, err := build(ctx, cache, imageCfg, logger)
	if err != nil {
		return "", nil, err
	}
	path, err = filepath.Abs(path)
	return path, check, err
}

func newTart(cfg config.Config, root string) (*vm.Tart, error) {
	if cfg.VM.DataDir != "" {
		if err := checkPrivateStorage(cfg.VM.DataDir); err != nil {
			return nil, err
		}
	}
	return vm.NewTart(cfg.VMDataPath(root), cfg.VM.VCPUs, cfg.VM.MemoryMiB, cfg.VM.DiskGiB)
}

func prepareTart(ctx context.Context, tart *vm.Tart, root, ref, toolchain string, execute bool, progress io.Writer) (string, func() error, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	if err := tart.Check(ctx); err != nil {
		return "", nil, fmt.Errorf("check Tart backend: %w", err)
	}
	if !execute {
		return tart.ResolveImage(ctx, ref, progress)
	}
	return tart.PrepareImage(ctx, ref, vm.TartRunnerVersion, vm.TartRunnerSHA256, toolchain, progress)
}

// PrepareImage explicitly prepares and verifies the configured image. Tart
// observation skips preparation in the daemon, while an explicit request pulls it.
func PrepareImage(ctx context.Context, cfg config.Config, root, cache string, logger *slog.Logger, progress io.Writer) (string, error) {
	cache = cfg.VMImageCachePath(cache)
	var id string
	var verify func() error
	var err error
	switch cfg.VM.Backend {
	case "qemu":
		id, verify, err = prepareQEMU(ctx, cfg, cache, logger, BuildImage)
	case "tart":
		var tart *vm.Tart
		tart, err = newTart(cfg, root)
		if err != nil {
			return "", err
		}
		id, verify, err = prepareTart(ctx, tart, root, cfg.VM.ImageRef, cfg.VM.AppleToolchain, cfg.Runner.Execute, progress)
	default:
		return "", fmt.Errorf("unknown VM backend %q", cfg.VM.Backend)
	}
	if err != nil {
		return "", err
	}
	return id, verify()
}

// CheckImage verifies current image availability without pulling or rebuilding it.
func CheckImage(ctx context.Context, cfg config.Config, root, cache string) (string, error) {
	switch cfg.VM.Backend {
	case "qemu":
		imageCfg, err := cfg.ImageConfig()
		if err != nil {
			return "", err
		}
		cached, err := image.CheckCached(ctx, cfg.VMImageCachePath(cache), imageCfg)
		return cached.Path, err
	case "tart":
		tart, err := newTart(cfg, root)
		if err != nil {
			return "", err
		}
		if err := tart.CheckInstalled(ctx); err != nil {
			return "", err
		}
		if cfg.Runner.Execute {
			return tart.CheckPreparedImage(ctx, cfg.VM.ImageRef, vm.TartRunnerVersion, vm.TartRunnerSHA256, cfg.VM.AppleToolchain)
		}
		return cfg.VM.ImageRef, tart.CheckImage(ctx, cfg.VM.ImageRef)
	default:
		return "", fmt.Errorf("unknown VM backend %q", cfg.VM.Backend)
	}
}

func checkQEMUHost(goos, arch, guest string) error {
	if goos != "linux" || (arch != "amd64" && arch != "arm64") {
		return errors.New("QEMU execution requires Linux amd64 or arm64")
	}
	if guest != arch {
		return fmt.Errorf("execution guest architecture %s must match host architecture %s", guest, arch)
	}
	return nil
}

func checkPrivateStorage(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("open configured VM storage %s (create it under the service account first): %w", path, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("configured VM storage %s must be a private directory (0700)", path)
	}
	return nil
}
