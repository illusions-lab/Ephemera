package macos

import (
	_ "embed"
	"path/filepath"
	"strings"
)

//go:embed tart-manifest.tsv
var tartManifest string

var tartFields = strings.Split(strings.TrimSuffix(tartManifest, "\n"), "\t")
var TartVersion = tartFields[0]

func TartManifest() string { return tartManifest }

func TartBinary() string { return pinnedTartBinary("tart") }

// BundledTartBinary belongs to an offline installation with a user-supplied app.
func BundledTartBinary() string { return pinnedTartBinary("tart-prebuilt") }

func PrebuiltTartBinary() string {
	return filepath.Join(Root(), "tools", "tart.app", "Contents", "MacOS", "tart")
}

func pinnedTartBinary(directory string) string {
	return filepath.Join(Root(), "tools", directory, TartVersion+"-"+tartFields[2], "tart.app", "Contents", "MacOS", "tart")
}
