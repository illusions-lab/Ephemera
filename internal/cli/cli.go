package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/version"
)

func Run(args []string) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")):
		printHelp()
		return nil
	case len(args) == 1 && args[0] == "version":
		fmt.Println(version.String())
		return nil
	case len(args) == 2 && args[0] == "--recover-activation":
		return macos.RecoverStartup(context.Background(), args[1])
	case len(args) == 1 && args[0] == "tart-manifest":
		fmt.Print(macos.TartManifest())
		return nil
	case len(args) >= 1 && args[0] == "update":
		if runtime.GOOS == "darwin" {
			return errors.New("use install-macos.sh --version TAG to switch versions")
		}
		for _, arg := range args[1:] {
			if arg != "--check" && arg != "--apply" {
				return errors.New("usage: ephe update [--check | --apply]")
			}
		}
		if len(args) > 2 {
			return errors.New("usage: ephe update [--check | --apply]")
		}
		installer := "/opt/ephemera/current/install.sh"
		if len(args) == 2 && args[1] == "--apply" {
			if _, err := os.Stat("/opt/ephemera/staged/install.sh"); err == nil {
				installer = "/opt/ephemera/staged/install.sh"
			}
		}
		cmd := exec.Command(installer, append([]string{"--update"}, args[1:]...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		return cmd.Run()
	case args[0] == "service":
		return runService(args[1:])
	case args[0] == "ps":
		return runPS(args[1:])
	case args[0] == "status":
		return runStatus(args[1:])
	case args[0] == "doctor":
		return runDoctor(args[1:])
	case len(args) >= 1 && args[0] == "setup":
		return runSetup(args[1:])
	case len(args) >= 2 && args[0] == "image" && args[1] == "prepare":
		return prepareImage(args[2:])
	case len(args) >= 2 && args[0] == "image" && args[1] == "check-host-xcode":
		return checkHostXcode(args[2:])
	case args[0] == "config":
		return runConfig(args[1:])
	default:
		return errors.New(usage)
	}
}
