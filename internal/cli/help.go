package cli

import "fmt"

const usage = "usage: ephe <service | status | ps | doctor | setup | config | image | version | update | help>"

func printHelp() {
	fmt.Println(usage)
	fmt.Println()
	fmt.Println("  version          Show version and commit")
	fmt.Println("  update           Check, stage, or apply an update")
	fmt.Println("  service stop | restart  Drain jobs and control the installed service")
	fmt.Println("  ps [-a] [-q]    List local runner VMs")
	fmt.Println("  status           Show service, GitHub, and job intake status")
	fmt.Println("  doctor           Check configuration, GitHub access, and VM prerequisites")
	fmt.Println("  setup            Open the configuration wizard")
	fmt.Println("  config           Edit the saved configuration interactively")
	fmt.Println("  config apply     Apply saved configuration after active jobs finish")
	fmt.Println("  config validate  Validate the saved configuration")
	fmt.Println("  image prepare    Build or verify the configured runner image")
	fmt.Println("  image check-host-xcode  Verify an Apple Xcode installation for Tart image builds")
	fmt.Println("  help             Show this help")
	fmt.Println()
	fmt.Println("Use --help after a command to see its options.")
}
