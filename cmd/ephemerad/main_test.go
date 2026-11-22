package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRotatesDaemonLogAndRecordsStartupError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(10 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	config := filepath.Join(dir, "missing.yaml")
	if err := run([]string{"--log-file", path, "--config", config}); err == nil {
		t.Fatal("expected startup failure for missing configuration")
	}
	logs, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), "Daemon stopped") || !strings.Contains(string(logs), config) {
		t.Fatalf("active log does not contain the startup error: %s", logs)
	}
	archives, err := filepath.Glob(filepath.Join(dir, "daemon-*.log*"))
	if err != nil || len(archives) == 0 {
		t.Fatalf("expected a rotated archive, got %v (%v)", archives, err)
	}
}
