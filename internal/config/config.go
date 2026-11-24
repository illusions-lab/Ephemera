package config

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"go.yaml.in/yaml/v3"
)

const Dir = host.LinuxConfigDir

func MacDefaults() Config {
	cfg := Defaults()
	cfg.GitHub.ScaleSetName = "ephemera-macos-arm64"
	cfg.Runner.MaxConcurrent = 1
	cfg.Runner.StartupTimeout = 10 * time.Minute
	cfg.VM.Backend, cfg.VM.Image, cfg.VM.Arch = "tart", "macos", "arm64"
	cfg.VM.ImageRef = macos.ImageRef
	return cfg
}

type Config struct {
	Version int
	GitHub  struct {
		RunnerGroup    string    `yaml:"runner_group,omitempty"`
		Labels         *[]string `yaml:"labels,omitempty"`
		ScopeURL       string    `yaml:"scope_url"`
		ScaleSetName   string    `yaml:"scale_set_name"`
		AppID          int64     `yaml:"app_id"`
		InstallationID int64     `yaml:"installation_id"`
	}
	Runner struct {
		ImageAutoUpdate *bool         `yaml:"image_auto_update,omitempty"`
		Execute         bool          `yaml:"execute,omitempty"`
		MaxConcurrent   int           `yaml:"max_concurrent"`
		StartupTimeout  time.Duration `yaml:"startup_timeout"`
		IdleTimeout     time.Duration `yaml:"idle_timeout"`
	}
	VM struct {
		Backend                string
		Image                  string
		Arch                   string `yaml:"arch,omitempty"`
		ImageRef               string `yaml:"image_ref,omitempty"`
		Profile                string `yaml:"profile,omitempty"`
		VCPUs                  int
		MemoryMiB              int    `yaml:"memory_mib"`
		DiskGiB                int    `yaml:"disk_gib,omitempty"`
		AppleToolchain         string `yaml:"apple_toolchain,omitempty"`
		HostReserveMiB         int    `yaml:"host_reserve_mib,omitempty"`
		HostDiskReserveGiB     int    `yaml:"host_disk_reserve_gib,omitempty"`
		CacheMaxGiB            int    `yaml:"cache_max_gib,omitempty"`
		PruneOnDiskPressure    bool   `yaml:"prune_on_disk_pressure,omitempty"`
		ExtraRetainedImages    int    `yaml:"extra_retained_images,omitempty"`
		DataDir                string `yaml:"data_dir,omitempty"`
		ImageCacheDir          string `yaml:"image_cache_dir,omitempty"`
		MemoryOversubscribeMiB int    `yaml:"memory_oversubscribe_mib,omitempty"`
	}
	ImageSources map[string]map[string]image.Source `yaml:"image,omitempty"`
}

func Load(path string) (Config, error) {
	var cfg Config
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var input struct {
		Config        `yaml:",inline"`
		LegacySources map[string]map[string]image.Source `yaml:"images"`
	}
	if err := decoder.Decode(&input); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return cfg, fmt.Errorf("%s: expected exactly one YAML document", path)
	}
	cfg = input.Config
	if input.LegacySources != nil {
		if cfg.ImageSources != nil {
			return cfg, errors.New("use only one of image or the legacy images key")
		}
		cfg.ImageSources = input.LegacySources
	}
	return cfg, cfg.Validate()
}

func (cfg Config) RunnerGroup() string {
	if name := strings.TrimSpace(cfg.GitHub.RunnerGroup); name != "" {
		return name
	}
	return "Default"
}

func (cfg Config) ScaleSetLabels() []string {
	var labels []string
	switch cfg.VM.Backend {
	case "tart":
		labels = []string{"self-hosted", "macOS", "ARM64"}
	case "qemu":
		switch cfg.VM.Image {
		case "ubuntu":
			labels = []string{"self-hosted", "Linux"}
		case "windows":
			labels = []string{"self-hosted", "Windows"}
		}
		if len(labels) != 0 {
			switch cfg.VM.Arch {
			case "amd64":
				labels = append(labels, "X64")
			case "arm64":
				labels = append(labels, "ARM64")
			}
		}
	}
	if cfg.GitHub.Labels != nil {
		labels = append(labels, (*cfg.GitHub.Labels)...)
	}
	return labels
}

func ValidateScope(value string) error {
	scope, err := url.Parse(value)
	if err != nil || scope.Scheme != "https" || scope.Hostname() == "" ||
		scope.User != nil || scope.ForceQuery || scope.RawQuery != "" || scope.Fragment != "" ||
		strings.Trim(scope.Path, "/") == "" {
		return errors.New("use an HTTPS repository or organization URL")
	}
	return nil
}

var localTartImageReference = regexp.MustCompile(`^local:(github-macos26|apple-macos)-[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var tartImageReference = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?/[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$`)

func (cfg Config) Validate() error {
	switch {
	case cfg.Version != 1:
		return errors.New("version must be 1")
	case strings.TrimSpace(cfg.GitHub.ScaleSetName) == "":
		return errors.New("github.scale_set_name is required")
	case cfg.GitHub.AppID <= 0 || cfg.GitHub.InstallationID <= 0:
		return errors.New("github.app_id and github.installation_id must be positive")
	case cfg.Runner.MaxConcurrent <= 0 || cfg.Runner.MaxConcurrent > math.MaxInt32:
		return errors.New("runner.max_concurrent must be between 1 and 2147483647")
	case cfg.Runner.StartupTimeout <= 0 || cfg.Runner.IdleTimeout <= 0:
		return errors.New("runner.startup_timeout and runner.idle_timeout must be positive")
	case cfg.VM.Backend == "qemu" && cfg.Runner.Execute && (cfg.VM.Image != "ubuntu" || (cfg.VM.Arch != "amd64" && cfg.VM.Arch != "arm64")):
		return errors.New("execution requires an Ubuntu amd64 or arm64 guest")
	case cfg.VM.Backend != "qemu" && cfg.VM.Backend != "tart":
		return errors.New("vm.backend must be qemu or tart")
	case cfg.VM.Backend == "tart" && (cfg.VM.Image != "macos" || cfg.VM.Arch != "arm64" || cfg.VM.Profile != "" || (!tartImageReference.MatchString(cfg.VM.ImageRef) && !localTartImageReference.MatchString(cfg.VM.ImageRef))):
		return errors.New("tart requires vm.image macos, vm.arch arm64 and a verified local build or an OCI vm.image_ref pinned by SHA-256 digest")
	case cfg.VM.Backend == "qemu" && cfg.VM.ImageRef != "":
		return errors.New("vm.image_ref is only supported by tart")
	case strings.HasPrefix(cfg.VM.ImageRef, "local:apple-macos-") && cfg.VM.AppleToolchain == "":
		return errors.New("an Apple host-Xcode image requires vm.apple_toolchain macos or ios")
	case cfg.VM.AppleToolchain != "" && (cfg.VM.Backend != "tart" || (cfg.VM.AppleToolchain != "macos" && cfg.VM.AppleToolchain != "ios")):
		return errors.New("vm.apple_toolchain must be macos or ios and requires Tart")
	case cfg.VM.HostReserveMiB != 0 && (cfg.VM.HostReserveMiB < 256 || cfg.VM.HostReserveMiB > math.MaxInt32):
		return errors.New("vm.host_reserve_mib must be 0 (default) or between 256 and 2147483647")
	case cfg.VM.CacheMaxGiB < 0 || cfg.VM.CacheMaxGiB > math.MaxInt32:
		return errors.New("vm.cache_max_gib must be between 0 (unlimited) and 2147483647")
	case cfg.VM.ExtraRetainedImages < 0 || cfg.VM.ExtraRetainedImages > 100:
		return errors.New("vm.extra_retained_images must be between 0 and 100")
	case cfg.VM.DataDir != "" && (!filepath.IsAbs(cfg.VM.DataDir) || filepath.Clean(cfg.VM.DataDir) != cfg.VM.DataDir || cfg.VM.DataDir == "/"):
		return errors.New("vm.data_dir must be a clean absolute path to a private filesystem directory")
	case cfg.VM.ImageCacheDir != "" && (cfg.VM.Backend != "qemu" || !filepath.IsAbs(cfg.VM.ImageCacheDir) || filepath.Clean(cfg.VM.ImageCacheDir) != cfg.VM.ImageCacheDir || cfg.VM.ImageCacheDir == "/"):
		return errors.New("vm.image_cache_dir requires QEMU and a clean absolute path; Tart keeps its cache under vm.data_dir")
	case cfg.VM.MemoryOversubscribeMiB < 0 || cfg.VM.MemoryOversubscribeMiB > 4096:
		return errors.New("vm.memory_oversubscribe_mib must be between 0 and 4096")
	case cfg.VM.HostDiskReserveGiB < 0 || cfg.VM.HostDiskReserveGiB > math.MaxInt32:
		return errors.New("vm.host_disk_reserve_gib must be between 0 (default) and 2147483647")
	case cfg.VM.DiskGiB != 0 && (cfg.VM.DiskGiB < 12 || cfg.VM.DiskGiB > 1024):
		return errors.New("vm.disk_gib must be 0 (default) or between 12 and 1024")
	case strings.TrimSpace(cfg.VM.Image) == "" || cfg.VM.VCPUs <= 0 || cfg.VM.MemoryMiB <= 0:
		return errors.New("vm.image is required and vm.vcpus and vm.memory_mib must be positive")
	}
	if err := ValidateScope(cfg.GitHub.ScopeURL); err != nil {
		return fmt.Errorf("github.scope_url: %w", err)
	}
	if cfg.VM.Backend == "qemu" && (cfg.VM.Arch != "" || cfg.VM.Image == "ubuntu" || cfg.VM.Image == "windows" || len(cfg.ImageSources) > 0) {
		if err := (image.Platform{Tag: cfg.VM.Image, Arch: cfg.VM.Arch}).Validate(); err != nil {
			return fmt.Errorf("vm.image / vm.arch: %w", err)
		}
	}
	if cfg.VM.Profile != "" && (cfg.VM.Profile != "ubuntu-24.04" || cfg.VM.Image != "ubuntu" || (cfg.VM.Arch != "amd64" && cfg.VM.Arch != "arm64")) {
		return errors.New("vm.profile must name an available recipe for the selected image and architecture")
	}
	if cfg.GitHub.Labels != nil {
		for _, label := range *cfg.GitHub.Labels {
			if label == "" || label != strings.TrimSpace(label) || strings.Contains(label, ",") || strings.ContainsFunc(label, unicode.IsControl) {
				return fmt.Errorf("invalid github.labels entry %q", label)
			}
		}
	}
	for tag, architectures := range cfg.ImageSources {
		for arch, source := range architectures {
			platform := image.Platform{Tag: tag, Arch: arch}
			if err := platform.Validate(); err != nil {
				return fmt.Errorf("image.%s.%s: %w", tag, arch, err)
			}
			if err := source.Validate(); err != nil {
				return fmt.Errorf("image.%s.%s: %w", tag, arch, err)
			}
		}
	}
	return nil
}

func ReadKey(path string) ([]byte, error) {
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

func WriteSetup(dir string, configData, keyData []byte) error {
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

// RecoverSetupKey replaces an uncommitted credential after authenticating it.
// The caller must have verified that the service has never been installed.
func RecoverSetupKey(dir string, configData, keyData []byte, validate func() error) error {
	destination := filepath.Join(dir, "config.yaml")
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("residual key recovery requires %s to be absent", destination)
	}
	credentials := filepath.Join(dir, "credentials")
	keyPath := filepath.Join(credentials, "github-app.pem")
	for path, mode := range map[string]os.FileMode{credentials: os.ModeDir | 0700, keyPath: 0600} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode() != mode {
			return fmt.Errorf("invalid owner or permissions for %s", path)
		}
	}
	saved, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(saved, keyData) {
		if err := validate(); err != nil {
			return err
		}
		if err := writeConfigFile(keyPath, keyData, 0600, os.Rename); err != nil {
			return err
		}
	}
	return writeNewFile(destination, configData, 0644)
}

func Replace(path string, data []byte) error {
	return writeConfigFile(path, data, 0644, os.Rename)
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	return writeConfigFile(path, data, mode, os.Link)
}

func writeConfigFile(path string, data []byte, mode os.FileMode, publish func(string, string) error) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".ephemera-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(data)
	modeErr := temp.Chmod(mode)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, modeErr, syncErr, closeErr); err != nil {
		return err
	}
	return publish(temp.Name(), path)
}

func Defaults() Config {
	var cfg Config
	cfg.Version = 1
	cfg.GitHub.ScaleSetName = "ephemera-ubuntu-amd64"
	cfg.Runner.MaxConcurrent = 2
	cfg.Runner.StartupTimeout = 3 * time.Minute
	cfg.Runner.IdleTimeout = 5 * time.Minute
	cfg.VM.Backend = "qemu"
	cfg.VM.Image, cfg.VM.Arch = "ubuntu", "amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		cfg.VM.Arch = "arm64"
		cfg.GitHub.ScaleSetName = "ephemera-ubuntu-arm64"
	}
	cfg.VM.VCPUs, cfg.VM.MemoryMiB = 2, 4096
	return cfg
}

func (cfg Config) ImageConfig() (image.Config, error) {
	if cfg.VM.Backend == "tart" {
		return image.Config{}, errors.New("Tart images are prepared by the Tart backend")
	}
	platform := image.Platform{Tag: cfg.VM.Image, Arch: cfg.VM.Arch}
	if cfg.VM.Arch == "" {
		return image.Config{}, errors.New("configure vm.image and vm.arch before starting managed image preparation")
	}
	source, configured := cfg.ImageSources[platform.Tag][platform.Arch]
	if !configured {
		var err error
		source, err = image.DefaultSource(platform)
		if err != nil {
			return image.Config{}, err
		}
	}
	return image.Config{
		Platform: platform, Source: source, VCPUs: cfg.VM.VCPUs, MemoryMiB: cfg.VM.MemoryMiB, DiskGiB: cfg.VM.DiskGiB,
	}, nil
}

// VMDataPath returns the backend's resource directory. Recovery records and
// daemon sockets remain in the original state directory.
func (cfg Config) VMDataPath(root string) string {
	if cfg.VM.DataDir != "" {
		return cfg.VM.DataDir
	}
	if cfg.VM.Backend == "tart" {
		return filepath.Join(root, "tart")
	}
	return root
}

// VMImageCachePath returns the QEMU baseline cache directory.
func (cfg Config) VMImageCachePath(cache string) string {
	if cfg.VM.ImageCacheDir != "" {
		return cfg.VM.ImageCacheDir
	}
	return cache
}
