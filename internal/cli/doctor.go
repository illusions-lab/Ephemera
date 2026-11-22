package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/illusions-lab/Ephemera/internal/backend"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/qemuplatform"
	"github.com/illusions-lab/Ephemera/internal/service"
	"github.com/illusions-lab/Ephemera/internal/status"
	"golang.org/x/sys/unix"
)

type doctorPaths struct {
	config, key, cache string
}

type doctorChecks struct {
	loadConfig    func(string) (config.Config, error)
	readKey       func(string) ([]byte, error)
	probeScaleSet func(context.Context, orders.Config) (bool, error)
	checkImage    func(context.Context, config.Config, string, string) (string, error)
	hostArch      string
	lookPath      func(string) (string, error)
	checkKVM      func() error
	diskSpace     func(string) (uint64, string, error)
}

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := flags.String("config", host.ConfigPath(), "configuration file")
	keyPath := flags.String("github-app-key", host.KeyPath(), "GitHub App PEM file")
	repair := flags.Bool("repair", false, "recover and apply the saved macOS service configuration")
	cacheDir := flags.String("image-cache", host.ImageCache(), "runner image cache directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to doctor")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *repair {
		if runtime.GOOS != "darwin" {
			return errors.New("use sudo ephe config apply to repair the Linux service")
		}
		if *path != host.ConfigPath() || *keyPath != host.KeyPath() || *cacheDir != host.ImageCache() {
			return errors.New("doctor --repair uses the installed configuration and credential paths")
		}
		if err := applyMacConfig(ctx, true); err != nil {
			return err
		}
	}
	if runtime.GOOS == "darwin" {
		return diagnoseMac(ctx, *path, *keyPath)
	}
	err := diagnose(ctx, os.Stdout, doctorPaths{config: *path, key: *keyPath, cache: *cacheDir}, doctorChecks{
		loadConfig: config.Load, readKey: config.ReadKey, probeScaleSet: orders.ProbeScaleSet,
		checkImage: backend.CheckImage, hostArch: linuxHostArch(),
		lookPath: exec.LookPath, checkKVM: checkKVM, diskSpace: availableDisk,
	})
	if err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil || !cfg.Runner.Execute {
		return err
	}
	current, _ := status.Read(ctx, host.StatusSocket())
	return diagnoseCapacity(ctx, cfg, current, os.Stdout)
}

func diagnose(ctx context.Context, output io.Writer, paths doctorPaths, checks doctorChecks) error {
	failed := false
	report := func(result, check, detail string) {
		failed = failed || result == "FAIL"
		detail = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, detail)
		fmt.Fprintf(output, "[%s] %-19s %s\n", result, check, detail)
	}
	cfg, configErr := checks.loadConfig(paths.config)
	if configErr == nil {
		report("OK", "Configuration", paths.config)
		if checks.hostArch != "" && cfg.VM.Arch != "" && cfg.VM.Arch != checks.hostArch {
			report("FAIL", "Architecture", fmt.Sprintf("Configured guest %s requires a matching host (current %s).", cfg.VM.Arch, checks.hostArch))
		}
	} else {
		report("FAIL", "Configuration", fmt.Sprintf("%v. Review the file or run sudo ephe setup.", configErr))
	}

	key, keyErr := checks.readKey(paths.key)
	switch {
	case keyErr == nil:
		report("OK", "GitHub App key", "Readable RSA private key")
	case errors.Is(keyErr, os.ErrPermission):
		report("FAIL", "GitHub App key", "Cannot read the protected credential. Run sudo ephe doctor.")
	default:
		report("FAIL", "GitHub App key", fmt.Sprintf("%v. Check the key path or run sudo ephe setup.", keyErr))
	}

	if configErr != nil || keyErr != nil {
		report("SKIP", "GitHub / Scale Set", "A valid configuration and App key are required.")
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		found, err := checks.probeScaleSet(probeCtx, runnerRouting(cfg, key))
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			report("FAIL", "GitHub / Scale Set", "GitHub probe timed out. Check network access from the runner host.")
		case err != nil:
			report("FAIL", "GitHub / Scale Set", fmt.Sprintf("%v. Check network access, App installation, and runner management permissions.", err))
		case !found:
			report("OK", "GitHub App access", "GitHub authenticated the App and granted runner management access.")
			report("FAIL", "Scale Set", fmt.Sprintf("%q was not found in runner group %q. Check the name and service startup logs.", cfg.GitHub.ScaleSetName, cfg.RunnerGroup()))
		default:
			report("OK", "GitHub App access", "GitHub authenticated the App and granted runner management access.")
			report("OK", "Scale Set", fmt.Sprintf("%q is reachable.", cfg.GitHub.ScaleSetName))
		}
	}

	if configErr != nil {
		report("SKIP", "Runner image cache", "A valid configuration is required.")
	} else {
		cacheCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cached, err := checks.checkImage(cacheCtx, cfg, host.StateDir(), paths.cache)
		cancel()
		switch {
		case err == nil:
			report("OK", "Runner image cache", cached)
		case errors.Is(err, os.ErrPermission):
			report("FAIL", "Runner image cache", "Cannot read the image cache. Run sudo ephe doctor.")
		default:
			report("FAIL", "Runner image cache", fmt.Sprintf("%v. Check image settings and run sudo ephe image prepare.", err))
		}
	}

	platform, platformErr := qemuplatform.ForArch(checks.hostArch)
	if platformErr != nil {
		report("FAIL", "QEMU host", "The Ubuntu runner image requires Linux amd64 or arm64.")
		report("SKIP", "KVM", "Run this check on the Linux runner host.")
	} else {
		var missing []string
		for _, binary := range []string{platform.Binary, "qemu-img"} {
			if _, err := checks.lookPath(binary); err != nil {
				missing = append(missing, binary)
			}
		}
		if len(missing) > 0 {
			report("FAIL", "QEMU", "Missing: "+strings.Join(missing, ", ")+". Install the QEMU packages on the host.")
		} else {
			report("OK", "QEMU", platform.Binary+" and qemu-img are available.")
		}
		if err := platform.CheckFirmware(); err != nil {
			report("FAIL", "QEMU firmware", err.Error())
		}
		if err := checks.checkKVM(); err != nil {
			report("WARN", "KVM", fmt.Sprintf("%v. QEMU image verification can fall back to software emulation.", err))
		} else {
			report("OK", "KVM", "/dev/kvm is accessible.")
		}
	}

	free, volume, err := checks.diskSpace(paths.cache)
	if err != nil {
		report("FAIL", "Disk space", err.Error())
	} else if free == 0 {
		report("FAIL", "Disk space", fmt.Sprintf("No available space on the filesystem containing %q.", volume))
	} else {
		report("OK", "Disk space", fmt.Sprintf("%.1f GiB available on the filesystem containing %q.", float64(free)/(1<<30), volume))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed {
		return errors.New("one or more checks failed")
	}
	return nil
}

func checkKVM() error {
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return file.Close()
}

func availableDisk(path string) (uint64, string, error) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return 0, "", err
	}
	for {
		var fs unix.Statfs_t
		err := unix.Statfs(dir, &fs)
		if err == nil {
			return uint64(fs.Bavail) * uint64(fs.Bsize), dir, nil
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, os.ErrNotExist) || parent == dir {
			return 0, "", fmt.Errorf("check free space at %q: %w", dir, err)
		}
		dir = parent
	}
}

func linuxHostArch() string {
	if runtime.GOOS == "linux" {
		return runtime.GOARCH
	}
	return ""
}

func diagnoseMac(ctx context.Context, configPath, keyPath string) error {
	if pending, err := macos.ActivationPending(); err != nil {
		return err
	} else if pending {
		return errors.New("an interrupted LaunchAgent activation is pending; run ephe setup or ephe doctor --repair")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.VM.Backend != "tart" {
		return errors.New("macOS runner configuration must use Tart")
	}
	key, err := config.ReadKey(keyPath)
	if err != nil {
		return fmt.Errorf("read GitHub App key: %w", err)
	}
	fmt.Println("Configuration and GitHub App key are readable.")
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	found, err := orders.ProbeScaleSet(probe, runnerRouting(cfg, key))
	cancel()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Scale Set %q was not found in runner group %q; run ephe setup or ephe doctor --repair", cfg.GitHub.ScaleSetName, cfg.RunnerGroup())
	}
	if cfg.Runner.Execute {
		ref, err := backend.CheckImage(ctx, cfg, host.StateDir(), host.ImageCache())
		if err != nil {
			return fmt.Errorf("check runner image cache: %w; run ephe setup or ephe doctor --repair", err)
		}
		fmt.Printf("Tart %s image cache contains %s.\n", macos.TartVersion, ref)
	}
	free, path, err := availableDisk(macos.Root())
	if err != nil {
		return err
	}
	fmt.Printf("%.1f GiB available on %s.\n", float64(free)/(1<<30), path)
	current, err := status.Read(ctx, host.StatusSocket())
	if err != nil {
		return fmt.Errorf("read LaunchAgent status: %w; run ephe setup or ephe doctor --repair", err)
	}
	fmt.Printf("Ephemera %s, GitHub %s, intake %s.\n", current.Phase, current.GitHub, current.Intake)
	if current.GitHub != "connected" {
		return errors.New("Ephemera is not connected to GitHub; check the daemon log and run ephe setup or ephe doctor --repair")
	}
	if current.Intake == "observing" {
		fmt.Println("Observation mode. To accept jobs, select Execute jobs in ephe config, then run ephe setup.")
	}
	if cfg.Runner.Execute {
		return diagnoseCapacity(ctx, cfg, current, os.Stdout)
	}
	return nil
}

func diagnoseCapacity(ctx context.Context, cfg config.Config, current status.Snapshot, output io.Writer) error {
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	total, err := service.HostMemoryMiB(probe)
	if err != nil {
		return fmt.Errorf("check host memory: %w", err)
	}
	digest, err := config.Digest(cfg)
	if err != nil {
		return err
	}
	return reportDoctorCapacity(cfg, current, digest, total, output)
}

func reportDoctorCapacity(cfg config.Config, current status.Snapshot, digest string, total uint64, output io.Writer) error {
	reserve := cfg.VM.HostReserveMiB
	if reserve == 0 {
		reserve = 1024
	}
	required := uint64(cfg.VM.MemoryMiB) + uint64(reserve)
	if required > total {
		return fmt.Errorf("VM memory plus host reserve requires %d MiB but this host has %d MiB; review VM memory and host reserve in ephe config", required, total)
	}
	if current.ConfigDigest == digest && current.Instances != nil && current.Instances.Capacity != nil {
		printCapacity(output, current)
		if current.Instances.Capacity.Startable == 0 && current.Instances.CapacityReason != "" {
			fmt.Fprintf(output, "No VM can start currently: %s.\n", current.Instances.CapacityReason)
		}
	}
	return nil
}
