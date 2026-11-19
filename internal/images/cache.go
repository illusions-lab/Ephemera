package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type Image struct {
	Path string
	info os.FileInfo
}

// Check detects a missing or replaced disk before it is handed to a runner.
func (i Image) Check() error {
	if i.info == nil {
		return errors.New("image has not been prepared")
	}
	info, err := os.Lstat(i.Path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(i.info, info) ||
		info.Size() != i.info.Size() || !info.ModTime().Equal(i.info.ModTime()) {
		return fmt.Errorf("prepared image changed: %s", i.Path)
	}
	return nil
}

type cache struct {
	root   string
	client *http.Client
	logger *slog.Logger
}

type manifest struct {
	SHA256 string `json:"sha256"`
}

func (c cache) prepare(ctx context.Context, key string, build func(context.Context, string) error) (Image, error) {
	if err := os.MkdirAll(c.root, 0750); err != nil {
		return Image{}, err
	}
	lock, err := lockCache(ctx, filepath.Join(c.root, key+".lock"))
	if err != nil {
		return Image{}, err
	}
	defer lock.Close()

	target := filepath.Join(c.root, key)
	if _, err := os.Lstat(target); err == nil {
		return verifiedImage(ctx, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Image{}, err
	}
	work, err := os.MkdirTemp(c.root, ".prepare-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(work)
	if err := build(ctx, work); err != nil {
		return Image{}, err
	}
	disk := filepath.Join(work, "image.qcow2")
	digest, err := hashFile(ctx, disk)
	if err != nil {
		return Image{}, err
	}
	data, err := json.Marshal(manifest{SHA256: digest})
	if err != nil {
		return Image{}, err
	}
	if err := os.WriteFile(filepath.Join(work, "manifest.json"), data, 0444); err != nil {
		return Image{}, err
	}
	if err := os.Chmod(disk, 0444); err != nil {
		return Image{}, err
	}
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	if err := os.Rename(work, target); err != nil {
		return Image{}, err
	}
	return cachedDisk(target)
}

func verifiedImage(ctx context.Context, dir string) (Image, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Image{}, fmt.Errorf("read image manifest: %w", err)
	}
	var record manifest
	if err := json.Unmarshal(data, &record); err != nil || !validSHA256(record.SHA256) {
		return Image{}, fmt.Errorf("invalid image manifest in %s", dir)
	}
	image, err := cachedDisk(dir)
	if err != nil {
		return Image{}, err
	}
	digest, err := hashFile(ctx, image.Path)
	if err != nil {
		return Image{}, err
	}
	if digest != record.SHA256 {
		return Image{}, fmt.Errorf("cached image SHA-256 mismatch: %s", image.Path)
	}
	return image, image.Check()
}

func cachedDisk(dir string) (Image, error) {
	path := filepath.Join(dir, "image.qcow2")
	info, err := os.Lstat(path)
	if err != nil {
		return Image{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Image{}, fmt.Errorf("invalid cached disk: %s", path)
	}
	return Image{Path: path, info: info}, nil
}

func lockCache(ctx context.Context, path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			file.Close()
			return nil, fmt.Errorf("lock image cache: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c cache) download(ctx context.Context, location, expected string) (string, error) {
	dir := filepath.Join(c.root, "downloads")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, expected)
	if digest, err := hashFile(ctx, path); err == nil && digest == expected {
		return path, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download from %s: HTTP %d", req.URL.Hostname(), resp.StatusCode)
	}
	file, err := os.CreateTemp(dir, ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), resp.Body)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return "", fmt.Errorf("download from %s: SHA-256 mismatch", req.URL.Hostname())
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func hashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buf := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buf)
		hash.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(hash.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}
