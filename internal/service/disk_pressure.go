package service

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

type imageDiskPressure func(string) bool

func (p imageDiskPressure) forImage(id string) bool {
	return p != nil && id != "" && p(id)
}

func (r *reconciler) imageDiskPressure(starting int) imageDiskPressure {
	var volume unix.Stat_t
	if err := unix.Stat(r.budget.root, &volume); err != nil {
		return nil
	}
	return func(id string) bool {
		// An image cache on another filesystem cannot unblock admission.
		var image unix.Stat_t
		if err := unix.Stat(r.images.imagePath(id), &image); err != nil || image.Dev != volume.Dev {
			return false
		}
		free, err := availableDiskBytes(r.budget.root)
		// The measured free space accounts for copy-on-write references. Stop
		// once one more VM fits, regardless of how large the deleted image was.
		return err == nil && r.budget.diskSlots(free) <= uint64(starting)
	}
}

func (r *reconciler) imageInUse(id string) bool {
	if id == r.template.Image {
		return true
	}
	for _, instance := range r.instances {
		if !instance.LocalReclaimed && instance.Image == id {
			return true
		}
	}
	return false
}

func (u *imageUpdates) removePrepared(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return u.prune(ctx, id)
}
