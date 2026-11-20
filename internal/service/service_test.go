package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/image"
	"github.com/illusions-lab/Ephemera/internal/orders"
	"github.com/illusions-lab/Ephemera/internal/status"
)

func TestReadinessAndCancellation(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: "../../configs/ephemera.example.yaml", KeyPath: keyPath, CacheDir: dir}
	t.Run("image failure", func(t *testing.T) {
		t.Setenv("NOTIFY_SOCKET", "")
		var logs bytes.Buffer
		failure := errors.New("image build failed")
		err := run(t.Context(), opts, slog.New(slog.NewTextHandler(&logs, nil)),
			func(context.Context, string, image.Config, *slog.Logger) (string, func() error, error) {
				return "", nil, failure
			}, func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error {
				t.Fatal("listener started after image failure")
				return nil
			})
		if !errors.Is(err, failure) || !strings.Contains(logs.String(), "state=Degraded") {
			t.Fatalf("result: %v, logs: %s", err, &logs)
		}
	})
	t.Run("ready after initial session update", func(t *testing.T) {
		socketDir, err := os.MkdirTemp("", "ephe-notify-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(socketDir)
		opts := opts
		opts.StatusSocket = filepath.Join(socketDir, "status.sock")
		addr := &net.UnixAddr{Name: filepath.Join(socketDir, "notify"), Net: "unixgram"}
		conn, err := net.ListenUnixgram("unixgram", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		t.Setenv("NOTIFY_SOCKET", addr.Name)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		read := func(want string) {
			t.Helper()
			conn.SetReadDeadline(time.Now().Add(time.Second))
			data := make([]byte, 128)
			n, err := conn.Read(data)
			if err != nil || string(data[:n]) != want {
				t.Fatalf("notification: %q, %v; want %q", data[:n], err, want)
			}
		}
		readStatus := func(phase, github string) status.Snapshot {
			t.Helper()
			snapshot, err := status.Read(ctx, opts.StatusSocket)
			if err != nil || snapshot.Phase != phase || snapshot.GitHub != github || snapshot.Intake != "observing" {
				t.Fatalf("unexpected status: %+v, %v", snapshot, err)
			}
			return snapshot
		}
		err = run(ctx, opts, slog.New(slog.DiscardHandler),
			func(_ context.Context, cache string, cfg image.Config, _ *slog.Logger) (string, func() error, error) {
				if cache != dir || cfg.Platform.String() != "ubuntu/amd64" {
					t.Fatalf("unexpected image request: %s, %+v", cache, cfg)
				}
				read("STATUS=Starting")
				read("STATUS=PreparingImage")
				if snapshot := readStatus("PreparingImage", "pending"); snapshot.Demand != nil {
					t.Fatal("job demand must be absent before the GitHub session opens")
				}
				return "image", func() error { return nil }, nil
			}, func(ctx context.Context, cfg orders.Config, _ *slog.Logger, update func(context.Context, orders.Update) error) error {
				read("STATUS=Connecting")
				readStatus("Connecting", "connecting")
				if cfg.Claim {
					t.Fatal("default mode must observe")
				}
				if err := update(ctx, orders.Update{AvailableJobs: 7, TargetRunners: 2, DemandObservedAt: time.Now().UTC()}); err != nil {
					return err
				}
				read("STATUS=Healthy\nREADY=1")
				if snapshot := readStatus("Healthy", "connected"); snapshot.Demand == nil || snapshot.Demand.Available != 7 || snapshot.Demand.TargetRunners != 2 || snapshot.Demand.UpdatedAt.IsZero() {
					t.Fatalf("initial job statistics not published: %+v", snapshot.Demand)
				}
				if err := update(ctx, orders.Update{RunningJobs: 1, DemandObservedAt: time.Now().UTC()}); err != nil {
					return err
				}
				if snapshot := readStatus("Healthy", "connected"); snapshot.Demand == nil || snapshot.Demand.Running != 1 || snapshot.Demand.Available != 0 {
					t.Fatalf("job statistics not refreshed: %+v", snapshot.Demand)
				}
				cancel()
				return ctx.Err()
			})
		if err != nil {
			t.Fatal(err)
		}
		read("STATUS=Stopping\nSTOPPING=1")
		if _, err := os.Lstat(opts.StatusSocket); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status socket not removed on exit: %v", err)
		}
		failure := errors.New("GitHub unavailable")
		err = run(t.Context(), opts, slog.New(slog.DiscardHandler),
			func(context.Context, string, image.Config, *slog.Logger) (string, func() error, error) {
				return "image", func() error { return nil }, nil
			}, func(context.Context, orders.Config, *slog.Logger, func(context.Context, orders.Update) error) error {
				return failure
			})
		if !errors.Is(err, failure) {
			t.Fatalf("expected GitHub connection failure: %v", err)
		}
		read("STATUS=Starting")
		read("STATUS=PreparingImage")
		read("STATUS=Connecting")
		read("STATUS=Degraded during Connecting")
	})
}

func TestExecutionRejectsTestClaim(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	example, err := os.ReadFile("../../configs/ephemera.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := bytes.Replace(example, []byte("execute: false"), []byte("execute: true"), 1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), Options{ConfigPath: path, Claim: true}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "runner.execute cannot be combined with --claim") {
		t.Fatalf("expected conflicting intake modes to be rejected, got %v", err)
	}
}
