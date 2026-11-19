package main

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"go.yaml.in/yaml/v3"
)

const configDir = "/etc/ephemera"

type config struct {
	Version int
	GitHub  struct {
		ScopeURL       string `yaml:"scope_url"`
		ScaleSetName   string `yaml:"scale_set_name"`
		AppID          int64  `yaml:"app_id"`
		InstallationID int64  `yaml:"installation_id"`
	}
	Runner struct {
		MaxConcurrent  int           `yaml:"max_concurrent"`
		StartupTimeout time.Duration `yaml:"startup_timeout"`
		IdleTimeout    time.Duration `yaml:"idle_timeout"`
	}
	VM struct {
		Backend   string
		Image     string
		VCPUs     int
		MemoryMiB int `yaml:"memory_mib"`
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ephemera:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) >= 1 && args[0] == "setup":
		return setup(args[1:])
	case len(args) >= 2 && args[0] == "config" && args[1] == "validate":
		flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
		path := flags.String("config", filepath.Join(configDir, "config.yaml"), "configuration file")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected arguments to config validate")
		}
		_, err := loadConfig(*path)
		return err
	default:
		return errors.New("usage: ephemera setup | ephemera config validate [--config PATH]")
	}
}

func loadConfig(path string) (config, error) {
	var cfg config
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return cfg, fmt.Errorf("%s: expected exactly one YAML document", path)
	}
	return cfg, cfg.validate()
}

func validateScope(value string) error {
	scope, err := url.Parse(value)
	if err != nil || scope.Scheme != "https" || scope.Hostname() == "" ||
		scope.User != nil || scope.ForceQuery || scope.RawQuery != "" || scope.Fragment != "" ||
		strings.Trim(scope.Path, "/") == "" {
		return errors.New("use an HTTPS repository or organization URL")
	}
	return nil
}

func (cfg config) validate() error {
	switch {
	case cfg.Version != 1:
		return errors.New("version must be 1")
	case strings.TrimSpace(cfg.GitHub.ScaleSetName) == "":
		return errors.New("github.scale_set_name is required")
	case cfg.GitHub.AppID <= 0 || cfg.GitHub.InstallationID <= 0:
		return errors.New("github.app_id and github.installation_id must be positive")
	case cfg.Runner.MaxConcurrent <= 0:
		return errors.New("runner.max_concurrent must be positive")
	case cfg.Runner.StartupTimeout <= 0 || cfg.Runner.IdleTimeout <= 0:
		return errors.New("runner.startup_timeout and runner.idle_timeout must be positive")
	case cfg.VM.Backend != "qemu":
		return errors.New("vm.backend must be qemu")
	case strings.TrimSpace(cfg.VM.Image) == "" || cfg.VM.VCPUs <= 0 || cfg.VM.MemoryMiB <= 0:
		return errors.New("vm.image is required and vm.vcpus and vm.memory_mib must be positive")
	}
	if err := validateScope(cfg.GitHub.ScopeURL); err != nil {
		return fmt.Errorf("github.scope_url: %w", err)
	}
	return nil
}

func setup(args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	nonInteractive := flags.Bool("non-interactive", false, "read configuration and key from files")
	source := flags.String("config", "", "source YAML configuration")
	keyPath := flags.String("github-app-key", "", "source GitHub App PEM file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to setup")
	}
	if *nonInteractive && (*source == "" || *keyPath == "") {
		return errors.New("--non-interactive requires --config and --github-app-key")
	}
	if !*nonInteractive && (*source != "" || *keyPath != "") {
		return errors.New("--config and --github-app-key require --non-interactive")
	}
	if os.Geteuid() != 0 {
		return errors.New("run setup with sudo to write the system configuration")
	}
	destination := filepath.Join(configDir, "config.yaml")
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("configuration already exists at %s; edit it directly", destination)
	}

	var cfg config
	var err error
	if *nonInteractive {
		cfg, err = loadConfig(*source)
	} else {
		if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
			return errors.New("setup requires a terminal or --non-interactive with input files")
		}
		cfg, *keyPath, err = promptConfig()
	}
	if err != nil {
		return err
	}
	key, err := readKey(*keyPath)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := writeSetup(configDir, data, key); err != nil {
		return err
	}
	fmt.Println("Configuration saved to", destination)
	return nil
}

func promptConfig() (config, string, error) {
	var cfg config
	cfg.Version = 1
	cfg.Runner.StartupTimeout = 3 * time.Minute
	cfg.Runner.IdleTimeout = 5 * time.Minute
	cfg.VM.Backend = "qemu"
	cfg.VM.VCPUs = 2
	cfg.VM.MemoryMiB = 4096
	name, concurrent := "ephemera-linux", "2"
	var scope, app, installation, key, image string
	required := func(value string) error {
		if strings.TrimSpace(value) == "" {
			return errors.New("enter a value")
		}
		return nil
	}
	positive := func(value string) error {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n <= 0 {
			return errors.New("enter a positive integer")
		}
		return nil
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("GitHub repository or organization URL").Value(&scope).Validate(validateScope),
			huh.NewInput().Title("Runner Scale Set name").Value(&name).Validate(required),
			huh.NewInput().Title("GitHub App ID").Value(&app).Validate(positive),
			huh.NewInput().Title("GitHub App Installation ID").Value(&installation).Validate(positive),
			huh.NewInput().Title("Path to the GitHub App private key (.pem)").Value(&key).Validate(func(path string) error {
				_, err := readKey(strings.TrimSpace(path))
				return err
			}),
		),
		huh.NewGroup(
			huh.NewInput().Title("Runner VM image path or ID").Value(&image).Validate(required),
			huh.NewInput().Title("Maximum concurrent runners").Value(&concurrent).Validate(positive),
		),
	)
	if err := form.Run(); err != nil {
		return cfg, "", err
	}
	cfg.GitHub.ScopeURL = scope
	cfg.GitHub.ScaleSetName = strings.TrimSpace(name)
	cfg.GitHub.AppID, _ = strconv.ParseInt(strings.TrimSpace(app), 10, 64)
	cfg.GitHub.InstallationID, _ = strconv.ParseInt(strings.TrimSpace(installation), 10, 64)
	cfg.Runner.MaxConcurrent, _ = strconv.Atoi(strings.TrimSpace(concurrent))
	cfg.VM.Image = strings.TrimSpace(image)
	return cfg, strings.TrimSpace(key), cfg.validate()
}

func readKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block != nil {
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return data, nil
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if _, ok := key.(*rsa.PrivateKey); err == nil && ok {
			return data, nil
		}
	}
	return nil, errors.New("GitHub App key must be an unencrypted RSA private key in PEM format")
}

func writeSetup(dir string, configData, keyData []byte) error {
	credentials := filepath.Join(dir, "credentials")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(credentials, 0700); err != nil {
		return err
	}
	keyPath := filepath.Join(credentials, "github-app.pem")
	if err := writeNewFile(keyPath, keyData, 0600); err != nil {
		// A retry can reuse the same key after an interrupted setup.
		info, statErr := os.Lstat(keyPath)
		if !errors.Is(err, os.ErrExist) || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return err
		}
		existing, readErr := os.ReadFile(keyPath)
		if readErr != nil || !bytes.Equal(existing, keyData) {
			return fmt.Errorf("a different GitHub App key already exists at %s", keyPath)
		}
	}
	return writeNewFile(filepath.Join(dir, "config.yaml"), configData, 0644)
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".ephemera-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(data)
	modeErr := temp.Chmod(mode)
	closeErr := temp.Close()
	if err := errors.Join(writeErr, modeErr, closeErr); err != nil {
		return err
	}
	return os.Link(temp.Name(), path)
}
