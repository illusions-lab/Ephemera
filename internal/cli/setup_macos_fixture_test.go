//go:build darwin && arm64

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/version"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

// The installer test runs the real CLI and setup path. Only the GitHub probe
// and daemon readiness are simulated, so credentials and trust stores stay local.
func TestMain(m *testing.M) {
	if os.Getenv("EPHE_INSTALL_FIXTURE") != "1" {
		os.Exit(m.Run())
	}
	if err := runInstallFixture(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runInstallFixture(args []string) error {
	if len(args) == 1 && args[0] == "serve-status-fixture" {
		readiness := filepath.Join(macos.Root(), "readiness.json")
		closeStatus, err := status.Listen(macos.Socket(), func() status.Snapshot {
			var snapshot status.Snapshot
			data, _ := os.ReadFile(readiness)
			_ = json.Unmarshal(data, &snapshot)
			return snapshot
		})
		if err != nil {
			return err
		}
		defer closeStatus()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return nil
	}
	if filepath.Base(os.Args[0]) == "ephemerad" {
		return runDaemonFixture(args)
	}
	macSetupProbe = func(_ context.Context, cfg config.Config, _ []byte) error {
		if cfg.GitHub.ScopeURL != "https://github.com/fixture/ephemera" || cfg.VM.Backend != "tart" {
			return fmt.Errorf("unexpected setup probe target: %s (%s)", cfg.GitHub.ScopeURL, cfg.VM.Backend)
		}
		return nil
	}
	return Run(args)
}

func runDaemonFixture(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Println(version.String())
		return nil
	case len(args) == 1 && args[0] == "--tart-manifest":
		fmt.Print(macos.TartManifest())
		return nil
	}
	flags := flag.NewFlagSet("ephemerad fixture", flag.ContinueOnError)
	path := flags.String("config", "", "")
	key := flags.String("github-app-key", "", "")
	id := flags.String("activation-id", "", "")
	create := flags.Bool("create", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*create || *id == "" || filepath.Dir(*path) != filepath.Dir(*key) || *path == filepath.Join(macos.Root(), "config.yaml") || *key == macos.Key() {
		return fmt.Errorf("LaunchAgent did not receive a unique activation and a matching config/key snapshot")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if _, err := config.ReadKey(*key); err != nil {
		return err
	}
	tart, err := vm.NewTart(filepath.Join(macos.StateDir(), "tart"), cfg.VM.VCPUs, cfg.VM.MemoryMiB)
	if err != nil {
		return err
	}
	if err := tart.CheckInstalled(context.Background()); err != nil {
		return err
	}
	snapshot := status.Snapshot{
		ActivationID: *id, Scope: cfg.GitHub.ScopeURL, ScaleSet: cfg.GitHub.ScaleSetName,
		Phase: "Healthy", GitHub: "connected", Intake: "observing",
	}
	if cfg.Runner.Execute {
		snapshot.Intake = "executing"
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(macos.Root(), "readiness.json"), body, 0600)
}
