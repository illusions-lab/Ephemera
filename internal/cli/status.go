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
	"syscall"
	"time"

	"github.com/illusions-lab/Ephemera/internal/host"
	runtimeStatus "github.com/illusions-lab/Ephemera/internal/status"
)

type statusReport struct {
	Service           host.ServiceStatus      `json:"service"`
	Daemon            *runtimeStatus.Snapshot `json:"daemon"`
	DaemonUnavailable string                  `json:"daemon_unavailable,omitempty"`
}

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "write JSON, one object per update in watch mode")
	watch := flags.Bool("watch", false, "refresh every two seconds")
	socket := flags.String("socket", host.StatusSocket(), "daemon status socket")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *socket == "" {
		return errors.New("usage: ephe status [--watch] [--json] [--socket PATH]")
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
		return collectStatus(ctx, *socket)
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

func collectStatus(ctx context.Context, socket string) statusReport {
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
	return report
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
