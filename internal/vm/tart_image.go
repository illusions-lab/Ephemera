package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const TartRunnerVersion = "2.338.2"
const TartRunnerSHA256 = "25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388"

func tartImageKey(ref, version, digest string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(ref+"\n"+version+"\n"+digest)))
}

func (t *Tart) preparedPath(ref, version, digest string) string {
	return filepath.Join(t.home, "ephemera-prepared-"+tartImageKey(ref, version, digest))
}

func (t *Tart) CheckPreparedImage(ctx context.Context, ref, version, digest string) (string, error) {
	data, err := os.ReadFile(t.preparedPath(ref, version, digest))
	if err != nil {
		return "", err
	}
	name := string(data)
	return name, t.checkLocalImage(ctx, name)
}

func (t *Tart) checkLocalImage(ctx context.Context, name string) error {
	id, ok := strings.CutPrefix(name, "ephemera-base-")
	parsed, err := uuid.Parse(id)
	if !ok || err != nil || parsed.String() != id {
		return errors.New("invalid prepared Tart image identity")
	}
	directory := filepath.Join(t.home, "vms", name)
	cfg, err := os.ReadFile(filepath.Join(directory, "config.json"))
	if err != nil {
		return fmt.Errorf("read prepared Tart base configuration: %w", err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &metadata); err != nil || len(metadata) == 0 {
		return errors.New("prepared Tart base configuration is invalid")
	}
	checkFile := func(filename string) error {
		path := filepath.Join(directory, filename)
		file, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !file.Mode().IsRegular() || file.Size() == 0 {
			return fmt.Errorf("invalid Tart base file %s", filename)
		}
		return nil
	}
	if err := checkFile("nvram.bin"); err != nil {
		return err
	}
	if err := checkFile("disk.img"); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := errors.Join(checkFile("manifest.json"), checkFile("overlay.asif")); err != nil {
			return err
		}
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	entries, err := t.list(check, "local")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == name && !entry.Running {
			return nil
		}
	}
	return fmt.Errorf("prepared Tart base %s is unavailable", name)
}

func (t *Tart) PrepareImage(ctx context.Context, ref, version, digest string, progress io.Writer) (string, func() error, error) {
	path := t.preparedPath(ref, version, digest)
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return "", nil, err
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	check := func(name string) func() error {
		return func() error { return t.checkLocalImage(context.Background(), name) }
	}
	if name, err := t.CheckPreparedImage(ctx, ref, version, digest); err == nil {
		return name, check(name), nil
	}
	if err := t.CheckImage(ctx, ref); err != nil {
		if _, _, err := t.ResolveImage(ctx, ref, progress); err != nil {
			return "", nil, err
		}
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(path)).String()
	i := Instance{ID: id, Resource: "ephemera-" + id, Backend: "tart", Namespace: t.home, Image: ref, RunnerVersion: version, RunnerSHA256: digest}
	remove := func(ctx context.Context) error {
		if err := t.Stop(ctx, i); err != nil {
			return err
		}
		return t.Delete(ctx, i)
	}
	if err := remove(ctx); err != nil {
		return "", nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = remove(cleanup)
	}()
	if err := t.Prepare(ctx, i); err != nil {
		return "", nil, err
	}
	if err := t.provision(ctx, i); err != nil {
		return "", nil, err
	}
	if err := t.Stop(ctx, i); err != nil {
		return "", nil, err
	}
	name := "ephemera-base-" + uuid.NewString()
	// Record the candidate before cloning so a later retry can reclaim it.
	candidate := path + ".candidate"
	if data, err := os.ReadFile(candidate); err == nil {
		if string(data) != name {
			active, _ := os.ReadFile(path)
			if string(data) != string(active) {
				if err := t.RemoveCachedImage(ctx, string(data)); err != nil {
					return "", nil, err
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if err := writeTartIndex(candidate, name); err != nil {
		return "", nil, err
	}
	if err := t.command(ctx, "clone", i.Resource, name).Run(); err != nil {
		return "", nil, err
	}
	if err := check(name)(); err != nil {
		return "", nil, err
	}
	if err := os.Rename(candidate, path); err != nil {
		return "", nil, err
	}
	return name, check(name), nil
}

func writeTartIndex(path, value string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".image-index-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(value)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
