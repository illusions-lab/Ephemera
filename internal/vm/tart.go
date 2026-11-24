package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/illusions-lab/Ephemera/internal/macos"
)

var pinnedTartSource = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?/[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$`)
var tartName = regexp.MustCompile(`^ephemera-[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

type Tart struct {
	binary, home string
	cpu, memory  int
	diskGiB      int
}

func NewTart(home string, cpu, memory, diskGiB int) (*Tart, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || os.Geteuid() == 0 {
		return nil, errors.New("Tart requires Apple Silicon macOS and a non-root service account")
	}
	binary, err := findTart()
	if err != nil {
		return nil, err
	}
	return &Tart{binary: binary, home: home, cpu: cpu, memory: memory, diskGiB: diskGiB}, nil
}

func findTart() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	releases := filepath.Join(macos.Root(), "releases")
	resolved, err := filepath.EvalSymlinks(releases)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	dir := filepath.Dir(executable)
	if filepath.Dir(dir) == resolved {
		path := filepath.Join(dir, "tart")
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("read release Tart binding: %w", err)
		}
		// The shell and launchd can spell the user's home via different
		// symlinks. Resolve the directories while retaining the bound filename.
		targetDir, err := filepath.EvalSymlinks(filepath.Dir(target))
		if err != nil {
			return "", fmt.Errorf("resolve release Tart binding: %w", err)
		}
		for _, expected := range []string{macos.TartBinary(), macos.BundledTartBinary()} {
			dir, err := filepath.EvalSymlinks(filepath.Dir(expected))
			if err == nil && targetDir == dir && filepath.Base(target) == filepath.Base(expected) {
				return filepath.Join(dir, filepath.Base(expected)), nil
			}
		}
		return "", fmt.Errorf("release Tart binding does not match the embedded Tart %s manifest", macos.TartVersion)
	}

	for _, binary := range []string{macos.TartBinary(), macos.PrebuiltTartBinary()} {
		if _, err := os.Stat(binary); err == nil {
			return binary, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return exec.LookPath("tart")
}

func (t *Tart) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.binary, args...)
	cmd.Env = append(cmd.Environ(), "TART_HOME="+t.home, "TART_NO_AUTO_PRUNE=")
	return cmd
}

func (t *Tart) Check(ctx context.Context) error {
	if err := os.Mkdir(t.home, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(t.home)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("TART_HOME must be a private directory (0700)")
	}
	return t.CheckInstalled(ctx)
}

func (t *Tart) CheckInstalled(ctx context.Context) error {
	out, err := t.command(ctx, "--version").Output()
	if err != nil {
		return fmt.Errorf("read Tart version: %w", err)
	}
	if strings.TrimSpace(string(out)) != macos.TartVersion {
		return fmt.Errorf("Tart %s is required", macos.TartVersion)
	}
	return nil
}

type tartVM struct {
	Name    string
	Running bool
	State   string
}

func (t *Tart) list(ctx context.Context, source string) ([]tartVM, error) {
	out, err := t.command(ctx, "list", "--source", source, "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("list Tart %s VMs: %w", source, err)
	}
	var entries []tartVM
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("decode Tart VM list: %w", err)
	}
	return entries, nil
}

func (t *Tart) ResolveImage(ctx context.Context, ref string, progress io.Writer) (string, func() error, error) {
	if strings.HasPrefix(ref, "local:") {
		return ref, func() error { return t.CheckImage(context.Background(), ref) }, t.CheckImage(ctx, ref)
	}
	pull, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	cmd := t.command(pull, "pull", ref)
	cmd.Stdout, cmd.Stderr = progress, progress
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("pull pinned Tart image: %w", err)
	}
	return ref, func() error { return t.CheckImage(ctx, ref) }, nil
}

func (t *Tart) CheckImage(ctx context.Context, ref string) error {
	info, err := os.Lstat(t.home)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("TART_HOME must be a private directory (0700)")
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if name, local := strings.CutPrefix(ref, "local:"); local {
		if err := t.checkLocalImageReceipt(name); err != nil {
			return err
		}
		entries, err := t.list(check, "local")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name == name && !entry.Running {
				return nil
			}
		}
		return errors.New("local Tart image is missing or running")
	}
	entries, err := t.list(check, "oci")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == ref {
			return nil
		}
	}
	return errors.New("pinned Tart image is missing from the private OCI cache")
}

func (t *Tart) checkLocalImageReceipt(name string) error {
	if id, ok := strings.CutPrefix(name, "github-macos26-"); ok && tartName.MatchString("ephemera-"+id) {
		provenance, err := os.ReadFile(filepath.Join(t.home, name+".github-revision"))
		if err == nil && strings.TrimSpace(string(provenance)) == macos.GitHubImageRevision {
			return nil
		}
		return errors.New("local Tart image is missing the pinned GitHub build receipt")
	}
	if id, ok := strings.CutPrefix(name, "apple-macos-"); ok && tartName.MatchString("ephemera-"+id) {
		var receipt struct {
			Source       string `json:"source"`
			OSBuild      string `json:"os_build"`
			XcodeVersion string `json:"xcode_version"`
			XcodeBuild   string `json:"xcode_build"`
		}
		data, err := os.ReadFile(filepath.Join(t.home, name+".apple-build.json"))
		if err == nil && json.Unmarshal(data, &receipt) == nil && receipt.Source == "apple-ipsw+host-xcode" &&
			receipt.OSBuild != "" && receipt.XcodeVersion != "" && receipt.XcodeBuild != "" {
			return nil
		}
		return errors.New("local Apple Tart image is missing a verified host-Xcode build receipt")
	}
	return errors.New("invalid local Tart image name")
}

func (t *Tart) owns(i Instance) error {
	if i.Namespace != t.home || i.Backend != "tart" || i.Resource != "ephemera-"+i.ID || !tartName.MatchString(i.Resource) {
		return errors.New("invalid Tart instance identity")
	}
	return nil
}

func (t *Tart) Prepare(ctx context.Context, i Instance) error {
	if err := t.owns(i); err != nil {
		return err
	}
	image := strings.TrimPrefix(i.Image, "local:")
	clone := t.command(ctx, "clone", image, i.Resource)
	var diagnostic boundedVMOutput
	clone.Stderr = &diagnostic
	if err := tartDiagnostic(clone.Run(), diagnostic.output.String()); err != nil {
		return fmt.Errorf("clone Tart image: %w", err)
	}
	args := []string{"set", i.Resource, "--cpu", strconv.Itoa(t.cpu), "--memory", strconv.Itoa(t.memory)}
	if t.diskGiB > 0 {
		// Tart accepts decimal GB, whereas Ephemera uses binary GiB.
		diskGB := (uint64(t.diskGiB)*(1<<30) + 999_999_999) / 1_000_000_000
		args = append(args, "--disk-size", strconv.FormatUint(diskGB, 10))
	}
	set := t.command(ctx, args...)
	diagnostic = boundedVMOutput{}
	set.Stderr = &diagnostic
	if err := tartDiagnostic(set.Run(), diagnostic.output.String()); err != nil {
		return fmt.Errorf("configure Tart VM: %w", err)
	}
	return nil
}

func (t *Tart) Start(startup context.Context, i Instance, jit string) (Process, error) {
	if err := t.owns(i); err != nil {
		return Process{}, err
	}
	vmExit, err := t.startVM(i.Resource)
	if err != nil {
		return Process{}, err
	}
	ready, exited := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(exited)
		exited <- t.runGuest(startup, i, jit, ready, vmExit)
	}()
	return Process{Ready: ready, Exited: exited}, nil
}

func (t *Tart) startVM(resource string) (<-chan error, error) {
	run := t.command(context.Background(), "run", "--no-graphics", "--no-audio", "--no-usb-accessories", "--no-clipboard", resource)
	var diagnostic boundedVMOutput
	run.Stderr = &diagnostic
	if err := run.Start(); err != nil {
		return nil, err
	}
	vmExit := make(chan error, 1)
	go func() { vmExit <- tartDiagnostic(run.Wait(), diagnostic.output.String()) }()
	return vmExit, nil
}

func tartDiagnostic(err error, output string) error {
	if err == nil || strings.TrimSpace(output) == "" {
		return err
	}
	err = fmt.Errorf("%w: Tart stderr: %s", err, strings.TrimSpace(output))
	diagnostic := strings.ToLower(output)
	for _, signature := range []string{"disk image is corrupted", "disk image is invalid", "invalid disk image", "corrupt disk image"} {
		if strings.Contains(diagnostic, signature) {
			return errors.Join(ErrImageInfrastructure, err)
		}
	}
	return err
}

func (t *Tart) runGuest(startup context.Context, i Instance, jit string, ready chan struct{}, vmExit <-chan error) error {
	if err := t.waitGuestAgent(startup, i.Resource, vmExit); err != nil {
		return err
	}
	if err := startup.Err(); err != nil {
		return err
	}
	runner := t.command(context.Background(), "exec", "-i", i.Resource,
		"/usr/bin/sudo", "-n", "-u", "ephemera-runner", "/bin/sh", "-c", tartLaunch)
	runner.Stdin = strings.NewReader(jit + "\n")
	lines, err := runner.StdoutPipe()
	if err != nil {
		return err
	}
	if err := runner.Start(); err != nil {
		lines.Close()
		return err
	}
	runnerExit := make(chan error, 1)
	go func() {
		readTartReadiness(lines, ready)
		runnerExit <- runner.Wait()
	}()
	select {
	case err := <-runnerExit:
		// The reconciler stops the VM before reclaiming this instance.
		return err
	case err := <-vmExit:
		_ = runner.Process.Kill()
		<-runnerExit
		return err
	}
}

func (t *Tart) waitGuestAgent(startup context.Context, resource string, vmExit <-chan error) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		probe, cancel := context.WithTimeout(startup, 3*time.Second)
		err := t.command(probe, "exec", resource, "/usr/bin/true").Run()
		cancel()
		if err == nil {
			return nil
		}
		select {
		case err := <-vmExit:
			return errors.Join(errors.New("Tart VM exited before Guest Agent readiness"), err)
		case <-startup.Done():
			return fmt.Errorf("wait for Tart Guest Agent: %w", startup.Err())
		case <-tick.C:
		}
	}
}

func readTartReadiness(output io.Reader, ready chan struct{}) {
	reader := bufio.NewReaderSize(output, 4096)
	var truncated bool
	for {
		line, prefix, err := reader.ReadLine()
		if !truncated && !prefix && ready != nil && runnerReadyLine.Match(line) {
			close(ready)
			ready = nil
		}
		truncated = prefix
		if err != nil {
			return
		}
	}
}

func (t *Tart) lookup(ctx context.Context, i Instance) (*tartVM, error) {
	if err := t.owns(i); err != nil {
		return nil, err
	}
	entries, err := t.list(ctx, "local")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name == i.Resource {
			return &entry, nil
		}
	}
	return nil, nil
}

func (t *Tart) Stop(ctx context.Context, i Instance) error {
	entry, err := t.lookup(ctx, i)
	if err != nil || entry == nil || !entry.Running {
		return err
	}
	stopErr := t.command(ctx, "stop", "--timeout", "3", i.Resource).Run()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		entry, err = t.lookup(ctx, i)
		if err != nil {
			return errors.Join(stopErr, err)
		}
		if entry == nil || !entry.Running {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(stopErr, ctx.Err())
		case <-tick.C:
		}
	}
}

func (t *Tart) Delete(ctx context.Context, i Instance) error {
	entry, err := t.lookup(ctx, i)
	if err != nil || entry == nil {
		return err
	}
	if entry.Running {
		return errors.New("cannot delete a running Tart VM")
	}
	deleteErr := t.command(ctx, "delete", i.Resource).Run()
	entry, err = t.lookup(ctx, i)
	if err == nil && entry == nil {
		return nil
	}
	return errors.Join(deleteErr, err, errors.New("Tart clone still present or could not be checked"))
}

// ValidateCandidate exercises the guest agent and Runner provisioning without a JIT credential.
func (t *Tart) ValidateCandidate(ctx context.Context, i Instance) (result error) {
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		result = errors.Join(result, t.Stop(cleanup, i), t.Delete(cleanup, i))
	}()
	if err := t.Prepare(ctx, i); err != nil {
		return err
	}
	return t.provision(ctx, i, "")
}

func (t *Tart) provision(ctx context.Context, i Instance, toolchain string) error {
	vmExit, err := t.startVM(i.Resource)
	if err != nil {
		return err
	}
	if err := t.waitGuestAgent(ctx, i.Resource, vmExit); err != nil {
		return err
	}
	if t.diskGiB > 0 {
		if err := t.verifyGuestDisk(ctx, i.Resource); err != nil {
			return err
		}
	}
	if err := t.command(ctx, "exec", i.Resource, "/usr/bin/sudo", "-n", "/bin/sh", "-c", tartProvision, "ephemera-provision", i.RunnerVersion, i.RunnerSHA256).Run(); err != nil {
		return err
	}
	output, err := t.command(ctx, "exec", i.Resource, "/usr/bin/sudo", "-n", "-u", "ephemera-runner", "/opt/ephemera-runner/bin/Runner.Listener", "--version").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != i.RunnerVersion {
		return errors.New("prepared macOS Runner version does not match the approved recipe")
	}
	if toolchain != "" {
		return t.verifyAppleToolchain(ctx, i.Resource, toolchain)
	}
	return nil
}

func (t *Tart) verifyAppleToolchain(ctx context.Context, resource, toolchain string) error {
	const check = `
set -e
/usr/bin/xcodebuild -version
/usr/bin/xcrun --sdk macosx --show-sdk-path
if [ "$1" = ios ]; then
  /usr/bin/xcrun --sdk iphoneos --show-sdk-path
fi
`
	cmd := t.command(ctx, "exec", resource, "/usr/bin/sudo", "-n", "-u", "ephemera-runner", "/bin/sh", "-c", check, "verify-xcode", toolchain)
	var diagnostic boundedVMOutput
	cmd.Stdout, cmd.Stderr = &diagnostic, &diagnostic
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("verify %s toolchain in Tart VM: use an image with full Xcode and the required SDK selected: %w", toolchain, tartDiagnostic(err, diagnostic.output.String()))
	}
	return nil
}

func (t *Tart) RemoveCachedImage(ctx context.Context, ref string) error {
	id, ok := strings.CutPrefix(ref, "ephemera-base-")
	parsed, err := uuid.Parse(id)
	if !ok || err != nil || parsed.String() != id {
		return errors.New("refusing to remove unmanaged Tart image")
	}
	entries, err := t.list(ctx, "local")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == ref {
			if entry.Running {
				return errors.New("cannot remove running Tart base")
			}
			return t.command(ctx, "delete", ref).Run()
		}
	}
	return nil
}

// RemoveCachedSource drops only an exact pinned OCI reference from Ephemera's
// private Tart cache. A locally built source remains available to its owner.
func (t *Tart) RemoveCachedSource(ctx context.Context, ref string) error {
	if strings.HasPrefix(ref, "local:") {
		return nil
	}
	if !pinnedTartSource.MatchString(ref) {
		return errors.New("refusing to remove an unpinned Tart source")
	}
	entries, err := t.list(ctx, "oci")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == ref {
			if entry.Running {
				return errors.New("cannot remove running Tart source")
			}
			return t.command(ctx, "delete", ref).Run()
		}
	}
	return nil
}

// verifyGuestDisk grows the main APFS container in the disposable preparation
// clone. APFS recovery and boot partitions are preserved.
func (t *Tart) verifyGuestDisk(ctx context.Context, resource string) error {
	const check = `
set -eu
target_kib=$1
volume=/System/Volumes/Data
size() { /bin/df -kP "$volume" | /usr/bin/awk 'NR == 2 { print $2 }'; }
if [ "$(size)" -lt "$target_kib" ]; then
  container=$(/usr/sbin/diskutil info -plist "$volume" |
    /usr/bin/plutil -extract APFSContainerReference raw -o - -)
  /usr/sbin/diskutil apfs resizeContainer "$container" 0
fi
[ "$(size)" -ge "$target_kib" ]
`
	// Allow for boot and recovery partitions while verifying the usable volume.
	minimumKiB := uint64(t.diskGiB) * (1 << 20) * 9 / 10
	cmd := t.command(ctx, "exec", resource, "/usr/bin/sudo", "-n", "/bin/sh", "-c",
		check, "ephemera-resize", strconv.FormatUint(minimumKiB, 10))
	var diagnostic boundedVMOutput
	cmd.Stdout, cmd.Stderr = &diagnostic, &diagnostic
	err := cmd.Run()
	if err := tartDiagnostic(err, diagnostic.output.String()); err != nil {
		return fmt.Errorf("verify the %d GiB Tart guest disk: %w; use a compatible pre-sized image if APFS cannot expand", t.diskGiB, err)
	}
	return nil
}

// StoragePath identifies Tart’s private home directory.
func (t *Tart) StoragePath() string { return t.home }
