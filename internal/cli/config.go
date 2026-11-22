package cli

import (
	"errors"
	"flag"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
)

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
