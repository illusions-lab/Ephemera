package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuickTunnelStopsWithSetup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test uses a local fixture readable by the current user")
	}
	if _, err := os.Stat(installedCloudflared); !os.IsNotExist(err) {
		t.Skip("an installed Ephemera cloudflared takes priority over the fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "cloudflared")
	script := "#!/bin/sh\nprintf '%s\\n' 'https://ephemera-fixture.trycloudflare.com' >&2\nexec /bin/sleep 60\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tunnel, err := startQuickTunnel(ctx, "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.close()
	if got := tunnel.address.String(); got != "https://ephemera-fixture.trycloudflare.com" {
		t.Fatalf("unexpected tunnel address %q", got)
	}
	cancel()
	select {
	case <-tunnel.done:
	case <-time.After(2 * time.Second):
		t.Fatal("cloudflared was not reaped after canceling setup")
	}
}
