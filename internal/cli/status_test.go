package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/config"
	runtimeStatus "github.com/illusions-lab/Ephemera/internal/status"
)

func TestStatusReportsHostSupervisorWhenSocketOffline(t *testing.T) {
	dir, err := os.MkdirTemp("", "ephe-status-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	script := "#!/bin/sh\nprintf '%s\\n' LoadState=loaded ActiveState=activating SubState=auto-restart Result=exit-code 'StatusText=Degraded during Connecting'\n"
	program, state, substate, detail := "systemctl", "activating", "auto-restart", "Degraded during Connecting"
	if runtime.GOOS == "darwin" {
		program, state, substate, detail = "launchctl", "active", "running", "(running)"
		script = "#!/bin/sh\nprintf '%s\n' 'state = running'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	report := collectStatus(t.Context(), filepath.Join(dir, "offline.sock"))
	if report.Daemon != nil || report.Service.State != state || report.Service.Substate != substate {
		t.Fatalf("unexpected status report: %+v", report)
	}
	var output bytes.Buffer
	printStatus(&output, report)
	for _, expected := range []string{"status socket is offline", detail} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("expected %q in status output: %s", expected, &output)
		}
	}
	if strings.Contains(output.String(), "0 available") {
		t.Fatalf("offline status represented unavailable data as zero: %s", &output)
	}
}

func TestStatusWatchJSON(t *testing.T) {
	dir, err := os.MkdirTemp("", "ephe-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("PATH", dir) // The socket can be queried independently of systemd.
	socket := filepath.Join(dir, "status.sock")
	var closeSocket func() error
	defer func() {
		if closeSocket != nil {
			_ = closeSocket()
		}
	}()
	start := func(snapshot runtimeStatus.Snapshot) {
		t.Helper()
		var err error
		closeSocket, err = runtimeStatus.Listen(socket, func() runtimeStatus.Snapshot { return snapshot })
		if err != nil {
			t.Fatal(err)
		}
	}

	// Each refresh reads the real Unix socket. Buffered ticks let the test
	// advance the watch loop without depending on wall clock intervals.
	ticks := make(chan time.Time, 3)
	for range 3 {
		ticks <- time.Time{}
	}
	close(ticks)
	var output bytes.Buffer
	refresh := 0
	err = streamStatus(t.Context(), &output, true, ticks, func(ctx context.Context) statusReport {
		switch refresh {
		case 0:
			start(runtimeStatus.Snapshot{Phase: "Connecting"})
		case 1:
			_ = closeSocket()
			start(runtimeStatus.Snapshot{Phase: "Healthy", Demand: &runtimeStatus.Demand{}})
		case 2:
			_ = closeSocket()
			closeSocket = nil
		case 3:
			start(runtimeStatus.Snapshot{Phase: "Healthy", Demand: &runtimeStatus.Demand{Available: 3}})
		}
		refresh++
		return collectStatus(ctx, socket)
	})
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 || refresh != 4 {
		t.Fatalf("expected four refreshes and four JSON lines, got %d refreshes and %d lines", refresh, len(lines))
	}
	reports := make([]statusReport, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &reports[i]); err != nil {
			t.Fatalf("refresh %d did not return one complete JSON object: %v", i, err)
		}
	}
	if r := reports[0]; r.Daemon == nil || r.Daemon.Demand != nil || r.Daemon.Phase != "Connecting" {
		t.Fatalf("expected live daemon without job statistics: %+v", r)
	}
	if r := reports[1]; r.Daemon == nil || r.Daemon.Demand == nil || r.Daemon.Demand.Available != 0 {
		t.Fatalf("expected real zero demand: %+v", r)
	}
	if r := reports[2]; r.Daemon != nil || r.DaemonUnavailable != "status socket is offline" {
		t.Fatalf("expected offline fallback: %+v", r)
	}
	if r := reports[3]; r.Daemon == nil || r.Daemon.Demand == nil || r.Daemon.Demand.Available != 3 || r.DaemonUnavailable != "" {
		t.Fatalf("expected fresh demand after reconnecting: %+v", r)
	}
}

func TestStatusComparesSavedAndLoadedConfiguration(t *testing.T) {
	data, err := os.ReadFile("../../configs/ephemera.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(data)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := config.Digest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded := &runtimeStatus.Snapshot{ConfigDigest: digest}
	write(append([]byte("# edited comment\n"), data...))
	if got := compareConfiguration(path, loaded); got.State != "applied" {
		t.Fatalf("comment change marked configuration %v", got)
	}
	write(bytes.Replace(data, []byte("scale_set_name: "), []byte("scale_set_name: new-"), 1))
	if got := compareConfiguration(path, loaded); got.State != "pending" {
		t.Fatalf("changed Scale Set marked configuration %v", got)
	}
	write([]byte("invalid:"))
	if got := compareConfiguration(path, loaded); got.State != "unknown" || !strings.Contains(got.Detail, "invalid") {
		t.Fatalf("invalid draft marked configuration %v", got)
	}
	if got := compareConfiguration(path, &runtimeStatus.Snapshot{}); got.State != "unknown" || !strings.Contains(got.Detail, "daemon does not report") {
		t.Fatalf("legacy daemon marked configuration %v", got)
	}
}

func TestCapacityDistinguishesMemoryShortageFromConfiguredConcurrency(t *testing.T) {
	cfg := config.MacDefaults()
	cfg.VM.HostReserveMiB = 384
	var available uint64 = 4400
	current := runtimeStatus.Snapshot{
		ConfigDigest: "applied", MaxConcurrent: 1,
		Instances: &runtimeStatus.Instances{
			CapacityReason: "available memory after the host reserve",
			Capacity: &runtimeStatus.Capacity{
				Startable: 0, AvailableMemoryMiB: &available, ReserveMiB: 384, VMMemoryMiB: 4096,
			},
		},
	}
	var output bytes.Buffer
	if err := reportDoctorCapacity(cfg, current, "applied", 8192, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"0 startable VM slots / 1 configured", "4400 MiB available", "384 MiB host reserve", "4096 MiB per VM", "No VM can start currently"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q from %s", expected, &output)
		}
	}
	if err := reportDoctorCapacity(cfg, current, "applied", 4095, &output); err == nil || !strings.Contains(err.Error(), "requires 4096 MiB") {
		t.Fatalf("expected a physical capacity error, got %v", err)
	}
}
