package vm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTartReadinessRequiresRunnerSignal(t *testing.T) {
	ready := make(chan struct{})
	readTartReadiness(strings.NewReader("ready\n"+strings.Repeat("x", 8192)+"2026-11-22 00:00:00Z: Listening for Jobs\n"), ready)
	select {
	case <-ready:
		t.Fatal("accepted guest agent output or a truncated runner line")
	default:
	}
	readTartReadiness(strings.NewReader("2026-11-22 00:00:00Z: Listening for Jobs\n"), ready)
	select {
	case <-ready:
	default:
		t.Fatal("did not recognize runner listening")
	}
}

func TestTartGuestLaunchContract(t *testing.T) {
	for _, test := range []struct {
		name  string
		ready bool
	}{
		{name: "listening marks ready", ready: true},

		{name: "runner exits before listening"},
	} {
		t.Run(test.name, func(t *testing.T) {
			verifyTartGuestLaunch(t, test.name, test.ready)
		})
	}
}

func verifyTartGuestLaunch(t *testing.T, scenario string, wantReady bool) {
	t.Helper()
	t.Setenv("EPHE_TART_TEST_SCENARIO", scenario)
	home := t.TempDir()
	binary := home + "/tart"
	const fixture = `#!/bin/sh
set -eu
test "$1" = exec
case "$2" in
  ephemera-fixture)
    case "$3" in
      /usr/bin/true) exit 0 ;;

    esac
    ;;
  -i)
    touch "$TART_HOME/runner-invoked"
    test "$3" = ephemera-fixture
    test "$4 $5 $6 $7" = "/usr/bin/sudo -n -u ephemera-runner"
    case "$*" in *synthetic-jit*) exit 1 ;; esac
    if env | grep -q synthetic-jit; then exit 1; fi
    IFS= read -r credential
    test "$credential" = synthetic-jit
    test "$EPHE_TART_TEST_SCENARIO" != "runner exits before listening"
    printf '2026-11-22 00:00:00Z: Listening for Jobs\n'
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(binary, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	backend := &Tart{binary: binary, home: home}
	err := backend.runGuest(ctx, Instance{Resource: "ephemera-fixture"}, "synthetic-jit", ready, make(chan error))
	if (err == nil) != wantReady {
		t.Fatalf("launch error = %v, want readiness %t", err, wantReady)
	}
	select {
	case <-ready:
		if !wantReady {
			t.Fatal("failed guest was marked ready")
		}
	default:
		if wantReady {
			t.Fatal("runner's listening signal did not mark the VM ready")
		}
	}
}

// This opt-in test provisions a disposable clone with synthetic data.
func TestTartGuestSmoke(t *testing.T) {
	home, ref := os.Getenv("EPHEMERA_TART_SMOKE_HOME"), os.Getenv("EPHEMERA_TART_SMOKE_IMAGE")
	if home == "" || ref == "" {
		t.Skip("set EPHEMERA_TART_SMOKE_HOME and EPHEMERA_TART_SMOKE_IMAGE on a macOS test host")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	backend, err := NewTart(home, 2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	image, check, err := backend.ResolveImage(ctx, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	i := Instance{ID: id, Resource: "ephemera-" + id, Backend: "tart", Image: image, Namespace: home}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := backend.Stop(ctx, i); err != nil {
			t.Error(err)
			return
		}
		if err := backend.Delete(ctx, i); err != nil {
			t.Error(err)
		}
		if err := backend.Stop(ctx, i); err != nil {
			t.Error(err)
		}
		if err := backend.Delete(ctx, i); err != nil {
			t.Error(err)
		}
	})
	if err := backend.Prepare(ctx, i); err != nil {
		t.Fatal(err)
	}
	t.Logf("prepared %s", i.Resource)
	vmExit, err := backend.startVM(i.Resource)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.waitGuestAgent(ctx, i.Resource, vmExit); err != nil {
		t.Fatal(err)
	}
	t.Log("Guest Agent reachable")
	provision := backend.command(ctx, "exec", i.Resource, "/usr/bin/sudo", "-n", "/bin/sh", "-c", tartProvision)
	if out, err := provision.CombinedOutput(); err != nil {
		t.Fatalf("provision runner: %v\n%s", err, out)
	}
	smoke := backend.command(ctx, "exec", "-i", i.Resource, "/usr/bin/sudo", "-n", "-u", "ephemera-runner",
		"/bin/sh", "-c", `IFS= read -r credential && test "$credential" = ephemera-offline-smoke && /usr/bin/id -un && cd /opt/ephemera-runner && ./bin/Runner.Listener --version`)
	smoke.Stdin = strings.NewReader("ephemera-offline-smoke\n")
	out, err := smoke.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ephemera-runner\n") || !strings.Contains(string(out), "2.338.2") {
		t.Fatal(fmt.Errorf("guest account, stdin or runner verification failed: %v (%s)", err, out))
	}
	t.Log("Guest received synthetic data as the non-admin runner account and ran the pinned runner")
}

func TestTartDeleteChecksIdentityAndRunningState(t *testing.T) {
	home := t.TempDir()
	binary := home + "/tart"
	script := `#!/bin/sh
case "$1" in
list)
  cat "$TART_HOME/state.json"
  ;;
delete)
  printf '[]\n' > "$TART_HOME/state.json"
  ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	backend := &Tart{home: home, binary: binary}
	id := uuid.NewString()
	i := Instance{ID: id, Resource: "ephemera-" + id, Backend: "tart", Namespace: home}
	writeState := func(running bool) {
		t.Helper()
		data := fmt.Sprintf(`[{"Name":"%s","Running":%t}]`, i.Resource, running)
		if err := os.WriteFile(home+"/state.json", []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeState(true)
	if err := backend.Delete(t.Context(), i); err == nil {
		t.Fatal("accepted deletion of a running clone")
	}
	writeState(false)
	foreign := i
	foreign.Namespace = home + "/elsewhere"
	if err := backend.Delete(t.Context(), foreign); err == nil {
		t.Fatal("accepted deletion in a different namespace")
	}
	if err := backend.Delete(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(t.Context(), i); err != nil {
		t.Fatal(err)
	}
}

func TestTartCandidateSmoke(t *testing.T) {
	home, ref := os.Getenv("EPHEMERA_TART_SMOKE_HOME"), os.Getenv("EPHEMERA_TART_SMOKE_IMAGE")
	if home == "" || ref == "" {
		t.Skip("set EPHEMERA_TART_SMOKE_HOME and EPHEMERA_TART_SMOKE_IMAGE on a macOS test host")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	tart, err := NewTart(home, 2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := tart.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tart.ResolveImage(ctx, ref, nil); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	instance := Instance{
		ID: id, Resource: "ephemera-" + id, Backend: "tart", Namespace: home, Image: ref,
		RunnerVersion: "2.338.2", RunnerSHA256: "25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388",
	}
	if err := tart.ValidateCandidate(ctx, instance); err != nil {
		t.Fatal(err)
	}
	entry, err := tart.lookup(ctx, instance)
	if err != nil || entry != nil {
		t.Fatalf("candidate VM was not removed: %v (%v)", entry, err)
	}
}
