//go:build darwin

package macos

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStartupSchedulesIndependentRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := Root()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(home, "launchctl.log")
	t.Setenv("EPHE_RECOVERY_TEST_LOG", log)
	writeActivationFile(t, filepath.Join(bin, "launchctl"), `#!/bin/sh
case "$1:$2" in
  print:*/*) exit 1 ;;
  print:*) exit 1 ;;
  bootstrap:*) printf '%s\n' "$3" > "$EPHE_RECOVERY_TEST_LOG" ;;
  bootout:*) exit 0 ;;
  *) exit 90 ;;
esac
`, 0700)

	const id = "fb36aabb-e926-4321-8d9a-cfeb8ffd9110"
	journal := filepath.Join(root, activationJournal)
	writeActivationFile(t, journal, "{}", 0600)
	fd, err := openActivationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	if err := GuardStartup(context.Background(), id); err != nil {
		unix.Close(fd)
		t.Fatalf("active transaction was blocked: %v", err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}

	err = GuardStartup(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "interrupted activation") {
		t.Fatalf("orphaned candidate started: %v", err)
	}
	plist := strings.TrimSpace(readActivationFile(t, log))
	if got := readPlist(t, plist, "Label"); got != recoveryLabelPrefix+id {
		t.Fatalf("recovery label = %q", got)
	}
	if got := readPlist(t, plist, "ProgramArguments.1"); got != "--recover-activation" {
		t.Fatalf("recovery command = %q", got)
	}
}
