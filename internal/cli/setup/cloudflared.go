package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const cloudflaredVersion = "2026.11.1"
const maxCloudflaredSize = 64 << 20

type cloudflaredRelease struct {
	asset, digest string
}

var cloudflaredReleases = map[string]cloudflaredRelease{
	"linux/amd64":  {"cloudflared-linux-amd64", "97bb3b02d23a5980fc9c15a1a270e692da1923f103f920d5b16d4931d5ab55e7"},
	"linux/arm64":  {"cloudflared-linux-arm64", "b155cec3e48e489cfb8e5c602769fb8c771083018d8881d68b4d18a791567c5a"},
	"darwin/arm64": {"cloudflared-darwin-arm64.tgz", "02fe4bcf08f2c09699cff30658228a739a0c47fb75f3653d1b8772a27ade4462"},
}

func findCloudflared(ctx context.Context) (string, func(), error) {
	noop := func() {}
	if runtime.GOOS == "linux" {
		if binary, err := exec.LookPath(installedCloudflared); err == nil {
			return binary, noop, nil
		}
	}
	if binary, err := exec.LookPath("cloudflared"); err == nil {
		return binary, noop, nil
	}
	if runtime.GOOS == "darwin" {
		if binary, err := exec.LookPath("/opt/homebrew/bin/cloudflared"); err == nil {
			return binary, noop, nil
		}
	}

	release, ok := cloudflaredReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", noop, errors.New("no cloudflared download for this platform")
	}
	fmt.Println("Downloading cloudflared for this setup…")
	binary, err := downloadCloudflared(ctx, release)
	if err != nil {
		return "", noop, err
	}
	// Linux setup can launch cloudflared as nobody. Use a traversable
	// directory that only the installer can modify.
	directory, err := os.MkdirTemp("/tmp", "ephemera-cloudflared-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, "cloudflared")
	if err := os.WriteFile(path, binary, 0755); err != nil {
		cleanup()
		return "", noop, err
	}
	if os.Geteuid() == 0 {
		if err := os.Chmod(path, 0755); err != nil {
			cleanup()
			return "", noop, err
		}
		if err := os.Chmod(directory, 0711); err != nil {
			cleanup()
			return "", noop, err
		}
	}
	return path, cleanup, nil
}

func downloadCloudflared(ctx context.Context, release cloudflaredRelease) ([]byte, error) {
	address := "https://github.com/cloudflare/cloudflared/releases/download/" + cloudflaredVersion + "/" + release.asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 45 * time.Second,
		CheckRedirect: func(req *http.Request, previous []*http.Request) error {
			if req.URL.Scheme != "https" || len(previous) >= 10 {
				return errors.New("refusing cloudflared download redirect")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download cloudflared: HTTP %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudflaredSize+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxCloudflaredSize || fmt.Sprintf("%x", sha256.Sum256(payload)) != release.digest {
		return nil, errors.New("cloudflared download did not match the pinned SHA-256")
	}
	if filepath.Ext(release.asset) != ".tgz" {
		return payload, nil
	}
	archive, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	reader := tar.NewReader(archive)
	header, err := reader.Next()
	if err != nil {
		return nil, err
	}
	if header.Name != "cloudflared" || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > maxCloudflaredSize {
		return nil, errors.New("invalid cloudflared archive")
	}
	return io.ReadAll(reader)
}
