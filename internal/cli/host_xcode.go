package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type hostXcode struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Build   string `json:"build"`
}

func checkHostXcode(args []string) error {
	flags := flag.NewFlagSet("image check-host-xcode", flag.ContinueOnError)
	path := flags.String("path", "auto", "Xcode application path or auto")
	version := flags.String("version", "", "required Xcode version")
	build := flags.String("build", "", "required Xcode build")
	asJSON := flags.Bool("json", false, "emit path, version and build as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *path == "" {
		return errors.New("usage: ephe image check-host-xcode [--path APP] [--version VERSION] [--build BUILD] [--json]")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("host Xcode is only available on macOS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	xcode, err := findHostXcode(ctx, *path)
	if err != nil {
		return err
	}
	if (*version != "" && xcode.Version != *version) || (*build != "" && xcode.Build != *build) {
		return fmt.Errorf("Xcode %s (%s) at %s does not match required version %q and build %q; install the matching Apple Xcode or pass --path",
			xcode.Version, xcode.Build, xcode.Path, *version, *build)
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(xcode)
	}
	fmt.Printf("Host Xcode %s (%s) at %s\n", xcode.Version, xcode.Build, xcode.Path)
	return nil
}

func findHostXcode(ctx context.Context, path string) (hostXcode, error) {
	if path == "auto" {
		path = "/Applications/Xcode.app"
		if out, err := exec.CommandContext(ctx, "/usr/bin/xcode-select", "-p").Output(); err == nil {
			selected, _ := strings.CutSuffix(strings.TrimSpace(string(out)), "/Contents/Developer")
			if strings.HasSuffix(selected, ".app") {
				path = selected
			}
		}
	}
	bundle, err := filepath.Abs(path)
	if err != nil {
		return hostXcode{}, err
	}
	if !strings.HasSuffix(bundle, ".app") {
		return hostXcode{}, errors.New("select a complete Xcode .app bundle")
	}
	resolved, err := filepath.EvalSymlinks(bundle)
	if err != nil {
		return hostXcode{}, fmt.Errorf("install full Xcode from Apple or pass --path to an installed Xcode.app: %w", err)
	}
	tool := filepath.Join(resolved, "Contents", "Developer", "usr", "bin", "xcodebuild")
	if info, err := os.Stat(tool); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return hostXcode{}, fmt.Errorf("no complete Xcode installation at %s; install full Xcode from Apple or pass --path", resolved)
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", `-R=anchor apple generic and identifier "com.apple.dt.Xcode" and certificate leaf[subject.O] = "Apple Inc."`, resolved).CombinedOutput(); err != nil {
		return hostXcode{}, fmt.Errorf("verify Apple signature of Xcode at %s: %w: %s", resolved, err, strings.TrimSpace(string(output)))
	}
	cmd := exec.CommandContext(ctx, tool, "-version")
	cmd.Env = append(cmd.Environ(), "DEVELOPER_DIR="+filepath.Join(resolved, "Contents", "Developer"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return hostXcode{}, fmt.Errorf("read host Xcode version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	version, build := xcodeVersion(string(output))
	if version == "" || build == "" {
		return hostXcode{}, fmt.Errorf("could not read Xcode version and build at %s", resolved)
	}
	return hostXcode{Path: resolved, Version: version, Build: build}, nil
}

func xcodeVersion(output string) (version, build string) {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, "Xcode "); ok {
			version = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "Build version "); ok {
			build = strings.TrimSpace(value)
		}
	}
	return
}
