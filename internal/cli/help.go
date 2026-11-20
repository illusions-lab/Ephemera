package cli

import "fmt"

const usage = "usage: ephe <setup | config validate | image prepare | version | update | help>"

func printHelp() {
	fmt.Println(usage)
	fmt.Println()
	fmt.Println("  version          Show version and commit")
	fmt.Println("  update           Check, stage, or apply an update")
	fmt.Println("  setup            Open the configuration wizard")
	fmt.Println("  config validate  Validate the saved configuration")
	fmt.Println("  image prepare    Build or verify the configured runner image")
	fmt.Println("  help             Show this help")
	fmt.Println()
	fmt.Println("Use --help after a command to see its options.")
}
