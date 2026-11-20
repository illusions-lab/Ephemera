// Package status exposes the daemon's read-only runtime state over a Unix socket.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const SocketPath = "/run/ephemera/status.sock"

type Snapshot struct {
	Phase         string     `json:"phase"`
	FailedPhase   string     `json:"failed_phase,omitempty"`
	GitHub        string     `json:"github"`
	Scope         string     `json:"scope,omitempty"`
	ScaleSet      string     `json:"scale_set,omitempty"`
	Backend       string     `json:"backend,omitempty"`
	Instances     *Instances `json:"instances,omitempty"`
	Intake        string     `json:"intake"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	Demand        *Demand    `json:"demand,omitempty"`
}

type Instances struct {
	Starting   int `json:"starting"`
	Waiting    int `json:"waiting"`
	Running    int `json:"running"`
	Reclaiming int `json:"reclaiming"`
}

type Demand struct {
	Available     int       `json:"available"`
	Assigned      int       `json:"assigned"`
	Running       int       `json:"running"`
	TargetRunners int       `json:"target_runners"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func Listen(path string, snapshot func() Snapshot) (func() error, error) {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on status socket: %w", err)
	}
	// The installed unit owns the socket directory. Local users can query
	// status, and the protocol exposes no operations or credential data.
	if err := os.Chmod(path, 0666); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set status socket permissions: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(snapshot())
	})
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096,
	}
	go func() { _ = server.Serve(listener) }()
	return func() error {
		// Close the listener even if shutdown races with Serve starting.
		err := server.Close()
		_ = listener.Close()
		return err
	}, nil
}

func Read(ctx context.Context, path string) (Snapshot, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/v1/status", nil)
	if err != nil {
		return Snapshot{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Snapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("status socket returned HTTP %d", response.StatusCode)
	}
	var snapshot Snapshot
	if err := json.NewDecoder(io.LimitReader(response.Body, 32768)).Decode(&snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode daemon status: %w", err)
	}
	if snapshot.Phase == "" {
		return Snapshot{}, fmt.Errorf("daemon status is missing its phase")
	}
	return snapshot, nil
}
