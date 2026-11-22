//go:build linux && (amd64 || arm64)

package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/qemuplatform"
	"golang.org/x/sys/unix"
)

type qemu struct {
	QEMUConfig
	binary   string
	platform qemuplatform.Platform
}

func NewQEMU(cfg QEMUConfig) (Backend, error) {
	platform, err := qemuplatform.ForArch(runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	return &qemu{QEMUConfig: cfg, platform: platform}, nil
}

func (q *qemu) Check(_ context.Context) error {
	if os.Geteuid() == 0 {
		return errors.New("run QEMU execution under an unprivileged service account")
	}
	// Finding the executable is only required for new VMs. Recovery
	// recognizes old processes by their recorded instance identity.
	binary, err := exec.LookPath(q.platform.Binary)
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	q.binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	for _, tool := range []string{"qemu-img", "cloud-localds"} {
		if _, err := exec.LookPath(tool); err != nil {
			return err
		}
	}
	if err := q.platform.CheckFirmware(); err != nil {
		return err
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("access KVM: %w", err)
	}
	kvm.Close()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err == nil {
		unix.Close(fd)
	}
	return err
}

func (q *qemu) directory(i Instance) string {
	return filepath.Join(q.Root, i.ID)
}

func (q *qemu) Prepare(ctx context.Context, i Instance) error {
	dir := q.directory(i)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2",
		"-b", i.Image, filepath.Join(dir, "disk.qcow2"))
	var stderr boundedVMOutput
	cmd.Stderr = &stderr
	return qemuDiagnostic(cmd.Run(), stderr.output.String())
}

func (q *qemu) Start(ctx context.Context, i Instance, jit string) (Process, error) {
	dir := q.directory(i)
	userData, metaData, seed := filepath.Join(dir, "user-data"), filepath.Join(dir, "meta-data"), filepath.Join(dir, "seed.img")
	defer os.Remove(userData)
	defer os.Remove(metaData)
	if err := os.WriteFile(userData, []byte(image.RunnerCloudConfig(jit)), 0600); err != nil {
		return Process{}, err
	}
	if err := os.WriteFile(metaData, []byte("instance-id: "+i.ID+"\n"), 0600); err != nil {
		return Process{}, err
	}
	seedCommand := exec.CommandContext(ctx, "cloud-localds", seed, userData, metaData)
	// cloud-localds copies user-data into a temporary directory. Keep that
	// copy under the same persistent identity so recovery can remove it.
	seedCommand.Env = append(seedCommand.Environ(), "TMPDIR="+dir)
	if err := seedCommand.Run(); err != nil {
		return Process{}, fmt.Errorf("create private guest seed: %w", err)
	}
	if err := os.Chmod(seed, 0600); err != nil {
		return Process{}, err
	}
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	ready := make(chan struct{})
	exited := make(chan error, 1)
	boot, err := q.platform.BootArgs(dir, true)
	if err != nil {
		return Process{}, err
	}
	args := []string{
		"-name", i.Resource, "-uuid", i.ID,
		"-m", strconv.Itoa(q.MemoryMiB), "-smp", strconv.Itoa(q.VCPUs),
		"-display", "none", "-monitor", "none",
		"-no-reboot",
		"-nic", "user,model=virtio-net-pci",
		"-drive", q.disk(i), "-drive", qemuplatform.FileOption(seed) + ",format=raw,if=virtio,readonly=on"}
	args = append(args, q.platform.StatusArgs("")...)
	cmd := exec.Command(q.binary, append(args, boot...)...)
	cmd.Stdout = &readiness{ready: ready}
	var stderr boundedVMOutput
	cmd.Stderr = &stderr
	// Keep the launching Linux thread alive until QEMU exits. The kernel
	// tracks this thread for the parent-death signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		started <- err
		if err == nil {
			waitErr := cmd.Wait()
			waitErr = qemuDiagnostic(waitErr, stderr.output.String())
			exited <- waitErr
			close(exited)
		}
	}()
	if err := <-started; err != nil {
		return Process{}, err
	}
	return Process{Ready: ready, Exited: exited}, nil
}

func (q *qemu) disk(i Instance) string {
	return qemuplatform.FileOption(filepath.Join(q.directory(i), "disk.qcow2")) + ",format=qcow2,if=virtio"
}

func qemuDiagnostic(err error, output string) error {
	if err == nil || strings.TrimSpace(output) == "" {
		return err
	}
	err = fmt.Errorf("%w: QEMU stderr: %s", err, strings.TrimSpace(output))
	diagnostic := strings.ToLower(output)
	for _, signature := range []string{"image is corrupt", "qcow2 header", "invalid backing file", "invalid qcow2", "could not open backing file"} {
		if strings.Contains(diagnostic, signature) {
			return errors.Join(ErrImageInfrastructure, err)
		}
	}
	return err
}

type readiness struct {
	ready    chan struct{}
	line     []byte
	overflow bool
}

func (r *readiness) Write(data []byte) (int, error) {
	for _, b := range data {
		if r.ready == nil {
			break
		}
		if b == '\n' {
			if !r.overflow && runnerReadyLine.Match(bytes.TrimSuffix(r.line, []byte("\r"))) {
				close(r.ready)
				r.ready = nil
			}
			r.line = r.line[:0]
			r.overflow = false
		} else if len(r.line) < 256 {
			r.line = append(r.line, b)
		} else {
			r.overflow = true
		}
	}
	return len(data), nil
}

func (q *qemu) Stop(ctx context.Context, i Instance) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		args, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || !q.matches(args, i) {
			continue
		}
		if err := q.stopProcess(ctx, pid, dir, i); err != nil {
			return err
		}
	}
	return nil
}

func (q *qemu) matches(args []byte, i Instance) bool {
	fields := strings.Split(string(args), "\x00")
	return len(fields) >= 7 && filepath.IsAbs(fields[0]) &&
		fields[1] == "-name" && fields[2] == i.Resource && fields[3] == "-uuid" &&
		fields[4] == i.ID && bytes.Contains(args, []byte("\x00-drive\x00"+q.disk(i)+"\x00"))
}

func (q *qemu) stopProcess(ctx context.Context, pid int, dir string, i Instance) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	args, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if errors.Is(err, os.ErrNotExist) || (err == nil && !q.matches(args, i)) {
		return nil
	}
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Stat(dir, &stat); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("QEMU process owner does not match daemon")
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSuffix(exe, " (deleted)") != strings.SplitN(string(args), "\x00", 2)[0] {
		return errors.New("QEMU executable identity does not match instance")
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for ctx.Err() == nil {
		n, err := unix.Poll(poll, 100)
		if n > 0 {
			return nil
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
	}
	return ctx.Err()
}

func (q *qemu) Delete(_ context.Context, i Instance) error {
	return os.RemoveAll(q.directory(i))
}
