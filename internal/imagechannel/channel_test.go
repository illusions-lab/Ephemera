package imagechannel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchManifest(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"valid", `{"schema":1,"images":{}}`, 200, true},
		{"unsupported schema", `{"schema":2,"images":{}}`, 200, false},
		{"malformed", `{"schema":1}` + "x", 200, false},
		{"oversized", strings.Repeat(" ", 65537), 200, false},
		{"network failure", "", 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			_, err := Fetch(t.Context(), server.URL)
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

func TestRecipeRequiresCompatibleVersionAndPinnedArtifacts(t *testing.T) {
	sha := strings.Repeat("a", 64)
	recipe := Recipe{Revision: 1, MinVersion: "v0.2.0", TartRef: "ghcr.io/cirruslabs/macos-tahoe-base@sha256:" + sha, RunnerVersion: "2.338.2", RunnerSHA256: sha}
	if err := recipe.Validate("darwin/arm64", "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := recipe.Validate("darwin/arm64", "v0.1.0"); err == nil {
		t.Fatal("accepted incompatible daemon")
	}
	if err := recipe.Validate("linux/amd64", "v0.2.0"); err == nil {
		t.Fatal("accepted wrong platform")
	}
	recipe.RunnerSHA256 = "broken"
	if err := recipe.Validate("darwin/arm64", "v0.2.0"); err == nil {
		t.Fatal("accepted invalid checksum")
	}
}
