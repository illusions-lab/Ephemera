package image

import (
	"strings"
	"testing"
)

func TestUbuntuSourcesMatchGuestArchitecture(t *testing.T) {
	for arch, archive := range map[string]string{"amd64": "x64", "arm64": "arm64"} {
		t.Run(arch, func(t *testing.T) {
			source, err := DefaultSource(Platform{Tag: "ubuntu", Arch: arch})
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Validate(); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(source.BaseURL, "-"+arch+".img") ||
				!strings.Contains(source.runnerURL(arch), "linux-"+archive+"-") {
				t.Fatalf("unexpected image or runner for %s", arch)
			}
		})
	}
}
