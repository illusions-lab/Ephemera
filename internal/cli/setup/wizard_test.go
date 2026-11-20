package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illusions-lab/Ephemera/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestSaveEditedConfig(t *testing.T) {
	for _, externalEdit := range []bool{false, true} {
		name := "unchanged"
		if externalEdit {
			name = "external edit"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			cfg := config.Defaults()
			cfg.GitHub.ScopeURL = "https://github.com/example/repo"
			cfg.GitHub.AppID, cfg.GitHub.InstallationID = 1, 2
			encode := func(cfg config.Config) []byte {
				t.Helper()
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			write := func(data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			write(encode(cfg))
			snapshot, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			before := encode(snapshot)
			snapshot.Runner.MaxConcurrent = 3
			candidate := encode(snapshot)
			want := candidate
			if externalEdit {
				cfg.Runner.MaxConcurrent = 4
				want = append([]byte("# external edit\n"), encode(cfg)...)
				write(want)
			}
			err = saveEditedConfig(path, before, candidate)
			if externalEdit {
				if err == nil || !strings.Contains(err.Error(), "changed while setup was open") {
					t.Fatalf("expected edit conflict, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, want) {
				t.Fatalf("saved contents = %q, %v, want %q", data, err, want)
			}
		})
	}
}
