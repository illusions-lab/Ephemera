package version

import (
	"fmt"
	"runtime/debug"
)

var Version = "dev"
var Commit = ""

func String() string {
	commit := Commit
	if commit == "" {
		commit = "unknown"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" {
					commit = s.Value
				}
			}
		}
	}
	return fmt.Sprintf("%s %s", Version, commit)
}
