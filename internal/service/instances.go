package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/illusions-lab/Ephemera/internal/vm"
	"golang.org/x/sys/unix"
)

type instanceStore string

func openInstances(root string) (instanceStore, func(), error) {
	if !filepath.IsAbs(root) {
		return "", nil, errors.New("instance state directory must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", nil, errors.New("instance state directory must be private (0700)")
	}
	fd, err := unix.Open(filepath.Join(root, ".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return "", nil, fmt.Errorf("lock instance state: %w", err)
	}
	return instanceStore(root), func() { unix.Close(fd) }, nil
}

func runnerName(id string) string { return "ephemera-" + id }

func (s instanceStore) path(id string) string {
	return filepath.Join(string(s), id+".json")
}

func (s instanceStore) load() ([]vm.Instance, error) {
	entries, err := os.ReadDir(string(s))
	if err != nil {
		return nil, err
	}
	var instances []vm.Instance
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".record-") {
			if err := os.Remove(filepath.Join(string(s), entry.Name())); err != nil {
				return nil, fmt.Errorf("discard incomplete instance record: %w", err)
			}
			continue
		}
		id, record := strings.CutSuffix(entry.Name(), ".json")
		if !record {
			continue
		}
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("invalid instance record %q", entry.Name())
		}
		data, err := os.ReadFile(s.path(id))
		if err != nil {
			return nil, err
		}
		var instance vm.Instance
		if err := json.Unmarshal(data, &instance); err != nil {
			return nil, fmt.Errorf("read instance %s: %w", id, err)
		}
		if instance.ID != id || instance.Resource != runnerName(id) || instance.ScaleSetID <= 0 ||
			instance.RunnerID < 0 || instance.ScaleSet == "" || instance.Backend == "" || instance.Image == "" {
			return nil, fmt.Errorf("invalid identity in instance record %s", id)
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func (s instanceStore) save(i vm.Instance) error {
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(string(s), ".record-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.path(i.ID)); err != nil {
		return err
	}
	return s.sync()
}

func (s instanceStore) remove(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.sync()
}

func (s instanceStore) sync() error {
	dir, err := os.Open(string(s))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
