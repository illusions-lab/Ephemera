package main

import (
	"fmt"
	"os"

	"github.com/illusions-lab/Ephemera/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ephemera:", err)
		os.Exit(1)
	}
}
