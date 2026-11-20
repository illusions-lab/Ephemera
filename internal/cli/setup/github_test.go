package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type githubTestTransport func(*http.Request) (*http.Response, error)

func (f githubTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestBrowserGitHubAppSetup(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}))
	registration, err := json.Marshal(githubApp{ID: 123, Slug: "ephemera-test", PEM: key})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: githubTestTransport(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, ""
		switch r.Method + " " + r.URL.Path {
		case "POST /app-manifests/test-code/conversions":
			body = string(registration)
		case "GET /orgs/example/installation":
			token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) {
				return &privateKey.PublicKey, nil
			}, jwt.WithValidMethods([]string{"RS256"}))
			if err != nil || !token.Valid || token.Claims.(jwt.MapClaims)["iss"] != "123" {
				t.Error("GitHub installation lookup used an invalid App JWT")
			}
			body = `{"id":456,"permissions":{"organization_self_hosted_runners":"write"}}`
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan [2]string, 1)
	type outcome struct {
		connection githubConnection
		err        error
	}
	done := make(chan outcome, 1)
	go func() {
		openTunnel := func(context.Context, string) (*quickTunnel, error) {
			stopped := make(chan struct{})
			return &quickTunnel{
				address: &url.URL{Scheme: "https", Host: "ephemera-test.trycloudflare.com"},
				done:    stopped,
				stop:    func() { close(stopped) },
			}, nil
		}
		connection, err := waitForGitHubApp(ctx, "ephemera-ubuntu-amd64", "", true, client, openTunnel, func(_, localURL, publicURL string) {
			start <- [2]string{localURL, publicURL}
		})
		done <- outcome{connection, err}
	}()

	var localURL, publicURL string
	select {
	case addresses := <-start:
		localURL, publicURL = addresses[0], addresses[1]
	case result := <-done:
		t.Fatalf("browser setup did not start: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	localOrigin, err := url.Parse(localURL)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{
		Timeout: 3 * time.Second,
		Transport: githubTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Scheme == "https" {
				r = r.Clone(r.Context())
				r.Host = r.URL.Host
				r.URL.Scheme, r.URL.Host = localOrigin.Scheme, localOrigin.Host
			}
			return http.DefaultTransport.RoundTrip(r)
		}),
	}
	request := func(method, address string, status int, text string, forms ...url.Values) string {
		t.Helper()
		var form io.Reader
		if len(forms) > 0 {
			form = strings.NewReader(forms[0].Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, address, form)
		if err != nil {
			t.Fatal(err)
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || !strings.Contains(string(body), text) {
			t.Fatalf("expected HTTP %d and %q, got HTTP %d (%v)", status, text, response.StatusCode, err)
		}
		return string(body)
	}
	readManifest := func(page string) githubManifest {
		t.Helper()
		field := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindStringSubmatch(page)
		if len(field) != 2 {
			t.Fatal("setup page is missing the manifest form field")
		}
		var manifest githubManifest
		if err := json.Unmarshal([]byte(html.UnescapeString(field[1])), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.URL == "" || manifest.Webhook.URL == "" || manifest.Webhook.Active {
			t.Fatal("manifest must include both URLs and disable webhook deliveries")
		}
		return manifest
	}
	initialPage := request(http.MethodGet, publicURL, http.StatusOK, "A full GitHub URL also works.")
	for _, image := range []string{"github-organization.png", "github-repository.png"} {
		if !strings.Contains(initialPage, "/examples/"+image) {
			t.Fatalf("setup page is missing %s", image)
		}
		request(http.MethodGet, publicURL+"/examples/"+image, http.StatusOK, "\x89PNG\r\n\x1a\n")
	}
	if !strings.Contains(initialPage, `<li aria-current="step">Choose the GitHub target</li>`) ||
		!strings.Contains(initialPage, "<li>Confirm service startup in the terminal</li>") {
		t.Fatal("system setup should list all steps and mark the first step current")
	}
	generatedName := regexp.MustCompile(`name="app_name"[^>]+value="(Ephemera [^"]+ [0-9]{4})"`).FindStringSubmatch(initialPage)
	if len(generatedName) != 2 {
		t.Fatal("setup page did not prefill a generated App name")
	}
	appName := html.UnescapeString(generatedName[1])
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		prefix := strings.TrimSuffix(hostGitHubAppName(hostname, 0), "0000")
		if !strings.HasPrefix(appName, prefix) {
			t.Fatalf("expected host-based App name, got %q", appName)
		}
	}
	request(http.MethodPost, publicURL+"/target", http.StatusUnprocessableEntity, "Enter a github.com URL", url.Values{"scope": {"https://other.example/org"}})
	request(http.MethodPost, publicURL+"/target", http.StatusUnprocessableEntity, "GitHub App name must be at most 34 characters", url.Values{"scope": {"example"}, "app_name": {strings.Repeat("a", 35)}})
	defaultPage := request(http.MethodPost, publicURL+"/target", http.StatusOK, "2. Create the App on GitHub", url.Values{"scope": {"example"}})
	if got := readManifest(defaultPage).Name; got != appName {
		t.Fatalf("expected the generated App name in the manifest, got %q", got)
	}
	request(http.MethodPost, publicURL+"/target", http.StatusOK, "Create App in example", url.Values{
		"scope":    {"example"},
		"scale":    {"ephemera-custom"},
		"app_name": {"  My Ephemera Test App  "},
	})
	localPage := request(http.MethodGet, localURL, http.StatusOK, "organization_self_hosted_runners")
	if callback := readManifest(localPage).RedirectURL; callback != localURL+"/callback" {
		t.Fatalf("expected loopback callback for SSH fallback, got %q", callback)
	}
	page := request(http.MethodGet, publicURL, http.StatusOK, "organization_self_hosted_runners")
	manifest := readManifest(page)
	if manifest.Name != "My Ephemera Test App" {
		t.Fatalf("expected submitted App name, got %q", manifest.Name)
	}
	if callback := manifest.RedirectURL; callback != publicURL+"/callback" {
		t.Fatalf("expected public callback for Cloudflare, got %q", callback)
	}
	request(http.MethodGet, publicURL+"/callback?code=test-code&state=wrong", http.StatusBadRequest, "Invalid GitHub callback")
	state := regexp.MustCompile(`[?&]state=([0-9a-f]+)`).FindStringSubmatch(html.UnescapeString(page))
	if len(state) != 2 {
		t.Fatal("setup page is missing the GitHub state parameter")
	}
	installPage := request(http.MethodGet, publicURL+"/callback?code=test-code&state="+url.QueryEscape(state[1]), http.StatusOK, "https://github.com/apps/ephemera-test/installations/new")
	if !strings.Contains(installPage, "3. Install the App") || !strings.Contains(installPage, "4. Verify the installation") {
		t.Fatal("installation and verification should match the numbered steps")
	}
	if !strings.Contains(installPage, "Step 3 · Install the GitHub App") || !strings.Contains(installPage, "Step 4 · Verify installation") {
		t.Fatal("installation and verification controls should include their step numbers")
	}
	finishPage := request(http.MethodPost, publicURL+"/verify", http.StatusOK, "5. Save the configuration in the terminal")
	if !strings.Contains(finishPage, "6. Confirm service startup in the terminal") {
		t.Fatal("system setup should explain how to start the service after saving")
	}
	select {
	case result := <-done:
		if result.err != nil || result.connection.AppID != 123 || result.connection.InstallationID != 456 || result.connection.Scope != "https://github.com/example" ||
			result.connection.ScaleSet != "ephemera-custom" || string(result.connection.Key) != key {
			t.Fatalf("expected verified GitHub App credentials: %v", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestLocalBrowserSetupSteps(t *testing.T) {
	var page bytes.Buffer
	if err := githubPage.Execute(&page, githubPageData{Step: 5}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "5. Save the configuration in the terminal") || strings.Contains(page.String(), "Confirm service startup") {
		t.Fatal("local setup should end at saving configuration")
	}
}

func TestHostGitHubAppName(t *testing.T) {
	if got := hostGitHubAppName("runner-01", 7); got != "Ephemera runner-01 0007" {
		t.Fatalf("unexpected name for short hostname: %q", got)
	}
	if got := hostGitHubAppName("runner-01-build-box.local", 1234); got != "Ephemera runner-01-build-box 1234" || len(got) > 34 {
		t.Fatalf("unexpected name for long hostname: %q", got)
	}
	if got := hostGitHubAppName(strings.Repeat("a", 40), 9876); got != "Ephemera "+strings.Repeat("a", 20)+" 9876" {
		t.Fatalf("expected a name shortened to the GitHub limit, got %q", got)
	}
}
