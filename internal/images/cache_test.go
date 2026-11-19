package images

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCachePublishesOnlySuccessfulBuildAndChecksReuse(t *testing.T) {
	c := cache{root: t.TempDir()}
	ctx := context.Background()
	builds := 0
	build := func(_ context.Context, work string) error {
		builds++
		if err := os.WriteFile(filepath.Join(work, "image.qcow2"), []byte("disk"), 0600); err != nil {
			return err
		}
		if builds == 1 {
			return errors.New("guest did not boot")
		}
		return nil
	}
	if _, err := c.prepare(ctx, "image", build); err == nil {
		t.Fatal("failed boot should prevent publication")
	}
	if _, err := os.Stat(filepath.Join(c.root, "image")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image published after failed build: %v", err)
	}
	image, err := c.prepare(ctx, "image", build)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.prepare(ctx, "image", build); err != nil || builds != 2 {
		t.Fatalf("expected cache reuse, builds = %d, error = %v", builds, err)
	}
	if err := os.Chmod(image.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image.Path, []byte("corrupted disk"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := image.Check(); err == nil {
		t.Fatal("expected runtime check to detect a modified disk")
	}
	if _, err := c.prepare(ctx, "image", build); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("cache should reject corrupted disk, got %v", err)
	}
}

func TestDownloadVerifiesAndReusesContent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "fixture")
	}))
	defer server.Close()
	c := cache{root: t.TempDir(), client: server.Client()}
	ctx := context.Background()
	bad := strings.Repeat("0", 64)
	if _, err := c.download(ctx, server.URL, bad); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("expected checksum error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.root, "downloads", bad)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected cache entry after checksum failure: %v", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture")))
	for range 2 {
		if _, err := c.download(ctx, server.URL, digest); err != nil {
			t.Fatal(err)
		}
	}
	if count := calls.Load(); count != 2 {
		t.Fatalf("download count = %d, want 2 including failed verification", count)
	}
}
