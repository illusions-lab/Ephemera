// Package images assembles and caches validated runner VM disks.
package images

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type Platform struct {
	Tag  string
	Arch string
}

func (p Platform) String() string { return p.Tag + "/" + p.Arch }

func (p Platform) Validate() error {
	if p.Tag != "ubuntu" && p.Tag != "windows" {
		return errors.New("image tag must be ubuntu or windows")
	}
	if p.Arch != "amd64" && p.Arch != "arm64" {
		return errors.New("image architecture must be amd64 or arm64")
	}
	return nil
}

type Source struct {
	BaseURL       string `yaml:"base_url"`
	BaseSHA256    string `yaml:"base_sha256"`
	RunnerVersion string `yaml:"runner_version"`
	RunnerSHA256  string `yaml:"runner_sha256"`
}

func DefaultSource(p Platform) (Source, error) {
	if !supports(p) {
		return Source{}, fmt.Errorf("no verified image recipe for %s", p)
	}
	return Source{
		BaseURL:       "https://cloud-images.ubuntu.com/releases/noble/release-20260826/ubuntu-24.04-server-cloudimg-amd64.img",
		BaseSHA256:    "d0fe84bb5f80853425fa6be28e2c106f30104c3cfe8611933f2e65c9b63f0e30",
		RunnerVersion: "2.338.2",
		RunnerSHA256:  "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40",
	}, nil
}

func supports(p Platform) bool { return p.Tag == "ubuntu" && p.Arch == "amd64" }

var runnerVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func (s Source) Validate() error {
	base, err := url.Parse(s.BaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Path == "" {
		return errors.New("base_url must be an HTTPS image URL without credentials, query or fragment")
	}
	if !validSHA256(s.BaseSHA256) || !validSHA256(s.RunnerSHA256) {
		return errors.New("base_sha256 and runner_sha256 must each contain 64 hexadecimal characters")
	}
	if !runnerVersion.MatchString(s.RunnerVersion) {
		return errors.New("runner_version must be a numeric release version, for example 2.338.2")
	}
	return nil
}

func (s Source) runnerURL() string {
	return fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-linux-x64-%s.tar.gz",
		s.RunnerVersion, s.RunnerVersion)
}

func (s Source) normalized() Source {
	s.BaseSHA256 = strings.ToLower(s.BaseSHA256)
	s.RunnerSHA256 = strings.ToLower(s.RunnerSHA256)
	return s
}

func validSHA256(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}
