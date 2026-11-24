package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"github.com/illusions-lab/Ephemera/internal/backend"
	"github.com/illusions-lab/Ephemera/internal/cli/setup"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
	"go.yaml.in/yaml/v3"
)

func runConfigApply(args []string) error {
	flags := flag.NewFlagSet("config apply", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "apply without prompting")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: ephe config apply [--yes]")
	}
	if !*yes && (!term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd())) {
		return errors.New("use ephe config apply --yes when running without a terminal")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if runtime.GOOS == "darwin" {
		return applyMacConfig(ctx, *yes)
	}
	if os.Geteuid() != 0 {
		return errors.New("run sudo ephe config apply to apply the system configuration")
	}
	options := []string{"--apply-config"}
	if *yes {
		options = append(options, "--yes")
	}
	cmd := exec.Command("/opt/ephemera/recover-install", options...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	cancel := context.AfterFunc(ctx, func() { _ = cmd.Process.Signal(syscall.SIGTERM) })
	defer cancel()
	err := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type configCandidate struct {
	cfg       config.Config
	data, key []byte
	digest    string
}

func loadConfigCandidate(ctx context.Context) (configCandidate, error) {
	cfg, err := config.Load(host.ConfigPath())
	if err != nil {
		return configCandidate{}, err
	}
	key, err := config.ReadKey(host.KeyPath())
	if err != nil {
		return configCandidate{}, err
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if runtime.GOOS == "darwin" {
		err = macSetupProbe(probe, cfg, key)
	} else {
		if cfg.VM.Backend != "qemu" || (cfg.Runner.Execute && cfg.VM.Arch != runtime.GOARCH) {
			return configCandidate{}, errors.New("Linux requires QEMU and a guest matching the host architecture when executing jobs")
		}
		_, err = orders.ProbeScaleSet(probe, orders.Config{
			ScopeURL: cfg.GitHub.ScopeURL, ScaleSetName: cfg.GitHub.ScaleSetName, RunnerGroup: cfg.RunnerGroup(),
			AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID, PrivateKey: key,
		})
	}
	if err != nil {
		return configCandidate{}, fmt.Errorf("check GitHub access: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return configCandidate{}, err
	}
	digest, err := config.Digest(cfg)
	return configCandidate{cfg: cfg, data: data, key: key, digest: digest}, err
}

var ensureScaleSetLabels = orders.EnsureScaleSetLabels

func applyScaleSetRouting(ctx context.Context, cfg config.Config, key []byte) error {
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ensureScaleSetLabels(probe, runnerRouting(cfg, key))
}

func runnerRouting(cfg config.Config, key []byte) orders.Config {
	return orders.Config{
		ScopeURL: cfg.GitHub.ScopeURL, ScaleSetName: cfg.GitHub.ScaleSetName, RunnerGroup: cfg.RunnerGroup(),
		AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID, PrivateKey: key,
		Labels: append([]string{cfg.GitHub.ScaleSetName}, cfg.ScaleSetLabels()...), ReplaceLabels: cfg.GitHub.Labels != nil,
	}
}

func confirmConfigCandidate(ctx context.Context, candidate configCandidate, oldPath string, yes bool) (bool, error) {
	cfg := candidate.cfg
	if old, err := config.Load(oldPath); err == nil {
		changes, err := setup.ConfigChanges(old, cfg)
		if err != nil {
			return false, err
		}
		if changes != "" {
			fmt.Print(changes)
		}
	}
	mode := "Observe only"
	if cfg.Runner.Execute {
		mode = "Execute jobs"
	}
	fmt.Printf("Target %s · %s · runner group %s\n%s · up to %d runners\nRequested labels %s\n",
		cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName, cfg.RunnerGroup(), mode, cfg.Runner.MaxConcurrent,
		strings.Join(append([]string{cfg.GitHub.ScaleSetName}, cfg.ScaleSetLabels()...), ", "))
	fmt.Println(setup.CapacityPreview(cfg))
	if yes {
		return true, nil
	}
	var confirmed bool
	confirmation := huh.NewConfirm().Title("Apply configuration now?").
		Description("Ephemera will wait for active jobs before restarting.").Value(&confirmed)
	err := huh.NewForm(huh.NewGroup(confirmation)).RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) {
		return false, nil
	}
	return confirmed, err
}

func checkConfigRecovery(candidate config.Config, activePath string) error {
	active, err := config.Load(activePath)
	if err == nil && active.GitHub.ScopeURL == candidate.GitHub.ScopeURL &&
		active.GitHub.ScaleSetName == candidate.GitHub.ScaleSetName && active.RunnerGroup() == candidate.RunnerGroup() &&
		active.VM.Backend == candidate.VM.Backend && active.VMDataPath(host.StateDir()) == candidate.VMDataPath(host.StateDir()) {
		return nil
	}
	records, err := filepath.Glob(filepath.Join(host.StateDir(), "*.json"))
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return errors.New("old runner recovery records remain; let the current service finish cleanup and retry")
	}
	return nil
}

func applyMacConfig(ctx context.Context, yes bool) error {
	if os.Geteuid() == 0 {
		return errors.New("run ephe config apply as the service account on macOS")
	}
	release, err := filepath.EvalSymlinks(filepath.Join(macos.Root(), "current"))
	if err != nil {
		return fmt.Errorf("find installed Ephemera release: %w", err)
	}
	confirmed := false
	err = macos.Activate(ctx, release, func() (*macos.Candidate, error) {
		candidate, err := loadConfigCandidate(ctx)
		if err != nil {
			return nil, err
		}
		activePath, err := macos.ActiveConfigPath(ctx)
		if err != nil {
			return nil, err
		}
		ok, err := confirmConfigCandidate(ctx, candidate, activePath, yes)
		if err != nil || !ok {
			return nil, err
		}
		if candidate.cfg.Runner.Execute {
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			if _, err := backend.PrepareImage(ctx, candidate.cfg, host.StateDir(), host.ImageCache(), logger, os.Stdout); err != nil {
				return nil, fmt.Errorf("prepare runner image: %w", err)
			}
		}
		if err := applyScaleSetRouting(ctx, candidate.cfg, candidate.key); err != nil {
			return nil, err
		}
		confirmed = true
		intake := "observing"
		if candidate.cfg.Runner.Execute {
			intake = "executing"
		}
		return &macos.Candidate{Config: candidate.data, Key: candidate.key, ConfigDigest: candidate.digest,
			Scope: candidate.cfg.GitHub.ScopeURL, ScaleSet: candidate.cfg.GitHub.ScaleSetName, Intake: intake,
			WaitOnly: true, BeforeSwitch: func() error { return checkConfigRecovery(candidate.cfg, activePath) },
		}, nil
	})
	if err == nil && confirmed {
		fmt.Println("Configuration applied.")
	}
	return err
}

// runLinuxConfigStep is invoked by the installer while it holds the service transaction lock.
func runLinuxConfigStep(args []string) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || len(args) < 2 {
		return errors.New("invalid configuration transaction step")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir := args[1]
	switch args[0] {
	case "prepare":
		candidate, err := loadConfigCandidate(ctx)
		if err != nil {
			return err
		}
		ok, err := confirmConfigCandidate(ctx, candidate, "/etc/ephemera/active/config.yaml", len(args) == 3 && args[2] == "--yes")
		if err != nil || !ok {
			return err
		}
		if err := applyScaleSetRouting(ctx, candidate.cfg, candidate.key); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), candidate.data, 0644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "github-app.pem"), candidate.key, 0600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "digest"), []byte(candidate.digest), 0600)
	case "stop", "check-recovery":
		candidate, err := config.Load(filepath.Join(dir, "config.yaml"))
		if err != nil {
			return err
		}
		check := func() error { return checkConfigRecovery(candidate, "/etc/ephemera/active/config.yaml") }
		if args[0] == "check-recovery" {
			return check()
		}
		return control.DrainWait(ctx, host.StateDir(), os.Stderr, func() error {
			if err := check(); err != nil {
				return err
			}
			cmd := exec.CommandContext(ctx, "systemctl", "stop", "ephemera.service")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		})
	case "verify":
		expected, err := os.ReadFile(filepath.Join(dir, "digest"))
		if err != nil {
			return err
		}
		running, err := status.Read(ctx, host.StatusSocket())
		if err != nil {
			return err
		}
		if running.ConfigDigest != string(expected) || running.Phase != "Healthy" || running.GitHub != "connected" {
			return errors.New("service did not report healthy with the expected configuration")
		}
		return nil
	default:
		return errors.New("invalid configuration transaction step")
	}
}
