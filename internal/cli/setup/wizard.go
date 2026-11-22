package setup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/illusions-lab/Ephemera/internal/config"
	"go.yaml.in/yaml/v3"
)

func Run(path, appName string, existingApp bool, apply func() error) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := configureFirstRun(path, appName, existingApp, true, config.Defaults(), nil); err != nil {
			return finishSetup(err)
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return finishSetup(apply())
	} else if err != nil {
		return err
	}
	var notice string
	for {
		var action string
		err := huh.NewSelect[string]().Title("Ephemera setup").Description(notice).Options(
			huh.NewOption("Edit configuration", "edit"),
			huh.NewOption("Prepare a golden image", "image"),
			huh.NewOption("Apply configuration and start or restart service", "apply"),
			huh.NewOption("Exit", "exit"),
		).Value(&action).Run()
		if errors.Is(err, huh.ErrUserAborted) || action == "exit" {
			return nil
		}
		if err != nil {
			return err
		}
		switch action {
		case "edit":
			err = Edit(path)
		case "image":
			err = prepareFromCatalog(path)
		case "apply":
			err = apply()
		}
		notice = ""
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			notice = err.Error()
		}
	}
}

// RunLocal configures GitHub credentials and runner settings without controlling a service.
func RunLocal(path, appName string, existingApp bool) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return finishSetup(configureFirstRun(path, appName, existingApp, false, config.Defaults(), nil))
	} else if err != nil {
		return err
	}
	return Edit(path)
}

func configureFirstRun(path, appName string, existingApp, systemSetup bool, cfg config.Config, validate func(config.Config, []byte) error) error {
	if err := editRunner(&cfg); err != nil {
		return err
	}
	// First-run setup enables image updates by default and lets the user opt out.
	enabled := true
	cfg.Runner.ImageAutoUpdate = &enabled
	if err := editVM(&cfg); err != nil {
		return err
	}
	if cfg.Runner.ImageAutoUpdate == nil {
		cfg.Runner.ImageAutoUpdate = &enabled
	}
	if existingApp {
		return configureExistingApp(path, cfg, validate)
	}
	if err := editScaleSetRouting(&cfg); err != nil {
		return err
	}
	return createGitHubApp(cfg.GitHub.ScaleSetName, appName, systemSetup, systemSetup || cfg.VM.Backend == "tart",
		func(connection githubConnection) error { return saveGitHubSetup(path, cfg, connection, validate) })
}

func saveGitHubSetup(path string, cfg config.Config, connection githubConnection, validate func(config.Config, []byte) error) error {
	cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName = connection.Scope, connection.ScaleSet
	cfg.GitHub.AppID, cfg.GitHub.InstallationID = connection.AppID, connection.InstallationID
	saved, err := writeFirstSetup(path, cfg, connection.Key, validate)
	if err != nil || !saved {
		return err
	}
	permission := "Organization self-hosted runners"
	if connection.Permission == "administration" {
		permission = "Repository administration"
	}
	fmt.Println("\nGitHub App installation verified")
	fmt.Printf("  App:              https://github.com/apps/%s (ID %d)\n", connection.AppSlug, connection.AppID)
	fmt.Printf("  Installation ID:  %d\n", connection.InstallationID)
	fmt.Printf("  GitHub target:    %s\n", connection.Scope)
	fmt.Printf("  Permission:       %s (write)\n", permission)
	fmt.Printf("  Runner scale set: %q\n", connection.ScaleSet)
	fmt.Printf("  Configuration:    %s\n\n", path)
	return nil
}

func finishSetup(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	return err
}

func configureExistingApp(path string, cfg config.Config, validate func(config.Config, []byte) error) error {
	if err := editGitHub(&cfg); err != nil {
		return err
	}
	var keyPath string
	if err := huh.NewInput().Title("Path to GitHub App private key").Value(&keyPath).Validate(func(path string) error {
		_, err := config.ReadKey(strings.TrimSpace(path))
		return err
	}).Run(); err != nil {
		return err
	}
	key, err := config.ReadKey(strings.TrimSpace(keyPath))
	if err != nil {
		return err
	}
	_, err = writeFirstSetup(path, cfg, key, validate)
	return err
}

func writeFirstSetup(path string, cfg config.Config, key []byte, validate func(config.Config, []byte) error) (bool, error) {
	if err := cfg.Validate(); err != nil {
		return false, err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return false, err
	}
	mode := "Observe only"
	if cfg.Runner.Execute {
		mode = "Execute jobs when service starts"
	}
	machine := cfg.VM.Image + "/" + cfg.VM.Arch
	if cfg.VM.Backend == "tart" {
		machine = cfg.VM.ImageRef
	}
	resources := fmt.Sprintf("%d CPUs · %d MiB per VM", cfg.VM.VCPUs, cfg.VM.MemoryMiB)
	if cfg.VM.Backend == "qemu" {
		resources += fmt.Sprintf(" · %d GiB guest disk", max(12, cfg.VM.DiskGiB))
	}
	summary := fmt.Sprintf("%s · %s\nRunner group %s\nLabels: %s\n%s · up to %d runners\n%s\n%s",
		cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName, cfg.RunnerGroup(),
		strings.Join(append([]string{cfg.GitHub.ScaleSetName}, cfg.ScaleSetLabels()...), ", "), mode, cfg.Runner.MaxConcurrent, machine, resources)
	ok, err := confirmSetup("Save this configuration?", summary)
	if err != nil || !ok {
		return false, err
	}
	if validate != nil {
		if err := validate(cfg, key); err != nil {
			return false, err
		}
	}
	err = config.WriteSetup(filepath.Dir(path), data, key)
	return err == nil, err
}

func saveEditedConfig(path string, before, data []byte) error {
	current, err := config.Load(path)
	if err != nil {
		return err
	}
	currentData, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, currentData) {
		return fmt.Errorf("%s changed while setup was open; reopen the editor to review and save", path)
	}
	return config.Replace(path, data)
}

func positiveNumber(value string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n <= 0 {
		return errors.New("enter a positive integer")
	}
	return nil
}

func positiveDuration(value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || d <= 0 {
		return errors.New("enter a positive duration, such as 3m")
	}
	return nil
}

func confirmSetup(title, description string) (bool, error) {
	var confirmed bool
	err := huh.NewConfirm().Title(title).Description(description).Value(&confirmed).Run()
	return confirmed, err
}

func ConfigureMac(path, appName string, existingApp bool, validate func(config.Config, []byte) error) error {
	return finishSetup(configureFirstRun(path, appName, existingApp, false, config.MacDefaults(), validate))
}

func ImageUpdatePrompt(cfg *config.Config) error {
	enabled := cfg.Runner.ImageAutoUpdate == nil || *cfg.Runner.ImageAutoUpdate
	if err := huh.NewConfirm().Title("Enable automatic runner image updates?").Value(&enabled).Run(); err != nil {
		return err
	}
	cfg.Runner.ImageAutoUpdate = &enabled
	return nil
}

func MacServerPrompt(preview string) (bool, error) {
	return confirmSetup("Is this Mac dedicated to running Ephemera runners?",
		"Yes applies these AC power settings. The default is No.\n"+preview)
}
