package status

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadInstances(t *testing.T) {
	root, err := os.MkdirTemp("", "ephe-instance-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "new.sock")
	closeStatus, err := Listen(socket, func() Snapshot { return Snapshot{Phase: "Healthy"} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeStatus() })
	rows, err := ReadInstances(t.Context(), socket)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty daemon: rows=%v, err=%v", rows, err)
	}
	oldSocket := filepath.Join(root, "old.sock")
	old, err := net.Listen("unix", oldSocket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = server.Serve(old) }()
	t.Cleanup(func() { _ = old.Close(); _ = server.Close() })
	if _, err := ReadInstances(t.Context(), oldSocket); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("legacy daemon: %v", err)
	}
	if _, err := ReadInstances(t.Context(), filepath.Join(root, "missing.sock")); err == nil {
		t.Fatal("offline daemon returned success")
	}
}
