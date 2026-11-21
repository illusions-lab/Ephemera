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

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/qemuplatform"
	"golang.org/x/sys/unix"
)

type doctorPaths struct {
	config, key, cache string
}

type doctorChecks struct {
	loadConfig    func(string) (config.Config, error)
	readKey       func(string) ([]byte, error)
	probeScaleSet func(context.Context, orders.Config) (bool, error)
	checkImage    func(context.Context, string, image.Config) (image.Image, error)
	hostArch      string
	lookPath      func(string) (string, error)
	checkKVM      func() error
	diskSpace     func(string) (uint64, string, error)
}

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := flags.String("config", filepath.Join(config.Dir, "config.yaml"), "configuration file")
	keyPath := flags.String("github-app-key", filepath.Join(config.Dir, "credentials", "github-app.pem"), "GitHub App PEM file")
	cacheDir := flags.String("image-cache", image.DefaultCache, "runner image cache directory")
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
	return diagnose(ctx, os.Stdout, doctorPaths{config: *path, key: *keyPath, cache: *cacheDir}, doctorChecks{
		loadConfig: config.Load, readKey: config.ReadKey, probeScaleSet: orders.ProbeScaleSet,
		checkImage: image.CheckCached, hostArch: linuxHostArch(),
		lookPath: exec.LookPath, checkKVM: checkKVM, diskSpace: availableDisk,
	})
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
		found, err := checks.probeScaleSet(probeCtx, orders.Config{
			ScopeURL: cfg.GitHub.ScopeURL, ScaleSetName: cfg.GitHub.ScaleSetName,
			AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID, PrivateKey: key,
		})
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			report("FAIL", "GitHub / Scale Set", "GitHub probe timed out. Check network access from the runner host.")
		case err != nil:
			report("FAIL", "GitHub / Scale Set", fmt.Sprintf("%v. Check network access, App installation, and runner management permissions.", err))
		case !found:
			report("OK", "GitHub App access", "GitHub authenticated the App and granted runner management access.")
			report("FAIL", "Scale Set", fmt.Sprintf("%q was not found in the default runner group. Check the name and service startup logs.", cfg.GitHub.ScaleSetName))
		default:
			report("OK", "GitHub App access", "GitHub authenticated the App and granted runner management access.")
			report("OK", "Scale Set", fmt.Sprintf("%q is reachable.", cfg.GitHub.ScaleSetName))
		}
	}

	if configErr != nil {
		report("SKIP", "Runner image cache", "A valid configuration is required.")
	} else {
		imageCfg, err := cfg.ImageConfig()
		var cached image.Image
		if err == nil {
			cacheCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			cached, err = checks.checkImage(cacheCtx, paths.cache, imageCfg)
			cancel()
		}
		switch {
		case err == nil:
			report("OK", "Runner image cache", cached.Path)
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
