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
		err = run(ctx, opts, slog.New(slog.DiscardHandler),
			func(_ context.Context, cache string, cfg image.Config, _ *slog.Logger) (string, func() error, error) {
				if cache != dir || cfg.Platform.String() != "ubuntu/amd64" {
					t.Fatalf("unexpected image request: %s, %+v", cache, cfg)
				}
				read("STATUS=Starting")
				read("STATUS=PreparingImage")
				return "image", func() error { return nil }, nil
			}, func(ctx context.Context, cfg orders.Config, _ *slog.Logger, update func(context.Context, orders.Update) error) error {
				read("STATUS=Connecting")
				if cfg.Claim {
					t.Fatal("default mode must observe")
				}
				if err := update(ctx, orders.Update{}); err != nil {
					return err
				}
				read("STATUS=Healthy\nREADY=1")
				cancel()
				return ctx.Err()
			})
		if err != nil {
			t.Fatal(err)
		}
		read("STATUS=Stopping\nSTOPPING=1")
	})
}
