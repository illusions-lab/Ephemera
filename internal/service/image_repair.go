package service

import (
	"context"
	"time"
)

const awaitingImageRepair = "runner image is unavailable and awaiting repair"

func (r *reconciler) monitorImage(ctx context.Context, baseline func(context.Context) (string, func() error, error)) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		active := r.active.Load()
		if active == nil || active.check == nil || active.check() == nil {
			if active != nil && r.imageBlocked.Load() {
				if err := r.submit(ctx, runnerEvent{kind: imageAvailable, source: active}); err != nil {
					return
				}
			}
			failures = 0
			timer.Reset(2 * time.Second)
			continue
		}
		if err := r.submit(ctx, runnerEvent{kind: imageUnavailable, source: active}); err != nil {
			return
		}
		work, cancel := context.WithTimeout(ctx, 55*time.Minute)
		var repaired preparedImage
		var err error
		if active.recipe != nil {
			repaired, err = r.images.prepare(work, *active.recipe, false)
		} else {
			repaired.id, repaired.check, err = baseline(work)
			if err == nil && repaired.check != nil {
				err = repaired.check()
			}
		}
		cancel()
		if err == nil {
			failures = 0
		} else {
			failures++
			r.logger.Warn("Repair runner image", "image", active.id, "error", err)
		}
		if r.submit(ctx, runnerEvent{kind: imageRepaired, source: active, image: repaired, err: err}) != nil {
			return
		}
		timer.Reset(backoff(max(2, failures)))
	}
}

func (r *reconciler) repairImage(event runnerEvent) {
	if event.source != r.active.Load() || event.err != nil {
		return
	}
	if r.images != nil {
		u := r.images
		state := u.state
		state.Failures = 0
		if event.source.id != event.image.id {
			r.retire(&state, *event.source)
		}
		if err := u.save(state); err != nil {
			r.logger.Error("Record repaired image", "error", err)
			return
		}
		if event.source.recipe == nil {
			if u.previous.id == u.baseline.id {
				u.previous = event.image
			}
			u.baseline = event.image
		}
	}
	r.setImage(event.image)
	r.logger.Info("Recovered runner image", "image", event.image.id)
}
