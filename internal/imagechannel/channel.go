// Package imagechannel reads approved runner image recipes.
package imagechannel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/image"
	"golang.org/x/mod/semver"
)

const URL = "https://raw.githubusercontent.com/illusions-lab/Ephemera/image-channel/stable.json"

type Manifest struct {
	Schema int               `json:"schema"`
	Images map[string]Recipe `json:"images"`
}

type Recipe struct {
	Revision      uint64        `json:"revision"`
	MinVersion    string        `json:"min_version"`
	Ubuntu        *image.Source `json:"ubuntu,omitempty"`
	TartRef       string        `json:"tart_ref,omitempty"`
	RunnerVersion string        `json:"runner_version,omitempty"`
	RunnerSHA256  string        `json:"runner_sha256,omitempty"`
}

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var runner = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func (r Recipe) Validate(platform, version string) error {
	if r.Revision == 0 || !semver.IsValid(r.MinVersion) || !semver.IsValid(version) || semver.Compare(version, r.MinVersion) < 0 {
		return errors.New("invalid or incompatible image revision")
	}
	switch platform {
	case "linux/amd64", "linux/arm64":
		if r.Ubuntu == nil || r.TartRef != "" || r.RunnerVersion != "" || r.RunnerSHA256 != "" || r.Ubuntu.Snapshot == "" {
			return errors.New("invalid Ubuntu image recipe")
		}
		u, err := url.Parse(r.Ubuntu.BaseURL)
		if err != nil || u.Host != "cloud-images.ubuntu.com" || !strings.HasPrefix(u.Path, "/releases/noble/") || !strings.HasSuffix(u.Path, "-"+strings.TrimPrefix(platform, "linux/")+".img") {
			return errors.New("unrecognized Ubuntu base image")
		}
		return r.Ubuntu.Validate()
	case "darwin/arm64":
		value, ok := strings.CutPrefix(r.TartRef, "ghcr.io/cirruslabs/macos-tahoe-base@sha256:")
		if r.Ubuntu != nil || !ok || !digest.MatchString(value) || !digest.MatchString(r.RunnerSHA256) || !runner.MatchString(r.RunnerVersion) {
			return errors.New("invalid Tart image recipe")
		}
		return nil
	}
	return errors.New("unsupported image platform")
}

func Fetch(ctx context.Context, endpoint string) (Manifest, error) {
	var result Manifest
	client := http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, err
	}
	response, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("image channel HTTP %d", response.StatusCode)
	}
	return Decode(response.Body)
}

func Decode(body io.Reader) (Manifest, error) {
	var result Manifest
	data, err := io.ReadAll(io.LimitReader(body, 65537))
	if err != nil {
		return result, err
	}
	if len(data) > 65536 {
		return result, errors.New("image manifest exceeds size limit")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if result.Schema != 1 {
		return result, errors.New("unsupported image manifest schema")
	}
	return result, nil
}
