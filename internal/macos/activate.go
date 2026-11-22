package macos

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/illusions-lab/Ephemera/internal/status"
	"golang.org/x/sys/unix"
)

type Candidate struct {
	Config, Key             []byte
	ReplaceEditable         bool
	Scope, ScaleSet, Intake string
}

type activation struct {
	root, plist, domain string
	wait                time.Duration
}

type previousService struct {
	Current string
	Plist   []byte
	Loaded  bool
}

func Activate(ctx context.Context, release string, prepare func() (*Candidate, error)) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	a := activation{Root(), filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), fmt.Sprintf("gui/%d", os.Getuid()), 45 * time.Second}
	return a.run(ctx, release, prepare)
}

func (a activation) run(ctx context.Context, release string, prepare func() (*Candidate, error)) (result error) {
	fd, err := unix.Open(filepath.Join(a.root, ".activation.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("lock macOS activation: %w", err)
	}
	if err := launchctl(ctx, "print", a.domain); err != nil {
		return fmt.Errorf("log in to the macOS desktop with this account: %w", err)
	}
	if err := a.recoverLegacy(); err != nil {
		return err
	}
	journal := filepath.Join(a.root, ".activation-transaction.json")
	data, err := os.ReadFile(journal)
	if err == nil {
		var previous previousService
		if err := json.Unmarshal(data, &previous); err != nil {
			return fmt.Errorf("read activation recovery record %s: %w", journal, err)
		}
		if err := a.recover(journal, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if launchctl(ctx, "print", a.domain+"/"+Label+".dev") == nil {
		return errors.New("stop the development LaunchAgent before activating Ephemera")
	}

	previous := previousService{Loaded: a.loaded(ctx)}
	previous.Current, err = os.Readlink(filepath.Join(a.root, "current"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	previous.Plist, err = os.ReadFile(a.plist)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if previous.Loaded && len(previous.Plist) == 0 {
		return errors.New("cannot recover the loaded LaunchAgent without its plist")
	}
	if err := a.pinLegacyConfig(ctx, &previous); err != nil {
		return err
	}
	candidate, err := prepare()
	if err != nil || candidate == nil {
		return err
	}
	id := uuid.NewString()
	snapshot, err := a.saveSnapshot(id, candidate.Config, candidate.Key)
	if err != nil {
		return err
	}
	logs := filepath.Join(a.root, "logs")
	for _, dir := range []string{logs, filepath.Dir(a.plist)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	plist, err := launchAgent(release, snapshot, id, filepath.Join(logs, "daemon.log"))
	if err != nil {
		return err
	}
	data, err = json.Marshal(previous)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceFile(journal, data); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			if err := a.recover(journal, previous); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	if previous.Loaded {
		if err := launchctl(ctx, "bootout", a.domain+"/"+Label); err != nil {
			return err
		}
	}
	if candidate.ReplaceEditable {
		if err := a.saveEditable(candidate.Config, candidate.Key); err != nil {
			return err
		}
	}
	if err := replaceFile(a.plist, plist); err != nil {
		return err
	}
	if err := launchctl(ctx, "bootstrap", a.domain, a.plist); err != nil {
		return err
	}
	if err := a.waitReady(ctx, id, candidate); err != nil {
		return fmt.Errorf("candidate LaunchAgent did not become healthy; see %s: %w", filepath.Join(logs, "daemon.log"), err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.setCurrent(release); err != nil {
		return err
	}
	return os.Remove(journal)
}

func (a activation) loaded(ctx context.Context) bool {
	return launchctl(ctx, "print", a.domain+"/"+Label) == nil
}

func (a activation) recover(journal string, previous previousService) error {
	if err := a.restore(previous); err != nil {
		return fmt.Errorf("restore previous LaunchAgent using %s: %w", journal, err)
	}
	return os.Remove(journal)
}

func (a activation) restore(previous previousService) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := launchctl(ctx, "print", a.domain); err != nil {
		return err
	}
	if a.loaded(ctx) {
		if err := launchctl(ctx, "bootout", a.domain+"/"+Label); err != nil {
			return err
		}
	}
	if err := a.setCurrent(previous.Current); err != nil {
		return err
	}
	if previous.Plist == nil {
		return removeIfPresent(a.plist)
	}
	if err := replaceFile(a.plist, previous.Plist); err != nil {
		return err
	}
	if previous.Loaded {
		return launchctl(ctx, "bootstrap", a.domain, a.plist)
	}
	return nil
}

func (a activation) saveSnapshot(id string, config, key []byte) (string, error) {
	dir := filepath.Join(a.root, "activations", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := replaceFile(filepath.Join(dir, "config.yaml"), config); err != nil {
		return "", err
	}
	return dir, replaceFile(filepath.Join(dir, "github-app.pem"), key)
}

func (a activation) setCurrent(target string) error {
	path := filepath.Join(a.root, "current")
	if target == "" {
		return removeIfPresent(path)
	}
	dir, err := os.MkdirTemp(a.root, ".current-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	return os.Rename(link, path)
}

func (a activation) waitReady(ctx context.Context, id string, candidate *Candidate) error {
	ctx, cancel := context.WithTimeout(ctx, a.wait)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		s, err := status.Read(ctx, filepath.Join(a.root, "state", "status.sock"))
		if err == nil && s.ActivationID == id && s.Scope == candidate.Scope &&
			s.ScaleSet == candidate.ScaleSet && s.Intake == candidate.Intake &&
			s.Phase == "Healthy" && s.GitHub == "connected" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func launchctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", args[0], err, out)
	}
	return nil
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".activation-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func launchAgent(release, snapshot, id, log string) ([]byte, error) {
	plist := struct {
		XMLName xml.Name     `xml:"plist"`
		Version string       `xml:"version,attr"`
		Dict    []plistValue `xml:"dict>value"`
	}{Version: "1.0", Dict: []plistValue{
		{"Label", Label},
		{"ProgramArguments", []string{
			filepath.Join(release, "ephemerad"), "--create",
			"--config", filepath.Join(snapshot, "config.yaml"),
			"--github-app-key", filepath.Join(snapshot, "github-app.pem"),
			"--activation-id", id,
		}},
		{"KeepAlive", true},
		{"ThrottleInterval", 10},
		{"ExitTimeOut", 60},
		{"Umask", 63},
		{"StandardOutPath", log},
		{"StandardErrorPath", log},
	}}
	body, err := xml.MarshalIndent(plist, "", "  ")
	return append([]byte(xml.Header), body...), err
}

type plistValue struct {
	key   string
	value any
}

func (p plistValue) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	write := func(tag string, value any) error {
		return e.EncodeElement(value, xml.StartElement{Name: xml.Name{Local: tag}})
	}
	if err := write("key", p.key); err != nil {
		return err
	}
	switch value := p.value.(type) {
	case string:
		return write("string", value)
	case int:
		return write("integer", value)
	case bool:
		return write("true", struct{}{})
	case []string:
		array := xml.StartElement{Name: xml.Name{Local: "array"}}
		if err := e.EncodeToken(array); err != nil {
			return err
		}
		for _, arg := range value {
			if err := write("string", arg); err != nil {
				return err
			}
		}
		return e.EncodeToken(array.End())
	default:
		return fmt.Errorf("unsupported plist value %T", value)
	}
}

// Keep the requested inputs available for subsequent setup retries.
func (a activation) saveEditable(config, key []byte) error {
	keyPath := filepath.Join(a.root, "credentials", "github-app.pem")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return err
	}
	if err := replaceFile(keyPath, key); err != nil {
		return err
	}
	return replaceFile(filepath.Join(a.root, "config.yaml"), config)
}
