package image

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBootCheckRequiresGuestValidationMarker(t *testing.T) {
	tools := t.TempDir()
	work := t.TempDir()
	t.Setenv("PATH", tools)
	for name, script := range map[string]string{
		"qemu-img":      "exit 0",
		"cloud-localds": "exit 0",
		"qemu-system-x86_64": `for arg do
  case "$arg" in
    file:*) printf '%s\n' "$EPHEMERA_TEST_SERIAL" > "${arg#file:}" ;;
  esac
done`,
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	var cloudConfig struct {
		Commands [][]string `yaml:"runcmd"`
		Files    []struct {
			Path, Permissions, Content string
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(bootConfig), &cloudConfig); err != nil {
		t.Fatal(err)
	}
	if len(cloudConfig.Files) != 2 || cloudConfig.Files[0].Path != "/run/ephemera/jitconfig" ||
		cloudConfig.Files[0].Permissions != "0600" || cloudConfig.Files[0].Content != "ephemera-offline-smoke" ||
		!slices.ContainsFunc(cloudConfig.Commands, func(cmd []string) bool {
			return slices.Equal(cmd, []string{"systemctl", "start", "ephemera-runner.service"})
		}) {
		t.Fatalf("invalid offline cloud-init bootstrap: %+v", cloudConfig)
	}
	cfg := Config{MemoryMiB: 2048, VCPUs: 2}
	t.Setenv("EPHEMERA_TEST_SERIAL", "guest shut down")
	if err := bootCheck(context.Background(), work, "/base.qcow2", cfg); err == nil {
		t.Fatal("a successful QEMU exit without the validation marker should fail")
	}
	t.Setenv("EPHEMERA_TEST_SERIAL", bootMarker+"\r")
	if err := bootCheck(context.Background(), work, "/base.qcow2", cfg); err != nil {
		t.Fatal(err)
	}
}
