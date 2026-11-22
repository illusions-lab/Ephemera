//go:build darwin

package macos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/control"
	"github.com/illusions-lab/Ephemera/internal/status"
)

const launchctlFixture = `#!/bin/bash
set -eu
case "$1:$2" in
  print:gui/*/com.illusions-lab.ephemera.dev) exit 1 ;;
  print:gui/*/com.illusions-lab.ephemera)
    if [[ -f "$EPHE_ACTIVATION_TEST_ROOT/stopping" ]]; then
      if [[ -f "$EPHE_ACTIVATION_TEST_ROOT/stop-seen" ]]; then
        rm "$EPHE_ACTIVATION_TEST_ROOT/"{loaded,stopping,stop-seen}
      else
        touch "$EPHE_ACTIVATION_TEST_ROOT/stop-seen"
      fi
    fi
    test -f "$EPHE_ACTIVATION_TEST_ROOT/loaded"
    ;;
  print:*) exit 0 ;;
  bootout:*) touch "$EPHE_ACTIVATION_TEST_ROOT/stopping" ;;
  bootstrap:*)
    test ! -e "$EPHE_ACTIVATION_TEST_ROOT/loaded"
    daemon=$(/usr/bin/plutil -extract ProgramArguments.0 raw "$3")
    test ! -e "$(dirname "$daemon")/fail-bootstrap"
    printf '%s\n' "$daemon" > "$EPHE_ACTIVATION_TEST_ROOT/loaded"
    ;;
  *) exit 1 ;;
esac
`

func testActivation(t *testing.T) activation {
	t.Helper()
	// Darwin limits Unix socket paths to 103 bytes. Keep the fixture path
	// short while still covering plist arguments containing spaces.
	box, err := os.MkdirTemp("/tmp", "ephe-activate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(box) })
	bin := filepath.Join(box, "bin")
	a := activation{root: filepath.Join(box, "user data"), plist: filepath.Join(box, "LaunchAgents", Label+".plist"), domain: "gui/999", wait: 700 * time.Millisecond}
	a.logFile = filepath.Join(box, "Logs", "Ephemera", "daemon.log")
	for _, dir := range []string{bin, filepath.Join(a.root, "state")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeActivationFile(t, filepath.Join(bin, "launchctl"), launchctlFixture, 0700)
	t.Setenv("EPHE_ACTIVATION_TEST_ROOT", a.root)
	t.Setenv("EPHE_ACTIVATION_TEST_PLIST", a.plist)
	t.Setenv("EPHE_ACTIVATION_TEST_LOG", a.logFile)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return a
}

func writeActivationFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func readActivationFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readPlist(t *testing.T, path, field string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/plutil", "-extract", field, "raw", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read %s: %v: %s", field, err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}

func activateCandidate(c *Candidate) func() (*Candidate, error) {
	return func() (*Candidate, error) { return c, nil }
}

func testCandidate(config string) *Candidate {
	return &Candidate{
		Config: []byte(config + "\n"), Key: []byte("key for " + config),
		Scope: "https://github.com/test/project", ScaleSet: "mac-test", Intake: "executing",
	}
}

func readySnapshot(id string, c *Candidate) status.Snapshot {
	return status.Snapshot{
		ActivationID: id, Scope: c.Scope, ScaleSet: c.ScaleSet, Intake: c.Intake,
		Phase: "Healthy", GitHub: "connected",
	}
}

func fixtureStatus(t *testing.T, a activation, snapshot func() status.Snapshot) {
	t.Helper()
	closeStatus, err := status.Listen(filepath.Join(a.root, "state", "status.sock"), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeStatus() })
	closeControl, err := control.Listen(context.Background(), filepath.Join(a.root, "state"), func(ctx context.Context, req <-chan control.Request, reply chan<- control.Snapshot) {
		select {
		case <-ctx.Done():
			return
		case <-req:
		}
		select {
		case <-ctx.Done():
			return
		case reply <- control.Snapshot{Ready: true}:
		}
		<-ctx.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeControl)

}

func plistActivationID(path string) string {
	out, err := exec.Command("/usr/bin/plutil", "-extract", "ProgramArguments.7", "raw", path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(out), "\n")
}

func assertCurrent(t *testing.T, a activation, release string) {
	t.Helper()
	current, err := os.Readlink(filepath.Join(a.root, "current"))
	if err != nil || current != release {
		t.Fatalf("current = %q, %v, want %q", current, err, release)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists or cannot be checked: %v", path, err)
	}
}

func TestActivationInstallsAndAppliesConfig(t *testing.T) {
	a := testActivation(t)
	ctx := context.Background()
	release := filepath.Join(a.root, "release A")
	source := filepath.Join(a.root, "config.yaml")
	first, second := testCandidate("first"), testCandidate("edited")
	fixtureStatus(t, a, func() status.Snapshot { return readySnapshot(plistActivationID(a.plist), first) })

	for _, candidate := range []*Candidate{first, second} {
		writeActivationFile(t, source, string(candidate.Config), 0600)
		if err := a.run(ctx, release, activateCandidate(candidate)); err != nil {
			t.Fatal(err)
		}
		assertCurrent(t, a, release)
		if out, err := exec.Command("/usr/bin/plutil", "-lint", a.plist).CombinedOutput(); err != nil {
			t.Fatalf("invalid LaunchAgent: %v: %s", err, out)
		}
		if raw := readActivationFile(t, a.plist); !strings.Contains(raw, "<!DOCTYPE plist PUBLIC") || !strings.Contains(raw, "<true/>") {
			t.Fatal("LaunchAgent was not written in canonical Apple XML plist format")
		}
		if got := readPlist(t, a.plist, "ProgramArguments.0"); got != filepath.Join(release, "ephemerad") {
			t.Fatalf("daemon path = %q", got)
		}
		if flag, path := readPlist(t, a.plist, "ProgramArguments.8"), readPlist(t, a.plist, "ProgramArguments.9"); flag != "--log-file" || path != a.logFile {
			t.Fatalf("log arguments = %q %q", flag, path)
		}
		config := readPlist(t, a.plist, "ProgramArguments.3")
		if config == source || readActivationFile(t, config) != string(candidate.Config) {
			t.Fatalf("config snapshot %q does not contain this activation's configuration", config)
		}
		key := readPlist(t, a.plist, "ProgramArguments.5")
		if readActivationFile(t, key) != string(candidate.Key) {
			t.Fatal("activation did not retain its own credential")
		}
		assertMissing(t, filepath.Join(a.root, ".activation-transaction.json"))
	}
}

func TestActivationRestoresLoadedAndUnloadedServices(t *testing.T) {
	for _, mode := range []string{"bootstrap failure", "stale readiness", "wrong scope", "wrong intake", "canceled", "previously unloaded"} {
		t.Run(mode, func(t *testing.T) {
			a := testActivation(t)
			ctx := context.Background()
			first, second := testCandidate("first"), testCandidate("edited")
			oldRelease, newRelease := filepath.Join(a.root, "version A"), filepath.Join(a.root, "version B")
			statusModeFile := filepath.Join(a.root, "status-mode")
			staleIDFile := filepath.Join(a.root, "stale-id")
			fixtureStatus(t, a, func() status.Snapshot {
				id := plistActivationID(a.plist)
				mode, _ := os.ReadFile(statusModeFile)
				s := readySnapshot(id, first)
				switch string(mode) {
				case "stale readiness", "previously unloaded", "canceled":
					old, _ := os.ReadFile(staleIDFile)
					s.ActivationID = string(old)
				case "wrong scope":
					s.Scope = "https://github.com/test/other"
				case "wrong intake":
					s.Intake = "observing"
				}
				return s
			})
			if err := a.run(ctx, oldRelease, activateCandidate(first)); err != nil {
				t.Fatal(err)
			}
			oldPlist := readActivationFile(t, a.plist)
			oldConfig := readPlist(t, a.plist, "ProgramArguments.3")
			writeActivationFile(t, staleIDFile, plistActivationID(a.plist), 0600)
			writeActivationFile(t, statusModeFile, mode, 0600)
			userConfig := filepath.Join(a.root, "config.yaml")
			writeActivationFile(t, userConfig, string(second.Config), 0600)
			loaded := filepath.Join(a.root, "loaded")
			if mode == "bootstrap failure" {
				if err := os.Mkdir(newRelease, 0700); err != nil {
					t.Fatal(err)
				}
				writeActivationFile(t, filepath.Join(newRelease, "fail-bootstrap"), "", 0600)
			}
			if mode == "previously unloaded" {
				if err := os.Remove(loaded); err != nil {
					t.Fatal(err)
				}
			}
			activateCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "canceled" {
				go func() {
					watchCtx, stop := context.WithTimeout(ctx, 2*time.Second)
					defer stop()
					_ = waitForFile(watchCtx, loaded, filepath.Join(newRelease, "ephemerad")+"\n")
					cancel()
				}()
			}
			err := a.run(activateCtx, newRelease, activateCandidate(second))
			if err == nil {
				t.Fatal("activation accepted a failed or unrelated candidate")
			}
			switch mode {
			case "bootstrap failure":
				if !strings.Contains(err.Error(), "launchctl bootstrap") {
					t.Fatal(err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			default:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			}
			assertCurrent(t, a, oldRelease)
			if readActivationFile(t, a.plist) != oldPlist || readActivationFile(t, oldConfig) != string(first.Config) ||
				readActivationFile(t, userConfig) != string(second.Config) {
				t.Fatal("rollback did not preserve both the active snapshot and editable config")
			}
			if mode == "previously unloaded" {
				assertMissing(t, loaded)
			} else if readActivationFile(t, loaded) != filepath.Join(oldRelease, "ephemerad")+"\n" {
				t.Fatal("rollback did not reload the previous daemon")
			}
			assertMissing(t, filepath.Join(a.root, ".activation-transaction.json"))
		})
	}
}

func TestActivationRetainsJournalWhenRecoveryFails(t *testing.T) {
	a := testActivation(t)
	ctx := context.Background()
	first := testCandidate("first")
	oldRelease, newRelease := filepath.Join(a.root, "first"), filepath.Join(a.root, "rejected")
	fixtureStatus(t, a, func() status.Snapshot {
		return readySnapshot(plistActivationID(a.plist), first)
	})
	if err := a.run(ctx, oldRelease, activateCandidate(first)); err != nil {
		t.Fatal(err)
	}
	oldPlist := readActivationFile(t, a.plist)
	for _, release := range []string{oldRelease, newRelease} {
		if err := os.Mkdir(release, 0700); err != nil {
			t.Fatal(err)
		}
		writeActivationFile(t, filepath.Join(release, "fail-bootstrap"), "", 0600)
	}
	journal := filepath.Join(a.root, ".activation-transaction.json")
	err := a.run(ctx, newRelease, activateCandidate(testCandidate("candidate")))
	if err == nil || !strings.Contains(err.Error(), journal) {
		t.Fatalf("expected rollback error identifying %s, got %v", journal, err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(oldRelease, "fail-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := a.run(ctx, oldRelease, func() (*Candidate, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, a, oldRelease)
	assertMissing(t, journal)
	if readActivationFile(t, a.plist) != oldPlist || readActivationFile(t, filepath.Join(a.root, "loaded")) != filepath.Join(oldRelease, "ephemerad")+"\n" {
		t.Fatal("next activation did not finish recovering the previous service")
	}
}

func TestActivationRecoversAfterKill(t *testing.T) {
	a := testActivation(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first := testCandidate("first")
	oldRelease, newRelease := filepath.Join(a.root, "first"), filepath.Join(a.root, "interrupted")
	fixtureStatus(t, a, func() status.Snapshot {
		return readySnapshot(plistActivationID(a.plist), first)
	})
	if err := a.run(ctx, oldRelease, activateCandidate(first)); err != nil {
		t.Fatal(err)
	}
	oldPlist := readActivationFile(t, a.plist)
	journal := filepath.Join(a.root, ".activation-transaction.json")

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestActivationInterruptedChild$")
	cmd.Env = append(os.Environ(), "EPHE_ACTIVATION_TEST_RELEASE="+newRelease)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	loaded := filepath.Join(a.root, "loaded")
	if err := waitForFile(ctx, loaded, filepath.Join(newRelease, "ephemerad")+"\n"); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, a, oldRelease)
	called := false
	err := a.run(ctx, oldRelease, func() (*Candidate, error) {
		called = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "lock macOS activation") || called {
		t.Fatalf("concurrent activation reached preparation or reported the wrong error: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("expected killed child: %s", &output)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal(err)
	}
	if err := a.run(ctx, oldRelease, func() (*Candidate, error) {
		if readActivationFile(t, a.plist) != oldPlist || readActivationFile(t, loaded) != filepath.Join(oldRelease, "ephemerad")+"\n" {
			t.Fatal("interrupted transaction had not recovered before preparation")
		}
		assertMissing(t, journal)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, a, oldRelease)
}

func TestActivationInterruptedChild(t *testing.T) {
	release := os.Getenv("EPHE_ACTIVATION_TEST_RELEASE")
	if release == "" {
		t.Skip("subprocess fixture")
	}
	a := activation{root: os.Getenv("EPHE_ACTIVATION_TEST_ROOT"), plist: os.Getenv("EPHE_ACTIVATION_TEST_PLIST"), logFile: os.Getenv("EPHE_ACTIVATION_TEST_LOG"), domain: "gui/999", wait: 30 * time.Second}
	candidate := testCandidate("interrupted")
	candidate.ScaleSet = "held-unhealthy"
	if err := a.run(context.Background(), release, activateCandidate(candidate)); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fixture unexpectedly committed")
}

func waitForFile(ctx context.Context, path, content string) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == content {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", path, ctx.Err())
		case <-tick.C:
		}
	}
}
