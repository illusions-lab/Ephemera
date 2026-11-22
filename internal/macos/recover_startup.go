package macos

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const recoveryLabelPrefix = Label + ".recover-"
const activationJournal = ".activation-transaction.json"

// ActivationPending reports whether a previous switch still needs recovery.
func ActivationPending() (bool, error) {
	_, err := os.Stat(filepath.Join(Root(), activationJournal))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// GuardStartup refuses to start an orphaned candidate. Recovery runs in its
// own LaunchAgent so it survives booting out the candidate.
func GuardStartup(ctx context.Context, activationID string) error {
	if runtime.GOOS != "darwin" || activationID == "" {
		return nil
	}
	root := Root()
	journal := filepath.Join(root, activationJournal)
	if _, err := os.Stat(journal); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	id, err := uuid.Parse(activationID)
	if err != nil || id.String() != activationID {
		return errors.New("invalid LaunchAgent activation identity")
	}
	label := recoveryLabelPrefix + activationID
	path := recoveryPlist(root, label)
	fd, err := openActivationLock(root)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	switch err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); {
	case errors.Is(err, unix.EWOULDBLOCK):
		// An active apply owns the lock unless the candidate has already
		// scheduled its independent recovery job.
		if _, err := os.Stat(path); err == nil {
			return errors.New("interrupted activation is being recovered")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	case err != nil:
		return err
	}
	if _, err := os.Stat(journal); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if err := launchctl(ctx, "print", domain+"/"+label); err != nil {
		plist := struct {
			XMLName xml.Name     `xml:"plist"`
			Version string       `xml:"version,attr"`
			Dict    []plistValue `xml:"dict>value"`
		}{Version: "1.0", Dict: []plistValue{
			{"Label", label},
			{"ProgramArguments", []string{filepath.Join(filepath.Dir(executable), "ephemera"), "--recover-activation", label}},
			{"RunAtLoad", true},
		}}
		data, err := xml.MarshalIndent(plist, "", "  ")
		if err != nil {
			return err
		}
		command := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", "-")
		command.Stdin = bytes.NewReader(append([]byte(xml.Header), data...))
		normalized, err := command.Output()
		if err != nil {
			return err
		}
		if err := replaceFile(path, normalized); err != nil {
			return err
		}
		if err := launchctl(ctx, "bootstrap", domain, path); err != nil {
			return fmt.Errorf("start activation recovery: %w", err)
		}
	} else if err := launchctl(ctx, "kickstart", domain+"/"+label); err != nil {
		return fmt.Errorf("resume activation recovery: %w", err)
	}
	return errors.New("interrupted activation is being recovered")
}

func recoveryPlist(root, label string) string {
	return filepath.Join(root, "."+label+".plist")
}

// RecoverStartup restores the old LaunchAgent from a separate launchd job.
func RecoverStartup(ctx context.Context, label string) error {
	if runtime.GOOS != "darwin" || os.Geteuid() == 0 {
		return errors.New("activation recovery requires the macOS service account")
	}
	value, ok := strings.CutPrefix(label, recoveryLabelPrefix)
	id, err := uuid.Parse(value)
	if !ok || err != nil || value != id.String() {
		return errors.New("invalid activation recovery label")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	a := activation{
		root: Root(), plist: filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
		domain: fmt.Sprintf("gui/%d", os.Getuid()),
	}
	fd, err := openActivationLock(a.root)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	journal := filepath.Join(a.root, activationJournal)
	data, err := os.ReadFile(journal)
	if err == nil {
		var previous previousService
		if err := json.Unmarshal(data, &previous); err != nil {
			return err
		}
		if err := a.recover(journal, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := removeIfPresent(recoveryPlist(a.root, label)); err != nil {
		return err
	}
	// The recovery is durable. Unload the transient job.
	return launchctl(ctx, "bootout", a.domain+"/"+label)
}

func openActivationLock(root string) (int, error) {
	return unix.Open(filepath.Join(root, ".activation.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
}
