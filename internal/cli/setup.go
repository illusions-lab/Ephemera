package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/illusions-lab/Ephemera/internal/cli/setup"
	"github.com/illusions-lab/Ephemera/internal/config"
	"go.yaml.in/yaml/v3"
)

func runSetup(args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	nonInteractive := flags.Bool("non-interactive", false, "read configuration and key from files")
	recoverKey := flags.Bool("recover-residual-key", false, "replace a verified residual key after an incomplete first install")
	resume := flags.Bool("resume", false, "verify existing configuration and key for an installation retry")
	source := flags.String("config", "", "source YAML configuration")
	keyPath := flags.String("github-app-key", "", "source GitHub App PEM file")
	appName := flags.String("app-name", "", "prefill the name of the GitHub App to create")
	existingApp := flags.Bool("existing-app", false, "enter credentials for an existing GitHub App in the terminal")
	localDir := flags.String("local-dir", "", "run setup locally and save configuration and credentials in this directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to setup")
	}
	if *recoverKey && (!*nonInteractive || *resume || runtime.GOOS != "linux") {
		return errors.New("--recover-residual-key requires a non-interactive Linux first install")
	}
	if *resume && !*nonInteractive {
		return errors.New("--resume requires --non-interactive")
	}
	if *appName != "" && (*existingApp || *nonInteractive) {
		return errors.New("--app-name requires interactive GitHub App creation")
	}
	if *existingApp && *nonInteractive {
		return errors.New("--existing-app requires interactive setup")
	}
	if *localDir != "" {
		if *nonInteractive || *source != "" || *keyPath != "" {
			return errors.New("--local-dir requires interactive setup")
		}
		if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
			return errors.New("setup requires a terminal")
		}
		dir, err := filepath.Abs(*localDir)
		if err != nil {
			return err
		}
		fmt.Println("Local setup directory:", dir)
		return setup.RunLocal(filepath.Join(dir, "config.yaml"), *appName, *existingApp)
	}
	if *nonInteractive && (*source == "" || *keyPath == "") {
		return errors.New("--non-interactive requires --config and --github-app-key")
	}
	if !*nonInteractive && (*source != "" || *keyPath != "") {
		return errors.New("--config and --github-app-key require --non-interactive")
	}
	if runtime.GOOS == "darwin" {
		return runMacSetup(*nonInteractive, *resume, *source, *keyPath, *appName, *existingApp)
	}
	if os.Geteuid() != 0 {
		return errors.New("run setup with sudo to write the system configuration")
	}
	destination := filepath.Join(config.Dir, "config.yaml")

	if !*nonInteractive {
		if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
			return errors.New("setup requires a terminal or --non-interactive with input files")
		}
		return setup.Run(destination, *appName, *existingApp)
	}
	var err error
	if *recoverKey {
		err = recoverSetupKey(config.Dir, *source, *keyPath)
	} else {
		err = setupFromFiles(config.Dir, *source, *keyPath, *resume)
	}
	if err != nil {
		return err
	}
	if *resume {
		fmt.Println("Configuration and key verified for installation retry")
	} else {
		fmt.Println("Configuration saved to", destination)
	}
	return nil
}

func recoverSetupKey(dir, source, keyPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if host.QueryService(ctx).State != "not-installed" {
		return errors.New("residual key recovery requires an uninstalled service")
	}
	for _, path := range []string{"/etc/systemd/system/ephemera.service", "/opt/ephemera/current"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("residual key recovery requires %s to be absent", path)
		}
	}
	cfg, err := config.Load(source)
	if err != nil {
		return err
	}
	key, err := config.ReadKey(keyPath)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return config.RecoverSetupKey(dir, data, key, func() error {
		_, err := orders.ProbeScaleSet(ctx, orders.Config{
			ScopeURL: cfg.GitHub.ScopeURL, ScaleSetName: cfg.GitHub.ScaleSetName,
			AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID, PrivateKey: key,
		})
		if err != nil {
			return fmt.Errorf("validate replacement GitHub App key: %w", err)
		}
		return nil
	})
}

func setupFromFiles(dir, source, keyPath string, resume bool) error {
	return setupFromValidatedFiles(dir, source, keyPath, resume, nil)
}

func setupFromValidatedFiles(dir, source, keyPath string, resume bool, validate func(config.Config, []byte) error) error {
	destination := filepath.Join(dir, "config.yaml")
	if !resume {
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("configuration already exists at %s", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	cfg, err := config.Load(source)
	if err != nil {
		return err
	}
	key, err := config.ReadKey(keyPath)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if !resume {
		if validate != nil {
			if err := validate(cfg, key); err != nil {
				return err
			}
		}
		return config.WriteSetup(dir, data, key)
	}
	saved, err := config.Load(destination)
	if err != nil {
		return err
	}
	savedData, err := yaml.Marshal(saved)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, savedData) {
		return errors.New("configuration does not match the saved configuration")
	}
	savedKeyPath := filepath.Join(dir, "credentials", "github-app.pem")
	info, err := os.Lstat(savedKeyPath)
	if err != nil {
		return err
	}
	if info.Mode() != 0600 {
		return errors.New("saved GitHub App key must be a regular file with mode 0600")
	}
	savedKey, err := os.ReadFile(savedKeyPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(key, savedKey) {
		return errors.New("GitHub App key does not match the saved key")
	}
	return nil
}
