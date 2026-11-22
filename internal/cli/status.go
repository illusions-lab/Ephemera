package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/host"
	runtimeStatus "github.com/illusions-lab/Ephemera/internal/status"
)

type statusReport struct {
	Service           host.ServiceStatus      `json:"service"`
	Daemon            *runtimeStatus.Snapshot `json:"daemon"`
	Configuration     configState             `json:"configuration"`
	DaemonUnavailable string                  `json:"daemon_unavailable,omitempty"`
}

type configState struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

func (s configState) description() string {
	switch s.State {
	case "applied":
		return "Applied"
	case "pending":
		return "Unapplied changes"
	default:
		return "Unknown (" + s.Detail + ")"
	}
}

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "write JSON, one object per update in watch mode")
	watch := flags.Bool("watch", false, "refresh every two seconds")
	socket := flags.String("socket", host.StatusSocket(), "daemon status socket")
	path := flags.String("config", "", "saved configuration to compare with the daemon")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *socket == "" {
		return errors.New("usage: ephe status [--watch] [--json] [--socket PATH] [--config PATH]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var ticks <-chan time.Time
	if *watch {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	return streamStatus(ctx, os.Stdout, *jsonOutput, ticks, func(ctx context.Context) statusReport {
		return collectStatus(ctx, *socket, *path)
	})
}

func streamStatus(ctx context.Context, output io.Writer, jsonOutput bool, ticks <-chan time.Time, collect func(context.Context) statusReport) error {
	encoder := json.NewEncoder(output)
	for ctx.Err() == nil {
		report := collect(ctx)
		if ctx.Err() != nil {
			break
		}
		if jsonOutput {
			if err := encoder.Encode(report); err != nil {
				return err
			}
		} else {
			if ticks != nil {
				fmt.Fprintln(output, time.Now().Format(time.RFC3339))
			}
			printStatus(output, report)
			if ticks != nil {
				fmt.Fprintln(output)
			}
		}
		if ticks == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return nil
			}
		}
	}
	return nil
}

func collectStatus(ctx context.Context, socket string, paths ...string) statusReport {
	report := statusReport{Service: host.QueryService(ctx)}
	snapshot, err := runtimeStatus.Read(ctx, socket)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		report.DaemonUnavailable = "status socket is offline"
	case errors.Is(err, os.ErrPermission):
		report.DaemonUnavailable = "permission denied accessing status socket"
	case err != nil:
		report.DaemonUnavailable = "status endpoint did not return a valid response"
	default:
		report.Daemon = &snapshot
	}
	path := ""
	if len(paths) != 0 {
		path = paths[0]
	}
	if path == "" && socket == host.StatusSocket() {
		path = host.ConfigPath()
	}
	report.Configuration = compareConfiguration(path, report.Daemon)
	return report
}

func compareConfiguration(path string, daemon *runtimeStatus.Snapshot) configState {
	switch {
	case daemon == nil:
		return configState{"unknown", "daemon is unavailable"}
	case daemon.ConfigDigest == "":
		return configState{"unknown", "daemon does not report its configuration"}
	case path == "":
		return configState{"unknown", "specify --config for a custom socket"}
	}
	cfg, err := config.Load(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return configState{"unknown", "saved configuration is missing"}
	case errors.Is(err, os.ErrPermission):
		return configState{"unknown", "saved configuration cannot be read"}
	case err != nil:
		return configState{"unknown", "saved configuration is invalid or unreadable"}
	}
	digest, err := config.Digest(cfg)
	if err != nil {
		return configState{"unknown", "saved configuration cannot be compared"}
	}
	if digest == daemon.ConfigDigest {
		return configState{State: "applied"}
	}
	return configState{State: "pending"}
}

func printStatus(w io.Writer, report statusReport) {
	fmt.Fprintf(w, "%-13s %s", "Service", report.Service.State)
	if report.Service.Substate != "" && report.Service.Substate != report.Service.State {
		fmt.Fprintf(w, " (%s)", report.Service.Substate)
	}
	if report.Service.Result != "" && report.Service.Result != "success" {
		fmt.Fprintf(w, " · %s", report.Service.Result)
	}
	fmt.Fprintln(w)
	if report.Configuration.State != "" {
		fmt.Fprintf(w, "%-13s %s\n", "Configuration", report.Configuration.description())
	}
	if report.Daemon == nil {
		fmt.Fprintf(w, "%-13s unavailable (%s)\n", "Daemon", report.DaemonUnavailable)
		if report.Service.Message != "" {
			fmt.Fprintf(w, "%-13s %s\n", "Last status", report.Service.Message)
		}
		return
	}
	s := report.Daemon
	fmt.Fprintf(w, "%-13s %s", "Daemon", s.Phase)
	if s.FailedPhase != "" {
		fmt.Fprintf(w, " during %s", s.FailedPhase)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-13s %s\n", "GitHub", s.GitHub)
	if s.Scope != "" {
		fmt.Fprintf(w, "%-13s %s\n", "Target", s.Scope)
		fmt.Fprintf(w, "%-13s %q\n", "Scale Set", s.ScaleSet)
	}
	if s.RunnerGroup != "" {
		fmt.Fprintf(w, "%-13s %s\n", "Runner group", s.RunnerGroup)
	}
	if s.Labels != nil {
		labels := "(none)"
		if len(*s.Labels) > 0 {
			labels = strings.Join(*s.Labels, ", ")
		}
		fmt.Fprintf(w, "%-13s %s\n", "Labels", labels)
	}
	fmt.Fprintf(w, "%-13s %s\n", "Job intake", s.Intake)
	if update := s.ImageUpdate; update != nil {
		fmt.Fprintf(w, "%-13s %s (automatic: %t)\n", "Runner image", update.Current, update.Enabled)
		if update.Previous != "" {
			fmt.Fprintf(w, "%-13s %s\n", "Previous image", update.Previous)
		}
		if !update.LastCheck.IsZero() {
			fmt.Fprintf(w, "%-13s %s\n", "Image check", update.LastCheck.Format(time.RFC3339))
		}
		if update.RolledBack {
			fmt.Fprintf(w, "Runner image rolled back (%s)\n", update.RollbackReason)
		}
	}
	if s.Backend != "" {
		fmt.Fprintf(w, "%-13s %s\n", "VM backend", s.Backend)
	}
	if i := s.Instances; i != nil {
		fmt.Fprintf(w, "%-13s %d starting · %d waiting · %d running · %d pending cleanup\n",
			"VM instances", i.Starting, i.Waiting, i.Running, i.Reclaiming)
		if i.Draining {
			fmt.Fprintln(w, "Service is draining")
		}
		if i.CapacityReason != "" {
			fmt.Fprintf(w, "Capacity limited: %s\n", i.CapacityReason)
		}
		if i.PendingDeregistration > 0 {
			fmt.Fprintf(w, "Pending GitHub deregistration: %d\n", i.PendingDeregistration)
		}
	}
	if s.MaxConcurrent > 0 {
		fmt.Fprintf(w, "%-13s %d configured concurrent runners\n", "Capacity", s.MaxConcurrent)
	}
	if s.Demand != nil {
		fmt.Fprintf(w, "%-13s %d available · %d assigned · %d running · %d target runners\n",
			"GitHub jobs", s.Demand.Available, s.Demand.Assigned, s.Demand.Running, s.Demand.TargetRunners)
		fmt.Fprintf(w, "%-13s %s\n", "Demand data", s.Demand.UpdatedAt.Format(time.RFC3339))
	}
}
