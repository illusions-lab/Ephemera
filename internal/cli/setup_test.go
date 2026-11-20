package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupResume(t *testing.T) {
	if err := runSetup([]string{"--resume"}); err == nil || !strings.Contains(err.Error(), "requires --non-interactive") {
		t.Fatalf("expected --resume to require --non-interactive, got %v", err)
	}
	example, err := os.ReadFile("../../configs/ephemera.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	for _, tc := range []struct {
		name      string
		wantError string
	}{
		{name: "same inputs"},
		{name: "different YAML formatting"},
		{name: "different config", wantError: "does not match the saved configuration"},
		{name: "different key bytes", wantError: "does not match the saved key"},
		{name: "missing config", wantError: "config.yaml"},
		{name: "missing key", wantError: "github-app.pem"},
		{name: "wrong key mode", wantError: "regular file with mode 0600"},
		{name: "linked key", wantError: "regular file with mode 0600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, dest := t.TempDir(), t.TempDir()
			source, sourceKey := filepath.Join(input, "input.yaml"), filepath.Join(input, "input.pem")
			write := func(path string, data []byte, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			write(source, example, 0644)
			write(sourceKey, key, 0600)
			if err := setupFromFiles(dest, source, sourceKey, false); err != nil {
				t.Fatal(err)
			}
			savedConfig := filepath.Join(dest, "config.yaml")
			savedKey := filepath.Join(dest, "credentials", "github-app.pem")
			switch tc.name {
			case "same inputs":
				data, err := os.ReadFile(savedConfig)
				if err != nil {
					t.Fatal(err)
				}
				write(source, data, 0644)
			case "different YAML formatting":
				write(source, append([]byte("# formatting only\n"), example...), 0644)
			case "different config":
				write(source, bytes.Replace(example, []byte("max_concurrent: 2"), []byte("max_concurrent: 3"), 1), 0644)
			case "different key bytes":
				write(sourceKey, append([]byte("\n"), key...), 0600)
			case "missing config", "missing key", "linked key":
				path := savedKey
				if tc.name == "missing config" {
					path = savedConfig
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if tc.name == "linked key" {
					if err := os.Symlink(sourceKey, path); err != nil {
						t.Fatal(err)
					}
				}
			case "wrong key mode":
				if err := os.Chmod(savedKey, 0644); err != nil {
					t.Fatal(err)
				}
			}

			assertUnchanged := func(path string) func() {
				t.Helper()
				before, statErr := os.Lstat(path)
				if os.IsNotExist(statErr) {
					return func() {
						if _, err := os.Lstat(path); !os.IsNotExist(err) {
							t.Errorf("%s should still be missing, got %v", path, err)
						}
					}
				}
				if statErr != nil {
					t.Fatal(statErr)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return func() {
					after, err := os.Lstat(path)
					if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
						t.Fatalf("%s metadata changed: %v, %v", path, after, err)
					}
					if current, err := os.ReadFile(path); err != nil || !bytes.Equal(data, current) {
						t.Errorf("%s contents changed", path)
					}
				}
			}
			checkConfig, checkKey := assertUnchanged(savedConfig), assertUnchanged(savedKey)
			err := setupFromFiles(dest, source, sourceKey, true)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantError)
			}
			checkConfig()
			checkKey()
		})
	}
}
