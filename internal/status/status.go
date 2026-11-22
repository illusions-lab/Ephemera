// Package status exposes the daemon's read-only runtime state over a Unix socket.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const SocketPath = "/run/ephemera/status.sock"

type ImageUpdate struct {
	Enabled        bool      `json:"enabled"`
	Current        string    `json:"current"`
	Previous       string    `json:"previous,omitempty"`
	LastCheck      time.Time `json:"last_check"`
	RollbackReason string    `json:"rollback_reason,omitempty"`
	RolledBack     bool      `json:"rolled_back,omitempty"`
}

type Snapshot struct {
	InstanceRows  []Instance   `json:"-"`
	ConfigDigest  string       `json:"config_digest,omitempty"`
	RunnerGroup   string       `json:"runner_group,omitempty"`
	Labels        *[]string    `json:"labels,omitempty"`
	ImageUpdate   *ImageUpdate `json:"image_update,omitempty"`
	ActivationID  string       `json:"activation_id,omitempty"`
	Phase         string       `json:"phase"`
	FailedPhase   string       `json:"failed_phase,omitempty"`
	GitHub        string       `json:"github"`
	Scope         string       `json:"scope,omitempty"`
	ScaleSet      string       `json:"scale_set,omitempty"`
	Backend       string       `json:"backend,omitempty"`
	Instances     *Instances   `json:"instances,omitempty"`
	Intake        string       `json:"intake"`
	MaxConcurrent int          `json:"max_concurrent,omitempty"`
	Demand        *Demand      `json:"demand,omitempty"`
}

type Instance struct {
	ID        string    `json:"id"`
	Runner    string    `json:"runner"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

type Capacity struct {
	Startable          int     `json:"startable"`
	AvailableMemoryMiB *uint64 `json:"available_memory_mib,omitempty"`
	ReserveMiB         uint64  `json:"reserve_mib"`
	VMMemoryMiB        uint64  `json:"vm_memory_mib"`
}

type Instances struct {
	Capacity              *Capacity `json:"capacity,omitempty"`
	CapacityReason        string    `json:"capacity_reason,omitempty"`
	Draining              bool      `json:"draining,omitempty"`
	Starting              int       `json:"starting"`
	Waiting               int       `json:"waiting"`
	Running               int       `json:"running"`
	Reclaiming            int       `json:"reclaiming"`
	PendingDeregistration int       `json:"pending_deregistration,omitempty"`
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
	mux.HandleFunc("GET /v1/instances", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		rows := snapshot().InstanceRows
		if rows == nil {
			rows = []Instance{}
		}
		_ = json.NewEncoder(w).Encode(rows)
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
	response, err := readEndpoint(ctx, path, "/v1/status")
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

func ReadInstances(ctx context.Context, path string) ([]Instance, error) {
	response, err := readEndpoint(ctx, path, "/v1/instances")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, errors.New("running daemon does not support ephe ps; upgrade and restart it")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instances endpoint returned HTTP %d", response.StatusCode)
	}
	var instances []Instance
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&instances); err != nil {
		return nil, fmt.Errorf("decode daemon instances: %w", err)
	}
	if instances == nil {
		return nil, errors.New("daemon returned an invalid instance list")
	}
	return instances, nil
}

func readEndpoint(ctx context.Context, path, endpoint string) (*http.Response, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+endpoint, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

// RemoveStale is called while holding the private instance directory lock.
func RemoveStale(path, root string) error {
	if filepath.Dir(path) != root {
		return errors.New("status socket must be directly inside the private instance directory")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("status path exists and is not a Unix socket")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return errors.New("status socket already has an active listener")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	return os.Remove(path)
}
