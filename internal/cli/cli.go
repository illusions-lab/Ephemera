package cli

import "errors"

func Run(args []string) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")):
		printHelp()
		return nil
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
