//go:build !linux

package setup

import "os/exec"

func isolateTunnelProcess(_ *exec.Cmd) error { return nil }
