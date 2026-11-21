package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/orders"
)

func TestDoctorChecks(t *testing.T) {
	for _, test := range []struct {
		name       string
		change     func(*doctorChecks)
		failed     bool
		skipGitHub bool
		want       []string
	}{
		{name: "ready", want: []string{"[OK] Configuration", "[OK] GitHub App key", "[OK] GitHub App access", "[OK] Scale Set", "[OK] Runner image cache", "[OK] QEMU", "[OK] KVM", "[OK] Disk space"}},
		{
			name: "invalid configuration", failed: true, skipGitHub: true,
			change: func(c *doctorChecks) {
				c.loadConfig = func(string) (config.Config, error) { return config.Config{}, errors.New("invalid YAML") }
			},
			want: []string{"[FAIL] Configuration", "invalid YAML", "[SKIP] GitHub / Scale Set", "[SKIP] Runner image cache"},
		},
		{
			name: "invalid private key", failed: true, skipGitHub: true,
			change: func(c *doctorChecks) {
				c.readKey = func(string) ([]byte, error) { return nil, errors.New("invalid RSA key") }
			},
			want: []string{"[FAIL] GitHub App key", "invalid RSA key", "[SKIP] GitHub / Scale Set"},
		},
		{
			name: "protected private key", failed: true, skipGitHub: true,
			change: func(c *doctorChecks) {
				c.readKey = func(string) ([]byte, error) { return nil, os.ErrPermission }
			},
			want: []string{"[FAIL] GitHub App key", "Run sudo ephe doctor", "[SKIP] GitHub / Scale Set"},
		},
		{
			name: "GitHub permission refused", failed: true,
			change: func(c *doctorChecks) {
				c.probeScaleSet = func(context.Context, orders.Config) (bool, error) {
					return false, errors.New("403 Forbidden")
				}
			},
			want: []string{"[FAIL] GitHub / Scale Set", "403 Forbidden", "runner management permissions"},
		},
		{
			name: "GitHub probe timeout", failed: true,
			change: func(c *doctorChecks) {
				c.probeScaleSet = func(context.Context, orders.Config) (bool, error) {
					return false, fmt.Errorf("probe: %w", context.DeadlineExceeded)
				}
			},
			want: []string{"[FAIL] GitHub / Scale Set", "GitHub probe timed out", "Check network access"},
		},
		{
			name: "Scale Set missing", failed: true,
			change: func(c *doctorChecks) {
				c.probeScaleSet = func(context.Context, orders.Config) (bool, error) { return false, nil }
			},
			want: []string{"[OK] GitHub App access", "[FAIL] Scale Set", "was not found in the default runner group"},
		},
		{
			name: "image missing", failed: true,
			change: func(c *doctorChecks) {
				c.checkImage = func(context.Context, string, image.Config) (image.Image, error) {
					return image.Image{}, os.ErrNotExist
				}
			},
			want: []string{"[FAIL] Runner image cache", "run sudo ephe image prepare"},
		},
		{
			name: "image corrupted", failed: true,
			change: func(c *doctorChecks) {
				c.checkImage = func(context.Context, string, image.Config) (image.Image, error) {
					return image.Image{}, errors.New("cached image SHA-256 mismatch")
				}
			},
			want: []string{"[FAIL] Runner image cache", "cached image SHA-256 mismatch"},
		},
		{
			name: "QEMU missing", failed: true,
			change: func(c *doctorChecks) {
				c.lookPath = func(name string) (string, error) {
					if name == "qemu-img" {
						return "", os.ErrNotExist
					}
					return name, nil
				}
			},
			want: []string{"[FAIL] QEMU", "Missing: qemu-img"},
		},
		{
			name: "KVM unavailable is a warning",
			change: func(c *doctorChecks) {
				c.checkKVM = func() error { return os.ErrNotExist }
			},
			want: []string{"[WARN] KVM", "fall back to software emulation"},
		},
		{
			name: "disk full", failed: true,
			change: func(c *doctorChecks) {
				c.diskSpace = func(path string) (uint64, string, error) { return 0, path, nil }
			},
			want: []string{"[FAIL] Disk space", "No available space"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.VM.Arch = "amd64"
			cfg.GitHub.ScopeURL = "https://github.com/example"
			cfg.GitHub.AppID, cfg.GitHub.InstallationID = 12, 34
			const key = "fixture private key"
			checks := doctorChecks{
				loadConfig: func(string) (config.Config, error) { return cfg, nil },
				readKey:    func(string) ([]byte, error) { return []byte(key), nil },
				probeScaleSet: func(ctx context.Context, input orders.Config) (bool, error) {
					if _, ok := ctx.Deadline(); !ok {
						t.Error("GitHub probe should have a timeout")
					}
					if input.AppID != 12 || input.InstallationID != 34 || input.ScopeURL != cfg.GitHub.ScopeURL ||
						input.ScaleSetName != cfg.GitHub.ScaleSetName || string(input.PrivateKey) != key || input.Create || input.Claim {
						t.Error("GitHub probe was not configured for a read-only lookup")
					}
					return true, nil
				},
				checkImage: func(_ context.Context, path string, _ image.Config) (image.Image, error) {
					return image.Image{Path: path + "/image.qcow2"}, nil
				},
				hostArch:  "amd64",
				lookPath:  func(name string) (string, error) { return name, nil },
				checkKVM:  func() error { return nil },
				diskSpace: func(path string) (uint64, string, error) { return 16 << 30, path, nil },
			}
			if test.change != nil {
				test.change(&checks)
			}
			probe := checks.probeScaleSet
			called := false
			checks.probeScaleSet = func(ctx context.Context, cfg orders.Config) (bool, error) {
				called = true
				return probe(ctx, cfg)
			}
			var output bytes.Buffer
			err := diagnose(t.Context(), &output, doctorPaths{config: "/config.yaml", key: "/key.pem", cache: "/image"}, checks)
			if (err != nil) != test.failed || called == test.skipGitHub {
				t.Fatalf("unexpected result: error=%v, GitHub probe called=%v\n%s", err, called, &output)
			}
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Errorf("missing %q in output:\n%s", want, &output)
				}
			}
			if strings.Contains(output.String(), key) {
				t.Fatal("diagnostics printed private key contents")
			}
		})
	}
}
