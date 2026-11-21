package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"charm.land/huh/v2"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"go.yaml.in/yaml/v3"
)

type upstreamImage struct {
	profile string
	arch    string
	labels  []string
}

// Keep the catalog snapshot aligned with actions/runner-images#available-image.
var upstreamImages = []upstreamImage{
	{"ubuntu-26.04", "amd64", []string{"ubuntu-26.04"}},
	{"ubuntu-26.04-arm", "arm64", []string{"ubuntu-26.04-arm"}},
	{"ubuntu-24.04", "amd64", []string{"ubuntu-24.04", "ubuntu-latest"}},
	{"ubuntu-24.04-arm", "arm64", []string{"ubuntu-24.04-arm"}},
	{"ubuntu-22.04", "amd64", []string{"ubuntu-22.04"}},
	{"ubuntu-22.04-arm", "arm64", []string{"ubuntu-22.04-arm"}},
	{"ubuntu-slim", "amd64", []string{"ubuntu-slim"}},
	{"xcode-27", "arm64", []string{"xcode-27", "xcode-27-xlarge"}},
	{"macos-26-intel", "amd64", []string{"macos-26-intel", "macos-latest-large", "macos-26-large"}},
	{"macos-26", "arm64", []string{"macos-26", "macos-latest", "macos-26-xlarge"}},
	{"macos-15-intel", "amd64", []string{"macos-15-intel", "macos-15-large"}},
	{"macos-15", "arm64", []string{"macos-15", "macos-15-xlarge"}},
	{"macos-14", "amd64", []string{"macos-14"}},
	{"windows-2025-vs2026", "amd64", []string{"windows-2025", "windows-2025-vs2026", "windows-latest"}},
	{"windows-2022", "amd64", []string{"windows-2022"}},
	{"windows-11-arm", "arm64", []string{"windows-11-arm", "windows-11-vs2026-arm"}},
}

func prepareFromCatalog(path string) error {
	cfg, err := config.Load(path)
	hasConfig := err == nil
	if errors.Is(err, os.ErrNotExist) {
		cfg = config.Defaults()
	} else if err != nil {
		return err
	}

	selected := cfg
	var catalog string
	if err := huh.NewSelect[string]().Title("Image catalog").Options(
		huh.NewOption("Ephemera presets", "presets"),
		huh.NewOption("GitHub runner images", "github"),
	).Value(&catalog).Run(); err != nil {
		return err
	}
	if catalog == "github" {
		entry, err := selectUpstreamImage()
		if err != nil {
			return err
		}
		if entry.profile != "ubuntu-24.04" && entry.profile != "ubuntu-24.04-arm" {
			return fmt.Errorf("%s (%s): %s", entry.profile, entry.arch, entry.requirements())
		}
		selected.VM.Image, selected.VM.Arch, selected.VM.Profile = "ubuntu", entry.arch, "ubuntu-24.04"
		source, err := image.DefaultSource(image.Platform{Tag: selected.VM.Image, Arch: selected.VM.Arch})
		if err != nil {
			return err
		}
		selected.ImageSources = maps.Clone(cfg.ImageSources)
		if selected.ImageSources == nil {
			selected.ImageSources = make(map[string]map[string]image.Source)
		}
		selected.ImageSources["ubuntu"] = maps.Clone(cfg.ImageSources["ubuntu"])
		if selected.ImageSources["ubuntu"] == nil {
			selected.ImageSources["ubuntu"] = make(map[string]image.Source)
		}
		selected.ImageSources["ubuntu"][entry.arch] = source
	} else {
		selected.VM.Profile = ""
		if err := choosePreset(&selected); err != nil {
			return err
		}
	}

	ok, err := confirmSetup("Prepare this image?", selected.VM.Image+" / "+selected.VM.Arch)
	if err != nil || !ok {
		return err
	}
	imageCfg, err := selected.ImageConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	prepared, err := image.Prepare(ctx, image.DefaultCache, imageCfg, logger)
	stop()
	if err != nil {
		return err
	}
	fmt.Println("Prepared image:", prepared.Path)
	if !hasConfig || (cfg.VM == selected.VM &&
		cfg.ImageSources[cfg.VM.Image][cfg.VM.Arch] == selected.ImageSources[selected.VM.Image][selected.VM.Arch]) {
		return nil
	}
	ok, err = confirmSetup("Use this image in the saved configuration?", "The service will use it after you apply the configuration.")
	if err != nil || !ok {
		return err
	}
	data, err := yaml.Marshal(selected)
	if err != nil {
		return err
	}
	before, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return saveEditedConfig(path, before, data)
}

func choosePreset(cfg *config.Config) error {
	if err := huh.NewSelect[string]().Title("Operating system").Options(
		huh.NewOption("Ubuntu", "ubuntu"), huh.NewOption("Windows", "windows"),
	).Value(&cfg.VM.Image).Run(); err != nil {
		return err
	}
	return huh.NewSelect[string]().Title("Architecture").Options(
		huh.NewOption("amd64", "amd64"), huh.NewOption("arm64", "arm64"),
	).Value(&cfg.VM.Arch).Run()
}

func selectUpstreamImage() (upstreamImage, error) {
	var options []huh.Option[int]
	for i, image := range upstreamImages {
		status := "recipe pending"
		if image.profile == "ubuntu-24.04" || image.profile == "ubuntu-24.04-arm" {
			status = "Ubuntu base build"
		} else if image.profile == "macos-14" {
			status = "retired on GitHub Actions"
		}
		for _, label := range image.labels {
			options = append(options, huh.NewOption(fmt.Sprintf("%s (%s, %s)", label, image.arch, status), i))
		}
	}
	var choice int
	err := huh.NewSelect[int]().Title("GitHub runner image").Options(options...).Height(12).Value(&choice).Run()
	return upstreamImages[choice], err
}

func (image upstreamImage) requirements() string {
	switch {
	case strings.HasPrefix(image.profile, "windows"):
		return "requires Windows installation media and an Ephemera build recipe"
	case strings.HasPrefix(image.profile, "macos"), strings.HasPrefix(image.profile, "xcode"):
		return "requires a compatible macOS host and an Ephemera build recipe"
	default:
		return "requires an Ephemera build recipe"
	}
}
