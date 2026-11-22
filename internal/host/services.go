package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/macos"
)

type ServiceStatus struct {
	State    string `json:"state"`
	Substate string `json:"substate,omitempty"`
	Result   string `json:"result,omitempty"`
	Message  string `json:"message,omitempty"`
}

func QueryService(ctx context.Context) ServiceStatus {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), macos.Label)).Output()
		if err != nil {
			return ServiceStatus{State: "unknown"}
		}
		for _, line := range strings.Split(string(out), "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
				if value == "running" {
					return ServiceStatus{State: "active", Substate: value}
				}
				return ServiceStatus{State: "inactive", Substate: value}
			}
		}
		return ServiceStatus{State: "unknown"}
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "show", "--no-pager",
		"--property=LoadState,ActiveState,SubState,Result,StatusText", "ephemera.service").Output()
	state := ServiceStatus{State: "unknown"}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "LoadState":
			if value == "not-found" {
				return ServiceStatus{State: "not-installed"}
			}
		case "ActiveState":
			if value != "" {
				state.State = value
			}
		case "SubState":
			state.Substate = value
		case "Result":
			state.Result = value
		case "StatusText":
			state.Message = value
		}
	}
	return state
}
