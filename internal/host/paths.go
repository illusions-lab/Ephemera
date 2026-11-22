// Package host holds platform policy for paths, credentials and native services.
package host

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/illusions-lab/Ephemera/internal/macos"
	"github.com/illusions-lab/Ephemera/internal/status"
)

const LinuxConfigDir = "/etc/ephemera"

func ConfigDir() string {
	if runtime.GOOS == "darwin" {
		return macos.Root()
	}
	return LinuxConfigDir
}

func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.yaml")
}

func KeyPath() string {
	return filepath.Join(ConfigDir(), "credentials", "github-app.pem")
}

func DaemonKeyPath() string {
	if runtime.GOOS != "darwin" {
		if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			return filepath.Join(dir, "github-app-key")
		}
	}
	return KeyPath()
}

func ImageCache() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(macos.Root(), "image")
	}
	return "/var/lib/ephemera/image"
}

func StateDir() string {
	if runtime.GOOS == "darwin" {
		return macos.StateDir()
	}
	return "/var/lib/ephemera/instances"
}

func StatusSocket() string {
	if runtime.GOOS == "darwin" {
		return macos.Socket()
	}
	return status.SocketPath
}

func DaemonStatusSocket() string {
	if runtime.GOOS == "darwin" {
		return StatusSocket()
	}
	// systemd supplies the socket path to the installed Linux daemon.
	return ""
}

// PrivateStatusSocket identifies listeners whose socket lifecycle is managed
// while holding the instance directory lock.
func PrivateStatusSocket(path string) bool {
	return runtime.GOOS == "darwin" && path != ""
}

func ListenStatus(path, state string, snapshot func() status.Snapshot) (func() error, error) {
	if PrivateStatusSocket(path) {
		if err := status.RemoveStale(path, state); err != nil {
			return nil, err
		}
	}
	return status.Listen(path, snapshot)
}
