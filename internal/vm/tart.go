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

	"github.com/illusions-lab/Ephemera/internal/macos"
)

var tartName = regexp.MustCompile(`^ephemera-[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

type Tart struct {
	binary, home string
	cpu, memory  int
}

func NewTart(home string, cpu, memory int) (*Tart, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || os.Geteuid() == 0 {
		return nil, errors.New("Tart requires Apple Silicon macOS and a non-root service account")
	}
	binary, err := findTart()
	if err != nil {
		return nil, err
	}
	return &Tart{binary: binary, home: home, cpu: cpu, memory: memory}, nil
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
	cmd.Env = append(cmd.Environ(), "TART_HOME="+t.home)
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
	if err := t.command(ctx, "clone", i.Image, i.Resource).Run(); err != nil {
		return fmt.Errorf("clone Tart image: %w", err)
	}
	return t.command(ctx, "set", i.Resource, "--cpu", strconv.Itoa(t.cpu), "--memory", strconv.Itoa(t.memory)).Run()
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
	if err := run.Start(); err != nil {
		return nil, err
	}
	vmExit := make(chan error, 1)
	go func() { vmExit <- run.Wait() }()
	return vmExit, nil
}

func (t *Tart) runGuest(startup context.Context, i Instance, jit string, ready chan struct{}, vmExit <-chan error) error {
	if err := t.waitGuestAgent(startup, i.Resource, vmExit); err != nil {
		return err
	}
	if err := t.command(startup, "exec", i.Resource, "/usr/bin/sudo", "-n", "/bin/sh", "-c", tartProvision).Run(); err != nil {
		return fmt.Errorf("prepare macOS runner: %w", err)
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
