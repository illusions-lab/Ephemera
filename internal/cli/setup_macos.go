package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"github.com/illusions-lab/Ephemera/internal/cli/setup"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

func runMacSetup(automatic, resume bool, source, key, appName string, existingApp bool) error {
	if os.Geteuid() == 0 {
		return errors.New("run macOS setup as your logged-in user")
	}
	if err := os.MkdirAll(macos.Root(), 0700); err != nil {
		return err
	}
	info, err := os.Lstat(macos.Root())
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("Ephemera needs a private data directory (0700)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 55*time.Minute)
	defer cancel()
	path := filepath.Join(macos.Root(), "config.yaml")
	profile := filepath.Join(macos.Root(), "server-profile")
	current := filepath.Join(macos.Root(), "current")
	serverTool := filepath.Join(current, "server-config.sh")
	if _, err := os.Stat(serverTool); err != nil {
		return fmt.Errorf("open installed macOS setup tools (run install-macos.sh first): %w", err)
	}
	choice, err := os.ReadFile(profile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && string(choice) != "personal\n" && string(choice) != "server\n" {
		return fmt.Errorf("invalid setup profile in %s", profile)
	}
	server := string(choice) == "server\n"
	_, statErr := os.Lstat(path)
	initial := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !initial {
		return statErr
	}
	validate := func(cfg config.Config, pem []byte) error { return probeMacSetup(ctx, cfg, pem) }
	if automatic {
		if err := setupFromValidatedFiles(macos.Root(), source, key, resume || !initial, validate); err != nil {
			return err
		}
	} else if initial {
		if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
			return errors.New("setup requires a terminal or --non-interactive with input files")
		}
		if len(choice) == 0 {
			preview, err := exec.CommandContext(ctx, serverTool, "--preview").Output()
			if err != nil {
				return fmt.Errorf("preview server settings: %w", err)
			}
			server, err = setup.MacServerPrompt(strings.TrimSpace(string(preview)))
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		if err := setup.ConfigureMac(path, appName, existingApp, validate); err != nil {
			return err
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if !initial {
		pem, err := config.ReadKey(macos.Key())
		if err != nil {
			return err
		}
		if err := validate(cfg, pem); err != nil {
			return err
		}
	}

	if len(choice) == 0 {
		value := "personal\n"
		if server {
			value = "server\n"
		}
		if err := os.WriteFile(profile, []byte(value), 0600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(macos.StateDir(), 0700); err != nil {
		return err
	}
	tart, err := vm.NewTart(filepath.Join(macos.StateDir(), "tart"), cfg.VM.VCPUs, cfg.VM.MemoryMiB)
	if err != nil {
		return err
	}
	if err := tart.Check(ctx); err != nil {
		return err
	}
	if err := tart.PullImage(ctx, cfg.VM.ImageRef, os.Stdout); err != nil {
		return fmt.Errorf("prepare Tart image: %w", err)
	}
	if server && !automatic {
		if err := runMacCommand(ctx, serverTool, "--apply"); err != nil {
			return err
		}
	}
	if err := runMacCommand(ctx, filepath.Join(current, "activate-macos.sh")); err != nil {
		return err
	}

	wait, cancelWait := context.WithTimeout(ctx, 45*time.Second)
	defer cancelWait()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	wantIntake := "observing"
	if cfg.Runner.Execute {
		wantIntake = "executing"
	}
	for {
		snapshot, err := status.Read(wait, macos.Socket())
		if err == nil && snapshot.Phase == "Healthy" && snapshot.GitHub == "connected" &&
			snapshot.Scope == cfg.GitHub.ScopeURL && snapshot.ScaleSet == cfg.GitHub.ScaleSetName && snapshot.Intake == wantIntake {
			fmt.Printf("Ephemera is running (%s).\n", snapshot.Intake)
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("LaunchAgent did not connect to GitHub; see %s: %w", filepath.Join(macos.Root(), "logs", "daemon.log"), wait.Err())
		case <-tick.C:
		}
	}
}

func probeMacSetup(ctx context.Context, cfg config.Config, key []byte) error {
	if cfg.VM.Backend != "tart" {
		return errors.New("macOS installation requires the Tart backend")
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := orders.ProbeScaleSet(probe, orders.Config{
		ScopeURL: cfg.GitHub.ScopeURL, ScaleSetName: cfg.GitHub.ScaleSetName,
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
