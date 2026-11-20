package setup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/illusions-lab/Ephemera/internal/config"
	"go.yaml.in/yaml/v3"
)

func Run(path, appName string, existingApp bool) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := configureFirstRun(path, appName, existingApp, true); err != nil {
			return finishSetup(err)
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return finishSetup(applyService(path))
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
			err = editConfig(path)
		case "image":
			err = prepareFromCatalog(path)
		case "apply":
			err = applyService(path)
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
		return finishSetup(configureFirstRun(path, appName, existingApp, false))
	} else if err != nil {
		return err
	}
	return finishSetup(editConfig(path))
}

func configureFirstRun(path, appName string, existingApp, systemSetup bool) error {
	if existingApp {
		return editConfig(path)
	}
	cfg := config.Defaults()
	connection, err := createGitHubApp(cfg.GitHub.ScaleSetName, appName, systemSetup)
	if err != nil {
		return err
	}
	cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName = connection.Scope, connection.ScaleSet
	cfg.GitHub.AppID, cfg.GitHub.InstallationID = connection.AppID, connection.InstallationID
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	ok, err := confirmSetup("Save this configuration?", cfg.GitHub.ScopeURL+" · "+cfg.GitHub.ScaleSetName)
	if err != nil || !ok {
		return err
	}
	return config.WriteSetup(filepath.Dir(path), data, connection.Key)
}

func finishSetup(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	return err
}

func editConfig(path string) error {
	cfg, err := config.Load(path)
	initial := errors.Is(err, os.ErrNotExist)
	if initial {
		cfg = config.Defaults()
	} else if err != nil {
		return err
	}

	before, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	github := []huh.Field{
		huh.NewInput().Title("GitHub repository or organization URL").Value(&cfg.GitHub.ScopeURL).Validate(config.ValidateScope),
		huh.NewInput().Title("Runner Scale Set name").Value(&cfg.GitHub.ScaleSetName).Validate(func(name string) error {
			if strings.TrimSpace(name) == "" {
				return errors.New("enter a Runner Scale Set name")
			}
			return nil
		}),
	}
	if err := huh.NewForm(huh.NewGroup(github...)).Run(); err != nil {
		return err
	}
	if !initial {
		limit := strconv.Itoa(cfg.Runner.MaxConcurrent)
		cpus, memory := strconv.Itoa(cfg.VM.VCPUs), strconv.Itoa(cfg.VM.MemoryMiB)
		startup, idle := cfg.Runner.StartupTimeout.String(), cfg.Runner.IdleTimeout.String()
		beforeImage, beforeArch := cfg.VM.Image, cfg.VM.Arch
		if err := huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Maximum concurrent runners").Value(&limit).Validate(positiveNumber),
			huh.NewInput().Title("Runner startup timeout").Value(&startup).Validate(positiveDuration),
			huh.NewInput().Title("Runner idle timeout").Value(&idle).Validate(positiveDuration),
			huh.NewInput().Title("Image tag (ubuntu or windows)").Value(&cfg.VM.Image),
			huh.NewInput().Title("Image architecture (amd64 or arm64)").Value(&cfg.VM.Arch),
			huh.NewInput().Title("VM CPU count").Value(&cpus).Validate(positiveNumber),
			huh.NewInput().Title("VM memory (MiB)").Value(&memory).Validate(positiveNumber),
		)).Run(); err != nil {
			return err
		}
		cfg.Runner.MaxConcurrent, err = strconv.Atoi(strings.TrimSpace(limit))
		if err != nil {
			return err
		}
		cfg.VM.VCPUs, _ = strconv.Atoi(strings.TrimSpace(cpus))
		cfg.VM.MemoryMiB, _ = strconv.Atoi(strings.TrimSpace(memory))
		cfg.Runner.StartupTimeout, _ = time.ParseDuration(strings.TrimSpace(startup))
		cfg.Runner.IdleTimeout, _ = time.ParseDuration(strings.TrimSpace(idle))
		if cfg.VM.Image != beforeImage || cfg.VM.Arch != beforeArch {
			cfg.VM.Profile = ""
		}
	}
	var key []byte
	app, installation := "", ""
	if !initial {
		app = strconv.FormatInt(cfg.GitHub.AppID, 10)
		installation = strconv.FormatInt(cfg.GitHub.InstallationID, 10)
	}
	fields := []huh.Field{
		huh.NewInput().Title("GitHub App ID").Value(&app).Validate(positiveNumber),
		huh.NewInput().Title("GitHub App installation ID").Value(&installation).Validate(positiveNumber),
	}
	var keyPath string
	if initial {
		fields = append(fields, huh.NewInput().Title("Path to GitHub App private key").Value(&keyPath).Validate(func(path string) error {
			_, err := config.ReadKey(strings.TrimSpace(path))
			return err
		}))
	}
	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return err
	}
	if initial {
		key, err = config.ReadKey(strings.TrimSpace(keyPath))
		if err != nil {
			return err
		}
	}
	cfg.GitHub.AppID, _ = strconv.ParseInt(strings.TrimSpace(app), 10, 64)
	cfg.GitHub.InstallationID, _ = strconv.ParseInt(strings.TrimSpace(installation), 10, 64)
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if !initial && bytes.Equal(before, data) {
		fmt.Println("Configuration is already up to date.")
		return nil
	}
	summary := fmt.Sprintf("%s · %s\nGitHub App %d · installation %d\n%s/%s · %d CPUs · %d MiB\n%d runners · startup %s · idle %s",
		cfg.GitHub.ScopeURL, cfg.GitHub.ScaleSetName, cfg.GitHub.AppID, cfg.GitHub.InstallationID,
		cfg.VM.Image, cfg.VM.Arch, cfg.VM.VCPUs, cfg.VM.MemoryMiB, cfg.Runner.MaxConcurrent,
		cfg.Runner.StartupTimeout, cfg.Runner.IdleTimeout)
	ok, err := confirmSetup("Save this configuration?", summary)
	if err != nil || !ok {
		return err
	}
	if initial {
		return config.WriteSetup(filepath.Dir(path), data, key)
	}
	return saveEditedConfig(path, before, data)
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

func applyService(path string) error {
	if _, err := config.Load(path); err != nil {
		return err
	}
	ok, err := confirmSetup("Apply configuration now?", "This starts Ephemera or restarts the running service.")
	if err != nil || !ok {
		return err
	}
	for _, operation := range []string{"enable", "restart"} {
		cmd := exec.Command("systemctl", operation, "ephemera.service")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl %s ephemera.service: %w", operation, err)
		}
	}
	return nil
}
