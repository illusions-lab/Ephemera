package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// retainExtra keeps the newest retired images that fit the discretionary
// budget. Rollback images and in-use images are protected by the caller.
func (u *imageUpdates) retainExtra(retired []string, protected ...string) map[string]bool {
	if u.extraRetained == 0 {
		return nil
	}
	seen := make(map[string]bool)
	size := uint64(0)
	if u.cacheMax > 0 {
		for _, id := range protected {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			bytes, err := u.imageBytes(id)
			if err != nil || bytes > u.cacheMax-size {
				return nil
			}
			size += bytes
		}
	}
	retained := make(map[string]bool)
	for i := len(retired) - 1; i >= 0 && len(retained) < u.extraRetained; i-- {
		id := retired[i]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if u.cacheMax > 0 {
			bytes, err := u.imageBytes(id)
			if err != nil || bytes > u.cacheMax-size {
				continue
			}
			size += bytes
		}
		retained[id] = true
	}
	return retained
}

func allocatedImageBytes(path string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			return errors.New("image allocation could not be measured")
		}
		total += uint64(stat.Blocks) * 512
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return total, err
}
