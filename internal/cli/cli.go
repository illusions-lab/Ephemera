package cli

import (
	"errors"
	"fmt"
	"github.com/illusions-lab/Ephemera/internal/version"
	"os"
	"os/exec"
)

func Run(args []string) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")):
		printHelp()
		return nil
	case len(args) == 1 && args[0] == "version":
		fmt.Println(version.String())
		return nil
	case len(args) >= 1 && args[0] == "update":
		for _, arg := range args[1:] {
			if arg != "--check" && arg != "--apply" {
				return errors.New("usage: ephe update [--check | --apply]")
			}
		}
		if len(args) > 2 {
			return errors.New("usage: ephe update [--check | --apply]")
		}
		cmd := exec.Command("/opt/ephemera/current/install.sh", append([]string{"--update"}, args[1:]...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		return cmd.Run()
	case args[0] == "status":
		return runStatus(args[1:])
	case args[0] == "doctor":
		return runDoctor(args[1:])
	case len(args) >= 1 && args[0] == "setup":
		return runSetup(args[1:])
	case len(args) >= 2 && args[0] == "image" && args[1] == "prepare":
		return prepareImage(args[2:])
	case len(args) >= 2 && args[0] == "config" && args[1] == "validate":
		return validateConfig(args[2:])
	default:
		return errors.New(usage)
	}
}
