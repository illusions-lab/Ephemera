package setup

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"go.yaml.in/yaml/v3"
)

// Edit opens an existing configuration without applying it to the service.
func Edit(path string) error {
	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no configuration at %s; run ephe setup first", path)
	}
	if err != nil {
		return err
	}
	before, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	var notice string
	for {
		mode := "Observe only"
		if cfg.Runner.Execute {
			mode = "Execute jobs"
		}
		var choice string
		err := huh.NewSelect[string]().Title("Ephemera configuration").
			Description(strings.Join([]string{mode + " · " + cfg.GitHub.ScaleSetName, notice}, "\n")).
			Options(
				huh.NewOption("Runner", "runner"),
				huh.NewOption("VM and images", "vm"),
				huh.NewOption("GitHub connection", "github"),
				huh.NewOption("Review changes and save", "save"),
				huh.NewOption("Exit", "exit"),
			).Value(&choice).Run()
		if errors.Is(err, huh.ErrUserAborted) {
			choice, err = "exit", nil
		}
		if err != nil {
			return err
		}
		notice = ""
		switch choice {
		case "runner":
			err = editRunner(&cfg)
		case "vm":
			err = editImages(&cfg)
		case "github":
			err = editGitHub(&cfg)
		case "save", "exit":
			data, marshalErr := yaml.Marshal(cfg)
			if marshalErr != nil {
				return marshalErr
			}
			changes, diffErr := configChanges(before, data)
			if diffErr != nil {
				return diffErr
			}
			if choice == "exit" {
				if changes == "" {
					return nil
				}
				discard, confirmErr := confirmSetup("Discard unsaved changes?", "The saved configuration will be kept.")
				if discard && confirmErr == nil {
					return nil
				}
				err = confirmErr
				break
			}
			if changes == "" {
				notice = "No changes to save."
				continue
			}
			if err = cfg.Validate(); err != nil {
				break
			}
			fmt.Println(changes)
			save, confirmErr := confirmSetup("Save these changes?", path+"\n"+CapacityPreview(cfg))
			if confirmErr != nil || !save {
				err = confirmErr
				break
			}
			if err = saveEditedConfig(path, before, data); err == nil {
				apply := "ephe config apply"
				if runtime.GOOS == "linux" {
					apply = "sudo ephe config apply"
				}
				fmt.Printf("Saved %s\nRun %s to apply the changes.\n", path, apply)
				return nil
			}
		}
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			notice = err.Error()
		}
	}
}

func editRunner(cfg *config.Config) error {
	runner := cfg.Runner
	concurrent := strconv.Itoa(runner.MaxConcurrent)
	startup, idle := runner.StartupTimeout.String(), runner.IdleTimeout.String()
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[bool]().Title("Should Ephemera accept jobs when the service starts?").Options(
			huh.NewOption("Observe only", false),
			huh.NewOption("Execute jobs", true),
		).Value(&runner.Execute),
		huh.NewInput().Title("Maximum concurrent runners").Value(&concurrent).Validate(intRange(1, math.MaxInt32)),
		huh.NewInput().Title("Runner startup timeout").Value(&startup).Validate(positiveDuration),
		huh.NewInput().Title("Runner idle timeout").Value(&idle).Validate(positiveDuration),
	)).Run()
	if err != nil {
		return err
	}
	runner.MaxConcurrent, _ = strconv.Atoi(strings.TrimSpace(concurrent))
	runner.StartupTimeout, _ = time.ParseDuration(strings.TrimSpace(startup))
	runner.IdleTimeout, _ = time.ParseDuration(strings.TrimSpace(idle))
	cfg.Runner = runner
	return nil
}

func editScaleSetRouting(cfg *config.Config) error {
	group := cfg.RunnerGroup()
	var labels string
	if cfg.GitHub.Labels != nil {
		labels = strings.Join(*cfg.GitHub.Labels, ", ")
	}
	before := labels
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("GitHub Runner Group").Description("Default is the standard group.").Value(&group).Validate(requiredInput),
		huh.NewInput().Title("Additional Scale Set labels").Description("Comma-separated. Ephemera adds the Scale Set name and platform labels automatically.").Value(&labels),
	)).Run(); err != nil {
		return err
	}
	group = strings.TrimSpace(group)
	if group != cfg.RunnerGroup() {
		cfg.GitHub.RunnerGroup = group
	}
	if labels != before {
		extra := parseExtraLabels(labels)
		cfg.GitHub.Labels = &extra
	}
	return nil
}

func parseExtraLabels(value string) []string {
	result := []string{}
	seen := make(map[string]bool)
	for _, label := range strings.Split(value, ",") {
		label = strings.TrimSpace(label)
		key := strings.ToLower(label)
		if label != "" && !seen[key] {
			seen[key] = true
			result = append(result, label)
		}
	}
	return result
}

func editGitHub(cfg *config.Config) error {
	github := cfg.GitHub
	var app, installation string
	if github.AppID > 0 {
		app = strconv.FormatInt(github.AppID, 10)
	}
	if github.InstallationID > 0 {
		installation = strconv.FormatInt(github.InstallationID, 10)
	}
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("GitHub repository or organization URL").Value(&github.ScopeURL).Validate(config.ValidateScope),
		huh.NewInput().Title("Runner Scale Set name").Value(&github.ScaleSetName).Validate(requiredInput),
		huh.NewInput().Title("GitHub App ID").Value(&app).Validate(positiveNumber),
		huh.NewInput().Title("GitHub App installation ID").Value(&installation).Validate(positiveNumber),
	)).Run()
	if err != nil {
		return err
	}
	github.ScaleSetName = strings.TrimSpace(github.ScaleSetName)
	github.AppID, _ = strconv.ParseInt(strings.TrimSpace(app), 10, 64)
	github.InstallationID, _ = strconv.ParseInt(strings.TrimSpace(installation), 10, 64)
	draft := *cfg
	draft.GitHub = github
	if err := editScaleSetRouting(&draft); err != nil {
		return err
	}
	*cfg = draft
	return nil
}

func editImages(cfg *config.Config) error {
	if cfg.VM.Backend != "qemu" {
		return editVM(cfg)
	}
	var choice string
	err := huh.NewSelect[string]().Title("VM and images").Options(
		huh.NewOption("VM settings and selected image", "vm"),
		huh.NewOption("Custom image sources", "sources"),
	).Value(&choice).Run()
	if err != nil {
		return err
	}
	if choice == "sources" {
		return editImageSource(cfg)
	}
	return editVM(cfg)
}

func editVM(cfg *config.Config) error {
	vm := cfg.VM
	cpus, memory := strconv.Itoa(vm.VCPUs), strconv.Itoa(vm.MemoryMiB)
	disk := strconv.Itoa(vm.DiskGiB)
	diskReserve := strconv.Itoa(vm.HostDiskReserveGiB)
	memoryCredit := strconv.Itoa(vm.MemoryOversubscribeMiB)
	cacheMax := strconv.Itoa(vm.CacheMaxGiB)
	retained := strconv.Itoa(vm.ExtraRetainedImages)
	diskTitle := "Guest disk (GiB, 0 uses the default of 12)"
	if vm.Backend == "tart" {
		diskTitle = "Guest disk (GiB, 0 inherits the Tart image size)"
	}
	reserve := strconv.Itoa(vm.HostReserveMiB)
	auto := "default"
	if cfg.Runner.ImageAutoUpdate != nil {
		auto = strconv.FormatBool(*cfg.Runner.ImageAutoUpdate)
	}
	fields := []huh.Field{
		huh.NewInput().Title("VM CPU count").Value(&cpus).Validate(intRange(1, math.MaxInt)),
		huh.NewInput().Title("VM memory (MiB)").Value(&memory).Validate(intRange(1, math.MaxInt)),
		huh.NewInput().Title("Host memory reserve (MiB, 0 uses 1024)").Value(&reserve).Validate(func(value string) error {
			if strings.TrimSpace(value) == "0" {
				return nil
			}
			return intRange(256, math.MaxInt32)(value)
		}),
		huh.NewInput().Title("VM data directory (absolute path; blank uses default)").Value(&vm.DataDir),
		huh.NewInput().Title(diskTitle).Value(&disk).Validate(func(value string) error {
			if strings.TrimSpace(value) == "0" {
				return nil
			}
			return intRange(12, 1024)(value)
		}),
		huh.NewInput().Title("Extra memory admission budget (MiB, 0 disables oversubscription)").
			Value(&memoryCredit).Validate(intRange(0, 4096)),
		huh.NewInput().Title("Host disk reserve (GiB, 0 uses 5)").Value(&diskReserve).Validate(intRange(0, math.MaxInt32)),
		huh.NewInput().Title("Image cache budget (GiB, 0 means unlimited)").Value(&cacheMax).Validate(intRange(0, math.MaxInt32)),
		huh.NewInput().Title("Extra retired images to retain").Value(&retained).Validate(intRange(0, 100)),
		huh.NewSelect[string]().Title("Automatic runner image updates").Options(
			huh.NewOption("Use default (enabled for new installations)", "default"), huh.NewOption("Enabled", "true"), huh.NewOption("Disabled", "false"),
		).Value(&auto),
	}
	if vm.Backend == "tart" {
		fields = append(fields,
			huh.NewInput().Title("Tart image reference (verified local build or pinned OCI digest)").Value(&vm.ImageRef),
			huh.NewSelect[string]().Title("Verify Apple toolchain in the VM").Options(
				huh.NewOption("No Xcode check", ""), huh.NewOption("Xcode and macOS SDK", "macos"),
				huh.NewOption("Xcode, macOS SDK, and iOS device SDK", "ios"),
			).Value(&vm.AppleToolchain),
		)
	} else {
		fields = append(fields,
			huh.NewInput().Title("Image tag").Value(&vm.Image).Validate(requiredInput),
			huh.NewInput().Title("Image cache directory (absolute path; blank uses default)").Value(&vm.ImageCacheDir),
			huh.NewInput().Title("Image architecture (amd64 or arm64)").Value(&vm.Arch),
			huh.NewInput().Title("Image profile (optional, ubuntu-24.04)").Value(&vm.Profile),
		)
	}
	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return err
	}
	vm.VCPUs, _ = strconv.Atoi(strings.TrimSpace(cpus))
	vm.MemoryMiB, _ = strconv.Atoi(strings.TrimSpace(memory))
	vm.HostReserveMiB, _ = strconv.Atoi(strings.TrimSpace(reserve))
	vm.DiskGiB, _ = strconv.Atoi(strings.TrimSpace(disk))
	vm.HostDiskReserveGiB, _ = strconv.Atoi(strings.TrimSpace(diskReserve))
	vm.MemoryOversubscribeMiB, _ = strconv.Atoi(strings.TrimSpace(memoryCredit))
	vm.CacheMaxGiB, _ = strconv.Atoi(strings.TrimSpace(cacheMax))
	vm.ExtraRetainedImages, _ = strconv.Atoi(strings.TrimSpace(retained))
	vm.DataDir, vm.ImageCacheDir = strings.TrimSpace(vm.DataDir), strings.TrimSpace(vm.ImageCacheDir)
	vm.Image, vm.Arch = strings.TrimSpace(vm.Image), strings.TrimSpace(vm.Arch)
	vm.ImageRef, vm.Profile = strings.TrimSpace(vm.ImageRef), strings.TrimSpace(vm.Profile)
	cfg.VM = vm
	cfg.Runner.ImageAutoUpdate = nil
	if auto != "default" {
		enabled := auto == "true"
		cfg.Runner.ImageAutoUpdate = &enabled
	}
	return nil
}

func editImageSource(cfg *config.Config) error {
	platform := image.Platform{Tag: "ubuntu", Arch: "amd64"}
	platforms := make([]huh.Option[image.Platform], 0, 4)
	for _, tag := range []string{"ubuntu", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			p := image.Platform{Tag: tag, Arch: arch}
			label := p.String()
			if _, custom := cfg.ImageSources[tag][arch]; custom {
				label += " (custom)"
			}
			platforms = append(platforms, huh.NewOption(label, p))
		}
	}
	if err := huh.NewSelect[image.Platform]().Title("Image source platform").Options(platforms...).Value(&platform).Run(); err != nil {
		return err
	}
	source, custom := cfg.ImageSources[platform.Tag][platform.Arch]
	if custom {
		var action string
		if err := huh.NewSelect[string]().Title(platform.String()).Options(
			huh.NewOption("Edit custom source", "edit"), huh.NewOption("Remove custom source", "remove"),
		).Value(&action).Run(); err != nil {
			return err
		}
		if action == "remove" {
			delete(cfg.ImageSources[platform.Tag], platform.Arch)
			if len(cfg.ImageSources[platform.Tag]) == 0 {
				delete(cfg.ImageSources, platform.Tag)
			}
			return nil
		}
	} else {
		source, _ = image.DefaultSource(platform)
	}
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Base image URL").Value(&source.BaseURL),
		huh.NewInput().Title("Base image SHA-256").Value(&source.BaseSHA256),
		huh.NewInput().Title("Runner version").Value(&source.RunnerVersion),
		huh.NewInput().Title("Runner archive SHA-256").Value(&source.RunnerSHA256),
		huh.NewInput().Title("Ubuntu snapshot timestamp (optional)").Value(&source.Snapshot),
	)).Run(); err != nil {
		return err
	}
	if err := source.Validate(); err != nil {
		return err
	}
	if cfg.ImageSources == nil {
		cfg.ImageSources = make(map[string]map[string]image.Source)
	}
	if cfg.ImageSources[platform.Tag] == nil {
		cfg.ImageSources[platform.Tag] = make(map[string]image.Source)
	}
	cfg.ImageSources[platform.Tag][platform.Arch] = source
	return nil
}

func requiredInput(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("enter a value")
	}
	return nil
}

func intRange(minimum, maximum int) func(string) error {
	return func(value string) error {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < minimum || n > maximum {
			return fmt.Errorf("enter an integer between %d and %d", minimum, maximum)
		}
		return nil
	}
}

// ConfigChanges reports the affected settings using their YAML field names.
func ConfigChanges(before, after config.Config) (string, error) {
	oldData, err := yaml.Marshal(before)
	if err != nil {
		return "", err
	}
	newData, err := yaml.Marshal(after)
	if err != nil {
		return "", err
	}
	return configChanges(oldData, newData)
}

func configChanges(before, after []byte) (string, error) {
	oldFields, newFields := make(map[string]string), make(map[string]string)
	for _, item := range []struct {
		data   []byte
		fields map[string]string
	}{{before, oldFields}, {after, newFields}} {
		var document map[string]any
		if err := yaml.Unmarshal(item.data, &document); err != nil {
			return "", err
		}
		flattenConfig("", document, item.fields)
	}
	fields := maps.Clone(oldFields)
	maps.Copy(fields, newFields)
	var changes strings.Builder
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		old, beforeOK := oldFields[field]
		new, afterOK := newFields[field]
		if beforeOK == afterOK && old == new {
			continue
		}
		if !beforeOK {
			old = "(unset)"
		}
		if !afterOK {
			new = "(unset)"
		}
		fmt.Fprintf(&changes, "%s  %s → %s\n", field, old, new)
	}
	return changes.String(), nil
}

func flattenConfig(prefix string, document map[string]any, fields map[string]string) {
	for key, value := range document {
		if nested, ok := value.(map[string]any); ok {
			flattenConfig(prefix+key+".", nested, fields)
		} else {
			fields[prefix+key] = fmt.Sprint(value)
		}
	}
}
