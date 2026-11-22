package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/macos"
)

func runService(args []string) error {
	if len(args) != 1 || (args[0] != "stop" && args[0] != "restart") {
		return errors.New("usage: ephe service stop | restart")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	invoke := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if runtime.GOOS != "darwin" {
		if os.Geteuid() != 0 {
			return errors.New("run the service command as root")
		}
		state := host.QueryService(ctx).State
		if state != "inactive" && state != "failed" && state != "not-installed" {
			if err := control.Drain(ctx, host.StateDir(), os.Stdin, os.Stderr, func() error {
				return invoke("systemctl", "stop", "ephemera.service")
			}); err != nil {
				return err
			}
		} else if err := invoke("systemctl", "stop", "ephemera.service"); err != nil {
			return err
		}
		if args[0] == "restart" {
			return invoke("systemctl", "start", "ephemera.service")
		}
		return nil
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/" + macos.Label
	if err := exec.CommandContext(ctx, "launchctl", "print", domain).Run(); err != nil {
		return err
	}
	if exec.CommandContext(ctx, "launchctl", "print", target).Run() == nil {
		if err := control.Drain(ctx, host.StateDir(), os.Stdin, os.Stderr, func() error {
			return macos.UnloadService(ctx, domain)
		}); err != nil {
			return err
		}
	}
	if args[0] == "restart" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		return invoke("launchctl", "bootstrap", domain, filepath.Join(home, "Library", "LaunchAgents", macos.Label+".plist"))
	}
	return ctx.Err()
}
