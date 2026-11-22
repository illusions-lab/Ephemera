package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/charmbracelet/x/term"
	"github.com/illusions-lab/Ephemera/internal/cli/setup"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
)

func runConfig(args []string) error {
	switch {
	case len(args) == 0:
		if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
			return errors.New("ephe config requires a terminal; use ephe config validate for scripts")
		}
		if runtime.GOOS == "linux" && os.Geteuid() != 0 {
			return errors.New("run sudo ephe config to edit the system configuration")
		}
		return setup.Edit(host.ConfigPath())
	case args[0] == "apply":
		return runConfigApply(args[1:])
	case args[0] == "--apply-step":
		return runLinuxConfigStep(args[1:])
	case args[0] == "validate":
		return validateConfig(args[1:])
	case len(args) == 1 && (args[0] == "--help" || args[0] == "-h"):
		fmt.Println("usage: ephe config | ephe config validate [--config PATH] | ephe config apply [--yes]")
		return nil
	default:
		return errors.New("usage: ephe config | ephe config validate [--config PATH] | ephe config apply [--yes]")
	}
}

func validateConfig(args []string) error {
	flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
	path := flags.String("config", host.ConfigPath(), "configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments to config validate")
	}
	_, err := config.Load(*path)
	return err
}
