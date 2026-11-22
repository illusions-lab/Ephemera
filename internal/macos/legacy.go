package macos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Older installers wrote a plist that read the editable files on every launch.
// Pin those inputs before setup can replace them, so rollback can relaunch it.
func (a activation) pinLegacyConfig(ctx context.Context, previous *previousService) error {
	if previous.Plist == nil {
		return nil
	}
	tmp, err := os.CreateTemp(a.root, ".legacy-plist-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(previous.Plist)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	plutil := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "/usr/bin/plutil", append(args, tmp.Name())...).CombinedOutput()
	}
	daemon, err := plutil("-extract", "ProgramArguments.0", "raw")
	if err != nil {
		return fmt.Errorf("read previous LaunchAgent arguments: %w", err)
	}
	if strings.TrimSpace(string(daemon)) != filepath.Join(a.root, "current", "ephemerad") {
		return nil
	}
	if arg, err := plutil("-extract", "ProgramArguments.2", "raw"); err == nil && strings.TrimSpace(string(arg)) == "--config" {
		return nil
	}
	config, err := os.ReadFile(filepath.Join(a.root, "config.yaml"))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(a.root, "credentials", "github-app.pem"))
	if err != nil {
		return err
	}
	dir, err := a.saveSnapshot(uuid.NewString(), config, key)
	if err != nil {
		return err
	}
	for index, arg := range []string{"--config", filepath.Join(dir, "config.yaml"), "--github-app-key", filepath.Join(dir, "github-app.pem")} {
		out, err := plutil("-insert", fmt.Sprintf("ProgramArguments.%d", index+2), "-string", arg)
		if err != nil {
			return fmt.Errorf("pin previous LaunchAgent configuration: %w: %s", err, out)
		}
	}
	previous.Plist, err = os.ReadFile(tmp.Name())
	return err
}

// Recover a transaction left by the shell installer before upgrading its plist.
func (a activation) recoverLegacy() error {
	journal := filepath.Join(a.root, ".install-transaction")
	data, err := os.ReadFile(journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	work := strings.TrimSuffix(string(data), "\n")
	fail := func(err error) error {
		return fmt.Errorf("recover previous installation using %s: %w", journal, err)
	}
	if filepath.Dir(work) != a.root || !strings.HasPrefix(filepath.Base(work), ".install.") {
		return fail(errors.New("invalid recovery directory"))
	}
	if info, err := os.Lstat(work); err != nil || !info.IsDir() {
		return fail(errors.New("missing or invalid recovery directory"))
	}
	target, err := os.ReadFile(filepath.Join(work, "previous"))
	if err != nil {
		return fail(err)
	}
	previous := previousService{Current: strings.TrimSuffix(string(target), "\n")}
	previous.Plist, err = os.ReadFile(filepath.Join(work, "previous.plist"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	_, err = os.Stat(filepath.Join(work, "was-running"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	previous.Loaded = err == nil
	if previous.Loaded && len(previous.Plist) == 0 {
		return fail(errors.New("missing previous LaunchAgent plist"))
	}
	if err := a.recover(journal, previous); err != nil {
		return err
	}
	return os.RemoveAll(work)
}
