package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"github.com/illusions-lab/Ephemera/internal/backend"
	"github.com/illusions-lab/Ephemera/internal/cli/setup"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

var macSetupProbe = probeMacSetup

func runMacSetup(automatic, resume bool, source, key, appName string, existingApp bool) error {
	if os.Geteuid() == 0 {
		return errors.New("run macOS setup as your logged-in user")
	}
	if err := os.MkdirAll(macos.Root(), 0700); err != nil {
		return err
	}
	var info unix.Stat_t
	if err := unix.Lstat(macos.Root(), &info); err != nil {
		return err
	}
	if info.Mode != unix.S_IFDIR|0700 || info.Uid != uint32(os.Geteuid()) {
		return errors.New("Ephemera needs a private user-owned data directory (0700)")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	release := filepath.Dir(executable)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !automatic && !resume {
		current, currentErr := filepath.EvalSymlinks(filepath.Join(macos.Root(), "current"))
		if currentErr == nil && current == release {
			if _, err := os.Stat(host.ConfigPath()); err == nil {
				if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
					return runConfigApply([]string{"--yes"})
				}
				return runConfigApply(nil)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 55*time.Minute)
	defer cancel()
	return macos.Activate(ctx, release, func() (*macos.Candidate, error) {
		return prepareMacActivation(ctx, release, automatic, resume, source, key, appName, existingApp)
	})
}

func prepareMacActivation(ctx context.Context, release string, automatic, resume bool, source, keyPath, appName string, existingApp bool) (*macos.Candidate, error) {
	path := filepath.Join(macos.Root(), "config.yaml")
	profile := filepath.Join(macos.Root(), "server-profile")
	serverTool := filepath.Join(release, "server-config.sh")
	for _, name := range []string{serverTool, filepath.Join(release, "ephemerad")} {
		if _, err := os.Stat(name); err != nil {
			return nil, fmt.Errorf("open installed macOS setup tools (run install-macos.sh first): %w", err)
		}
	}
	choice, err := os.ReadFile(profile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && string(choice) != "personal\n" && string(choice) != "server\n" {
		return nil, fmt.Errorf("invalid setup profile in %s", profile)
	}
	server := string(choice) == "server\n"
	validate := func(cfg config.Config, pem []byte) error { return macSetupProbe(ctx, cfg, pem) }
	_, statErr := os.Lstat(path)
	initial := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !initial {
		return nil, statErr
	}
	if automatic && resume {
		if err := setupFromValidatedFiles(macos.Root(), source, keyPath, true, nil); err != nil {
			return nil, err
		}
	}
	var cfg config.Config
	var key []byte
	switch {
	case automatic && !resume:
		cfg, err = config.Load(source)
		if err == nil {
			key, err = config.ReadKey(keyPath)
		}
		if err == nil {
			err = validate(cfg, key)
		}
	case initial:
		if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
			return nil, errors.New("setup requires a terminal or --non-interactive with input files")
		}
		if len(choice) == 0 {
			preview, err := exec.CommandContext(ctx, serverTool, "--preview").Output()
			if err != nil {
				return nil, fmt.Errorf("preview server settings: %w", err)
			}
			server, err = setup.MacServerPrompt(strings.TrimSpace(string(preview)))
			if errors.Is(err, huh.ErrUserAborted) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
		}
		if err := setup.ConfigureMac(path, appName, existingApp, validate); err != nil {
			return nil, err
		}
		cfg, err = config.Load(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err == nil {
			key, err = config.ReadKey(macos.Key())
		}
	default:
		activePath, err := macos.ActiveConfigPath(ctx)
		if err != nil {
			return nil, err
		}
		activeKey := macos.Key()
		if activePath == "" || automatic {
			activePath = path
		} else {
			activeKey = filepath.Join(filepath.Dir(activePath), "github-app.pem")
		}
		cfg, err = config.Load(activePath)
		if err == nil {
			key, err = config.ReadKey(activeKey)
		}
		if err == nil {
			err = validate(cfg, key)
		}
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if len(choice) == 0 {
		value := "personal\n"
		if server {
			value = "server\n"
		}
		if err := os.WriteFile(profile, []byte(value), 0600); err != nil {
			return nil, err
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if _, err := backend.PrepareImage(ctx, cfg, host.StateDir(), host.ImageCache(), logger, os.Stdout); err != nil {
		return nil, fmt.Errorf("prepare runner image: %w", err)
	}
	if err := applyScaleSetRouting(ctx, cfg, key); err != nil {
		return nil, err
	}
	if server && !automatic {
		if err := runMacCommand(ctx, serverTool, "--apply"); err != nil {
			return nil, err
		}
	}
	intake := "observing"
	if cfg.Runner.Execute {
		intake = "executing"
	}
	activePath, err := macos.ActiveConfigPath(ctx)
	if err != nil {
		return nil, err
	}
	return &macos.Candidate{Config: data, Key: key, ReplaceEditable: !resume && automatic,
		Scope: cfg.GitHub.ScopeURL, ScaleSet: cfg.GitHub.ScaleSetName, Intake: intake,
		BeforeSwitch: func() error { return checkConfigRecovery(cfg, activePath) },
	}, nil
}

func probeMacSetup(ctx context.Context, cfg config.Config, key []byte) error {
	if cfg.VM.Backend != "tart" {
		return errors.New("macOS installation requires the Tart backend")
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := orders.ProbeScaleSet(probe, orders.Config{
		ScopeURL: cfg.GitHub.ScopeURL, ScaleSetName: cfg.GitHub.ScaleSetName, RunnerGroup: cfg.RunnerGroup(),
		AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID, PrivateKey: key,
	})
	if err != nil {
		return fmt.Errorf("verify GitHub App access: %w", err)
	}
	return nil
}

func runMacCommand(ctx context.Context, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
