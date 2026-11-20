package setup

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	randv2 "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"charm.land/huh/v2"
	"github.com/golang-jwt/jwt/v4"
)

const githubAPI = "https://api.github.com"
const githubAppNameLimit = 34

var githubPathPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

//go:embed examples/github-organization.png examples/github-repository.png
var githubExamples embed.FS

type githubApp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	PEM  string `json:"pem"`
}

type githubConnection struct {
	AppID, InstallationID int64
	Scope, ScaleSet       string
	AppSlug, Permission   string
	Key                   []byte
}

type githubManifest struct {
	Name        string `json:"name,omitempty"`
	URL         string `json:"url"`
	RedirectURL string `json:"redirect_url"`
	Public      bool   `json:"public"`
	Webhook     struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	} `json:"hook_attributes"`
	Permissions map[string]string `json:"default_permissions"`
}

var githubPage = template.Must(template.New("github setup").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect Ephemera to GitHub</title>
<style>
body { font: 1rem/1.55 system-ui, sans-serif; margin: 12vh auto; max-width: 34rem; padding: 0 1.5rem; color-scheme: light dark; }
button, input { font: inherit; padding: .5em .75em; }
button, summary { cursor: pointer; }
.step-action { display: inline-block; box-sizing: border-box; padding: .65em 1em; border: 1px solid transparent; border-radius: .375rem; background: #374151; color: #fff; font: inherit; font-weight: 600; line-height: 1.4; text-align: center; text-decoration: none; }
.step-action:hover { background: #1f2937; }
.step-action:focus-visible { outline: 2px solid currentColor; outline-offset: -4px; }
@media (prefers-color-scheme: dark) {
  .step-action { background: #e5e7eb; color: #111827; }
  .step-action:hover { background: #fff; }
}
label { display: block; margin-top: 1em; }
input { box-sizing: border-box; width: 100%; margin: .25em 0 1em; }
form { margin: 1em 0; }
.help { margin: .25em 0; }
.steps { padding-left: 1.5em; }
.steps [aria-current="step"] { font-weight: 700; }
.examples figure { margin: 0 0 1rem; }
.examples img { display: block; width: 100%; height: auto; border-radius: .5rem; }
.examples figcaption { margin-top: .25rem; }
</style>
<h1>Connect Ephemera to GitHub</h1>
<nav aria-label="Setup steps">
<ol class="steps">
<li{{if eq .Step 1}} aria-current="step"{{end}}>Choose the GitHub target</li>
<li{{if eq .Step 2}} aria-current="step"{{end}}>Create the App on GitHub</li>
<li{{if eq .Step 3}} aria-current="step"{{end}}>Install the App</li>
<li{{if eq .Step 4}} aria-current="step"{{end}}>Verify the installation</li>
<li{{if eq .Step 5}} aria-current="step"{{end}}>Save the configuration in the terminal</li>
{{if .SystemSetup}}<li>Confirm service startup in the terminal</li>{{end}}
</ol>
</nav>
{{if .Target}}<p>Target: <strong>{{.Target}}</strong></p>{{end}}
{{if .Message}}<p role="status">{{.Message}}</p>{{end}}
{{if .InstallURL}}
<h2>3. Install the App</h2>
<p>GitHub will open in a new tab. Select the account that owns the target. If GitHub asks which repositories to grant access to, include the repository you entered above.</p>
<p><a class="step-action" href="{{.InstallURL}}" target="_blank" rel="noopener noreferrer">Step 3 · Install the GitHub App</a></p>
<h2>4. Verify the installation</h2>
<p>After GitHub confirms the installation, return here and verify it. Ephemera checks that the App is installed for the target and has the required permission.</p>
<form method="post" action="{{.CheckURL}}"><button class="step-action" type="submit">Step 4 · Verify installation</button></form>
{{end}}
{{if .CreateURLs}}
<h2>2. Create the App on GitHub</h2>
<p>GitHub will show the prefilled name and ask you to confirm creation. Ephemera requests runner management access for an organization or administration access for a single repository. After creation, you will return here to install the App.</p>
{{end}}
{{range .CreateURLs}}
<form method="post" action="{{.URL}}">
<input type="hidden" name="manifest" value="{{$.Manifest}}">
<button type="submit">{{.Label}}</button>
</form>
{{end}}
{{if .TargetURL}}
{{if .CreateURLs}}
<details{{if eq .Step 1}} open{{end}}><summary>1. Change target or App name</summary>
{{else}}
<h2>1. Choose the GitHub target</h2>
<p>Select where Ephemera should connect to GitHub Actions. This setup creates a private GitHub App for that target.</p>
{{end}}
<form method="post" action="{{.TargetURL}}">
<label for="scope">GitHub organization or repository</label>
<p id="scope-help" class="help">Enter <code>my-org</code> for an organization or <code>owner/my-repo</code> for a single repository. A full GitHub URL also works.</p>
<input id="scope" name="scope" aria-describedby="scope-help" required maxlength="256" placeholder="my-org or owner/my-repo" value="{{.ScopeInput}}">
<div class="examples">
<figure>
<img src="{{.ExamplesURL}}/github-organization.png" width="944" height="410" alt="GitHub address bar with only illusions-lab highlighted, before the repository name">
<figcaption><strong>Organization:</strong> Copy the first part after <code>github.com/</code>. Here it is <code>illusions-lab</code>.</figcaption>
</figure>
<figure>
<img src="{{.ExamplesURL}}/github-repository.png" width="918" height="468" alt="GitHub address bar with Iktahana/Ephemera highlighted">
<figcaption><strong>Single repository:</strong> Copy both the owner and repository name, such as <code>Iktahana/Ephemera</code>.</figcaption>
</figure>
</div>
<label for="app-name">GitHub App name</label>
<p id="app-name-help" class="help">This name appears in GitHub's App settings and can be up to {{.AppNameLimit}} characters. You can keep the suggested name or edit it.</p>
<input id="app-name" name="app_name" aria-describedby="app-name-help" maxlength="{{.AppNameLimit}}" value="{{.AppName}}">
<details>
<summary>Advanced settings</summary>
<label for="scale">Runner Scale Set name</label>
<p id="scale-help" class="help">Workflows use this name in <code>runs-on</code> to target the scale set. Keep the default unless your workflows need a specific name.</p>
<input id="scale" name="scale" aria-describedby="scale-help" maxlength="128" value="{{.ScaleSet}}">
</details>
<p><button type="submit">Continue</button></p>
</form>
{{if .CreateURLs}}</details>{{end}}
{{end}}
{{if eq .Step 5}}
<h2>5. Save the configuration in the terminal</h2>
<p>Return to the terminal where you started setup and confirm <strong>Save this configuration?</strong></p>
{{if .SystemSetup}}
<h2>6. Confirm service startup in the terminal</h2>
<p>After saving, confirm <strong>Apply configuration now?</strong> to start the Ephemera service.</p>
{{end}}
{{end}}
</html>`))

type githubCreateOption struct {
	URL, Label string
}

type githubPageData struct {
	Step, AppNameLimit                                                                        int
	SystemSetup                                                                               bool
	ExamplesURL                                                                               string
	Target, TargetURL, ScopeInput, ScaleSet, AppName, Message, InstallURL, CheckURL, Manifest string
	CreateURLs                                                                                []githubCreateOption
}

func createGitHubApp(scaleSet, appName string, systemSetup bool) (githubConnection, error) {
	useTunnel := systemSetup && !browserOnThisHost()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	wait, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var openTunnel func(context.Context, string) (*quickTunnel, error)
	if useTunnel {
		openTunnel = func(ctx context.Context, address string) (*quickTunnel, error) {
			fmt.Println("Starting Cloudflare Quick Tunnel…")
			tunnel, err := startQuickTunnel(ctx, address)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				fmt.Printf("Cloudflare Quick Tunnel unavailable (%v). Continuing with local browser or SSH forwarding.\n", err)
			}
			return tunnel, nil
		}
	}
	connection, err := waitForGitHubApp(wait, scaleSet, appName, systemSetup, client, openTunnel, func(address, localURL, publicURL string) {
		showBrowserSetup(wait, address, localURL, publicURL)
		if publicURL == "" && !useTunnel {
			openSetupBrowser(wait, localURL)
		}
	})
	if errors.Is(err, context.Canceled) {
		return githubConnection{}, huh.ErrUserAborted
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return githubConnection{}, errors.New("GitHub App setup expired")
	}
	return connection, err
}

func browserOnThisHost() bool {
	if os.Geteuid() == 0 || os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	return runtime.GOOS == "darwin" || (runtime.GOOS == "linux" &&
		(os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""))
}

func openSetupBrowser(ctx context.Context, address string) {
	if !browserOnThisHost() {
		return
	}
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, command, address).Run()
	}()
}

type githubTarget struct {
	scope, scaleSet, appName, permission, apiPath string
	options                                       []githubCreateOption
}

func selectGitHubTarget(scope, scaleSet, appName, state string) (githubTarget, error) {
	scope = strings.TrimSpace(scope)
	if !strings.Contains(scope, "://") {
		scope = "https://github.com/" + scope
	}
	target, err := url.Parse(scope)
	if err != nil || target.Scheme != "https" || !strings.EqualFold(target.Host, "github.com") ||
		target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return githubTarget{}, errors.New("Enter a github.com URL, organization name, or owner/repository.")
	}
	parts := strings.Split(strings.Trim(target.Path, "/"), "/")
	if len(parts) > 2 {
		return githubTarget{}, errors.New("Enter a GitHub organization or repository.")
	}
	for _, part := range parts {
		if !githubPathPart.MatchString(part) || part == "." || part == ".." {
			return githubTarget{}, errors.New("Enter a GitHub organization or repository.")
		}
	}
	owner := parts[0]
	selected := githubTarget{
		scope: "https://github.com/" + strings.Join(parts, "/"), scaleSet: scaleSet, appName: appName,
		permission: "organization_self_hosted_runners", apiPath: "/orgs/" + owner + "/installation",
		options: []githubCreateOption{{
			URL:   "https://github.com/organizations/" + owner + "/settings/apps/new?state=" + state,
			Label: "Create App in " + owner,
		}},
	}
	if len(parts) == 2 {
		selected.permission = "administration"
		selected.apiPath = "/repos/" + owner + "/" + parts[1] + "/installation"
		selected.options = append(selected.options, githubCreateOption{
			URL:   "https://github.com/settings/apps/new?state=" + state,
			Label: "Create App in my personal account",
		})
	}
	return selected, nil
}

func truncateGitHubAppName(name string, limit int) string {
	length := 0
	for i, r := range name {
		length += utf16.RuneLen(r)
		if length > limit {
			return name[:i]
		}
	}
	return name
}

func hostGitHubAppName(hostname string, number int) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		hostname = strings.ToLower(rand.Text()[:10])
	}
	name := "Ephemera " + hostname
	suffix := fmt.Sprintf(" %04d", number)
	limit := githubAppNameLimit - len(suffix)
	if truncateGitHubAppName(name, limit) != name {
		name = "Ephemera " + strings.TrimSuffix(hostname, ".local")
	}
	return strings.TrimSpace(truncateGitHubAppName(name, limit)) + suffix
}

func waitForGitHubApp(ctx context.Context, defaultScaleSet, defaultAppName string, systemSetup bool, client *http.Client, openTunnel func(context.Context, string) (*quickTunnel, error), ready func(address, localURL, publicURL string)) (githubConnection, error) {
	defaultAppName = strings.TrimSpace(defaultAppName)
	if defaultAppName == "" {
		hostname, _ := os.Hostname()
		defaultAppName = hostGitHubAppName(hostname, randv2.IntN(10000))
	} else if truncateGitHubAppName(defaultAppName, githubAppNameLimit) != defaultAppName {
		return githubConnection{}, fmt.Errorf("--app-name must be at most %d characters", githubAppNameLimit)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return githubConnection{}, fmt.Errorf("start browser setup: %w", err)
	}
	defer listener.Close()

	address := listener.Addr().String()
	localBase := "http://" + address
	baseURL, publicHost := localBase, ""
	if openTunnel != nil {
		tunnel, err := openTunnel(ctx, address)
		if err != nil {
			return githubConnection{}, err
		}
		if tunnel != nil {
			defer tunnel.close()
			baseURL, publicHost = tunnel.address.String(), tunnel.address.Host
		}
	}

	var nonce [32]byte
	rand.Read(nonce[:])
	startPath := "/setup/" + hex.EncodeToString(nonce[:])
	targetPath := startPath + "/target"
	examplesPath := startPath + "/examples"
	callbackPath := startPath + "/callback"
	checkPath := startPath + "/verify"

	var mutex sync.Mutex
	var target githubTarget
	var state string
	var app githubApp
	var tokenKey *rsa.PrivateKey
	var connected bool
	result := make(chan githubConnection, 1)

	render := func(w http.ResponseWriter, r *http.Request, message string) {
		page := githubPageData{Step: 1, Target: target.scope, Message: message, AppNameLimit: githubAppNameLimit, SystemSetup: systemSetup, ExamplesURL: examplesPath}
		switch {
		case connected:
			page.Step = 5
		case app.ID > 0:
			page.Step = 3
			if r.URL.Path == checkPath {
				page.Step = 4
			}
			page.InstallURL = "https://github.com/apps/" + url.PathEscape(app.Slug) + "/installations/new"
			page.CheckURL = checkPath
		default:
			page.TargetURL, page.ScopeInput, page.ScaleSet, page.AppName = targetPath, target.scope, target.scaleSet, target.appName
			if target.scope == "" {
				page.ScaleSet = defaultScaleSet
				page.AppName = defaultAppName
			} else {
				page.Step = 2
			}
			if r.Method == http.MethodPost && r.URL.Path == targetPath {
				page.Step = 1
				page.ScopeInput = r.PostForm.Get("scope")
				page.ScaleSet = r.PostForm.Get("scale")
				page.AppName = r.PostForm.Get("app_name")
			}
		}
		if app.ID == 0 && target.scope != "" {
			callbackBase := localBase
			if r.Host == publicHost {
				callbackBase = baseURL
			}
			manifest := githubManifest{
				Name:        target.appName,
				URL:         "https://github.com/illusions-lab/Ephemera",
				RedirectURL: callbackBase + callbackPath,
				Permissions: map[string]string{target.permission: "write"},
			}
			// GitHub requires the webhook URL even when deliveries are disabled.
			manifest.Webhook.URL = manifest.URL
			data, err := json.Marshal(manifest)
			if err != nil {
				http.Error(w, "Could not build GitHub manifest.", http.StatusInternalServerError)
				return
			}
			page.CreateURLs, page.Manifest = target.options, string(data)
		}
		_ = githubPage.Execute(w, page)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+examplesPath+"/{image}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Del("Content-Type")
		http.ServeFileFS(w, r, githubExamples, "examples/"+r.PathValue("image"))
	})
	mux.HandleFunc("GET "+startPath, func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		render(w, r, "")
	})
	mux.HandleFunc("POST "+targetPath, func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid setup form.", http.StatusBadRequest)
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		if app.ID != 0 {
			http.Redirect(w, r, startPath, http.StatusSeeOther)
			return
		}
		scale := strings.TrimSpace(r.PostForm.Get("scale"))
		if scale == "" {
			scale = defaultScaleSet
		}
		appName := strings.TrimSpace(r.PostForm.Get("app_name"))
		if appName == "" {
			appName = defaultAppName
		}
		if truncateGitHubAppName(appName, githubAppNameLimit) != appName {
			w.WriteHeader(http.StatusUnprocessableEntity)
			render(w, r, fmt.Sprintf("GitHub App name must be at most %d characters.", githubAppNameLimit))
			return
		}
		var nonce [32]byte
		rand.Read(nonce[:])
		newState := hex.EncodeToString(nonce[:])
		selected, err := selectGitHubTarget(r.PostForm.Get("scope"), scale, appName, newState)
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			render(w, r, err.Error())
			return
		}
		target, state = selected, newState
		http.Redirect(w, r, startPath, http.StatusSeeOther)
	})
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if query := r.URL.Query(); state == "" || query.Get("state") != state || query.Get("code") == "" {
			http.Error(w, "Invalid GitHub callback. Reopen the setup link.", http.StatusBadRequest)
			return
		}
		if app.ID == 0 {
			var registration githubApp
			code := url.PathEscape(r.URL.Query().Get("code"))
			_, err := githubRequest(r.Context(), client, http.MethodPost, "/app-manifests/"+code+"/conversions", "", &registration)
			if err != nil {
				render(w, r, "Could not complete GitHub App registration. Reopen the callback URL to retry.")
				return
			}
			key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(registration.PEM))
			if err != nil || registration.ID <= 0 || !githubPathPart.MatchString(registration.Slug) {
				render(w, r, "GitHub returned incomplete App credentials. Return to the terminal and restart setup.")
				return
			}
			app, tokenKey = registration, key
		}
		http.Redirect(w, r, startPath, http.StatusSeeOther)
	})
	mux.HandleFunc("POST "+checkPath, func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if connected || app.ID == 0 {
			http.Redirect(w, r, startPath, http.StatusSeeOther)
			return
		}
		now := time.Now()
		token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
			Issuer:    strconv.FormatInt(app.ID, 10),
			IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		}).SignedString(tokenKey)
		if err != nil {
			render(w, r, "Could not sign a GitHub App token.")
			return
		}
		var installation struct {
			ID          int64             `json:"id"`
			SuspendedAt *time.Time        `json:"suspended_at"`
			Permissions map[string]string `json:"permissions"`
		}
		status, err := githubRequest(r.Context(), client, http.MethodGet, target.apiPath, token, &installation)
		switch {
		case status == http.StatusNotFound:
			render(w, r, "GitHub cannot find the installation for this target yet. Confirm the installation and try again.")
		case err != nil:
			render(w, r, fmt.Sprintf("GitHub could not verify the installation (%s). Try again.", err))
		case installation.ID <= 0 || installation.SuspendedAt != nil || installation.Permissions[target.permission] != "write":
			render(w, r, "The installation must be active and grant the runner permission requested by Ephemera.")
		default:
			connected = true
			render(w, r, "GitHub App connected.")
			result <- githubConnection{
				AppID: app.ID, InstallationID: installation.ID, Scope: target.scope, ScaleSet: target.scaleSet,
				AppSlug: app.Slug, Permission: target.permission, Key: []byte(app.PEM),
			}
		}
	})

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      25 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != address && (publicHost == "" || r.Host != publicHost) {
				http.Error(w, "Invalid setup address", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Frame-Options", "DENY")
			mux.ServeHTTP(w, r)
		}),
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	var publicURL string
	if publicHost != "" {
		publicURL = baseURL + startPath
	}
	ready(address, localBase+startPath, publicURL)
	select {
	case connection := <-result:
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		return connection, nil
	case err := <-serveErr:
		return githubConnection{}, fmt.Errorf("serve browser setup: %w", err)
	case <-ctx.Done():
		return githubConnection{}, ctx.Err()
	}
}

func githubRequest(ctx context.Context, client *http.Client, method, path, token string, result any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, githubAPI+path, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, errors.New("could not reach GitHub")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return response.StatusCode, json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}
