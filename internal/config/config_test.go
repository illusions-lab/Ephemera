package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSetupAfterInterruptedKeyWrite(t *testing.T) {
	for _, tc := range []struct {
		name         string
		input        string
		mode         os.FileMode
		link         bool
		configExists bool
	}{
		{name: "same key", input: "original", mode: 0600},
		{name: "different key", input: "different", mode: 0600},
		{name: "wrong permissions", input: "original", mode: 0644},
		{name: "symbolic link", input: "original", mode: 0600, link: true},
		{name: "existing configuration", input: "original", mode: 0600, configExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			credentials := filepath.Join(dir, "credentials")
			if err := os.Mkdir(credentials, 0700); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(credentials, "github-app.pem")
			original := key
			if tc.link {
				original = filepath.Join(dir, "original.pem")
				if err := os.Symlink(original, key); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(original, []byte("original"), tc.mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.yaml")
			if tc.configExists {
				if err := os.WriteFile(path, []byte("existing"), 0644); err != nil {
					t.Fatal(err)
				}
			}

			err := WriteSetup(dir, []byte("candidate"), []byte(tc.input))
			wantSuccess := tc.name == "same key"
			if (err == nil) != wantSuccess {
				t.Fatalf("WriteSetup error = %v, want success = %t", err, wantSuccess)
			}
			assertSetupFile(t, original, "original", tc.mode)
			if tc.link {
				if target, err := os.Readlink(key); err != nil || target != original {
					t.Fatalf("Readlink = %q, %v", target, err)
				}
			}
			switch {
			case wantSuccess:
				assertSetupFile(t, path, "candidate", 0644)
				if info, err := os.Stat(credentials); err != nil || info.Mode() != os.ModeDir|0700 {
					t.Fatalf("credentials directory = %v, %v", info, err)
				}
			case tc.configExists:
				assertSetupFile(t, path, "existing", 0644)
			default:
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("expected no configuration, got %v", err)
				}
			}
			for _, parent := range []string{dir, credentials} {
				entries, err := filepath.Glob(filepath.Join(parent, ".ephemera-*"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("temporary files = %v, %v", entries, err)
				}
			}
		})
	}
}

func assertSetupFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("%s contents = %q, %v", path, data, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode() != mode {
		t.Fatalf("%s metadata = %v, %v", path, info, err)
	}
}

func TestTartConfigRequiresPinnedMacOSImage(t *testing.T) {
	cfg := Defaults()
	cfg.GitHub.ScopeURL = "https://github.com/example"
	cfg.GitHub.AppID, cfg.GitHub.InstallationID = 1, 2
	cfg.Runner.Execute = true
	cfg.VM.Backend, cfg.VM.Image, cfg.VM.Arch = "tart", "macos", "arm64"
	cfg.VM.ImageRef = "ghcr.io/example/macos@sha256:" + strings.Repeat("a", 64)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.VM.ImageRef = "ghcr.io/example/macos:latest"
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a mutable tag")
	}
}

func TestMacDefaultsObserveWithPinnedTartImage(t *testing.T) {
	cfg := MacDefaults()
	cfg.GitHub.ScopeURL = "https://github.com/owner/repo"
	cfg.GitHub.AppID, cfg.GitHub.InstallationID = 1, 2
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Execute || cfg.Runner.MaxConcurrent != 1 || cfg.VM.Backend != "tart" || cfg.VM.Arch != "arm64" || cfg.VM.ImageRef == "" {
		t.Fatalf("unexpected macOS defaults: runner=%+v vm=%+v", cfg.Runner, cfg.VM)
	}
}
