package status

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveStalePreservesLiveSocketAndRegularFile(t *testing.T) {
	root, err := os.MkdirTemp("", "ephe-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "status.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = listener.Close() })
	if err := RemoveStale(path, root); err == nil {
		t.Fatal("removed live listener")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStale(path, root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStale(path, root); err == nil {
		t.Fatal("removed regular file")
	}
}
