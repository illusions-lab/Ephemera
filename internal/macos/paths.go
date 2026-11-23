package macos

import (
	"os"
	"path/filepath"
)

const Label = "com.illusions-lab.ephemera"
const GitHubImageRevision = "6ae22b7fe1e9897ad8220e69f154a603cbd4b0ab"
const ImageRef = "ghcr.io/cirruslabs/macos-tahoe-base@sha256:65c3133a633fda8753cf1f71f2d55727792d534a2125b0032466563dcc8e58fd"

func Root() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Ephemera")
}

func StateDir() string { return filepath.Join(Root(), "state") }
func Socket() string   { return filepath.Join(StateDir(), "status.sock") }
func Key() string      { return filepath.Join(Root(), "credentials", "github-app.pem") }
