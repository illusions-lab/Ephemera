package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/illusions-lab/Ephemera/internal/imagechannel"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/vm"
)

func TestImageUpdatePersistsSwitchAndRollback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	baseline := preparedImage{id: "old", check: func() error { return nil }}
	first := imagechannel.Recipe{Revision: 1, TartRef: "first", RunnerSHA256: "first"}
	next := imagechannel.Recipe{Revision: 2, TartRef: "next", RunnerSHA256: "next"}
	prepare := func(_ context.Context, recipe imagechannel.Recipe, _ bool) (preparedImage, error) {
		return preparedImage{recipe: &recipe, id: recipe.TartRef, check: func() error { return nil }}, nil
	}
	path := filepath.Join(t.TempDir(), "images", "state.json")
	updates := &imageUpdates{path: path, logger: logger, prepare: prepare, baseline: baseline, previous: baseline}
	r := &reconciler{images: updates}
	r.setImage(baseline)
	checked := func(recipe imagechannel.Recipe) {
		t.Helper()
		p, err := prepare(t.Context(), recipe, true)
		if err != nil {
			t.Fatal(err)
		}
		state := make(chan updateState, 1)
		r.imageChecked(runnerEvent{image: p, checked: time.Now(), imageReply: state})
		<-state
	}
	checked(first)
	oldInstance := r.template
	checked(next)
	if oldInstance.Image != "first" || r.template.Image != "next" {
		t.Fatalf("unexpected image selection: %s / %s", oldInstance.Image, r.template.Image)
	}

	// Failures from the old VM and from GitHub registration must not roll back.
	for range 4 {
		r.imageOutcome(oldInstance, runnerEvent{kind: exited, stage: "start VM", err: vm.ErrImageInfrastructure})
		r.imageOutcome(r.template, runnerEvent{kind: exited, stage: "register runner"})
	}
	if updates.state.Failures != 0 {
		t.Fatal("unrelated failures counted against candidate")
	}

	// Recover the previous image and the candidate after a daemon restart.
	restarted := &imageUpdates{path: path, logger: logger, prepare: prepare}
	active := restarted.recover(t.Context(), baseline)
	if active.id != "next" || restarted.previous.id != "first" {
		t.Fatal("active and rollback images were not recovered")
	}
	r.images = restarted
	r.setImage(active)
	failed := r.template
	for range 2 {
		r.imageOutcome(failed, runnerEvent{kind: exited, stage: "start VM", err: vm.ErrImageInfrastructure})
	}
	r.imageOutcome(failed, runnerEvent{kind: jobTerminal})
	for range 3 {
		r.imageOutcome(failed, runnerEvent{kind: exited, stage: "start VM", err: vm.ErrImageInfrastructure})
	}
	if r.template.Image != "first" || !restarted.state.RolledBack {
		t.Fatal("did not roll back after three consecutive failures")
	}

	republished := next
	republished.Revision++
	checked(republished)
	if r.template.Image != "first" {
		t.Fatal("republishing a rejected image activated it")
	}

	active = restarted.recover(t.Context(), baseline)
	if active.id != "first" {
		t.Fatal("rollback was not durable")
	}
}

func TestFailedImageCheckKeepsActiveImageAndPrunesOnlyUnreferenced(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var removed []string
	updates := &imageUpdates{
		path: filepath.Join(t.TempDir(), "state.json"), logger: logger,
		state:    updateState{Retired: []string{"busy", "unused", "current", "previous"}},
		previous: preparedImage{id: "previous"},
		prune:    func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}
	r := &reconciler{images: updates, instances: map[string]*instanceState{"job": {Instance: vm.Instance{Image: "busy"}}}}
	r.setImage(preparedImage{id: "current"})
	state := make(chan updateState, 1)
	r.imageChecked(runnerEvent{image: preparedImage{recipe: &imagechannel.Recipe{Revision: 2}}, err: errors.New("disk full"), checked: time.Now(), imageReply: state})
	<-state
	if r.template.Image != "current" || len(removed) != 1 || removed[0] != "unused" {
		t.Fatalf("active image %q, removed %v", r.template.Image, removed)
	}
}

func TestImageWatcherReportsUnavailableLocalChannel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("invalid channel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		updates := &imageUpdates{channelFile: filepath.Join(t.TempDir(), "missing.json"), logger: logger}
		r := &reconciler{images: updates, events: make(chan runnerEvent)}
		done := make(chan struct{})
		go func() { defer close(done); updates.watch(ctx, r, updateState{}) }()
		select {
		case event := <-r.events:
			if event.kind != imageChecked || !errors.Is(event.err, os.ErrNotExist) || event.image.recipe != nil {
				t.Fatalf("unexpected update event: %+v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("image check did not report a missing channel file")
		}
		cancel()
		<-done
	})
}

func TestImageAvailabilityTransitionsPreserveQuarantine(t *testing.T) {
	updates := &imageUpdates{}
	r := &reconciler{images: updates}
	r.setImage(preparedImage{id: "base"})
	active := r.active.Load()
	r.apply(runnerEvent{kind: imageUnavailable, source: active})
	if !r.imageBlocked.Load() {
		t.Fatal("accepted image after a transient probe failure")
	}
	r.apply(runnerEvent{kind: imageAvailable, source: active})
	if r.imageBlocked.Load() {
		t.Fatal("successful probe did not restore image availability")
	}
	updates.state.Failures = 3
	r.blockFailedImage()
	r.apply(runnerEvent{kind: imageUnavailable, source: active})
	r.apply(runnerEvent{kind: imageAvailable, source: active})
	if !r.imageBlocked.Load() || r.imageReason != failedImageQuarantine {
		t.Fatal("existence probe cleared a base quarantined for guest failure")
	}
}

func TestLateCompletionResetsCurrentImageFailureCount(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	updates := &imageUpdates{
		path:   filepath.Join(t.TempDir(), "state.json"),
		logger: logger,
		state:  updateState{Failures: 2},
	}
	r := &reconciler{images: updates, logger: logger}
	r.setImage(preparedImage{id: "base"})
	instance := vm.Instance{ID: "finished-runner", Image: "base", RunnerID: 91}
	r.instances = map[string]*instanceState{instance.ID: {Instance: instance, phase: running}}
	r.apply(runnerEvent{kind: exited, id: instance.ID})
	r.apply(runnerEvent{kind: cleaned, id: instance.ID})
	if updates.state.Failures != 2 {
		t.Fatal("process exit without terminal event reset image failures")
	}
	r.apply(runnerEvent{kind: statistics, update: orders.Update{
		Completed: []*scaleset.JobCompleted{{RunnerID: instance.RunnerID}},
	}})
	if updates.state.Failures != 0 {
		t.Fatal("GitHub completion after local exit did not reset image failures")
	}
}
