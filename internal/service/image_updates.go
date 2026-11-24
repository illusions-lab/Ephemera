package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/illusions-lab/Ephemera/internal/backend"
	"github.com/illusions-lab/Ephemera/internal/config"
	"github.com/illusions-lab/Ephemera/internal/imagechannel"
	"github.com/illusions-lab/Ephemera/internal/status"
	"github.com/illusions-lab/Ephemera/internal/version"
	"github.com/illusions-lab/Ephemera/internal/vm"
	"golang.org/x/sys/unix"
	"regexp"
)

type preparedImage struct {
	recipe *imagechannel.Recipe
	id     string
	check  func() error
}

type updateState struct {
	Current, Previous *imagechannel.Recipe
	Highest           uint64
	Rejected          []imagechannel.Recipe
	Failures          int
	FailureImage      string `json:",omitempty"`
	LastCheck         time.Time
	RolledBack        bool
	RollbackReason    string
	Retired           []string
}

type imageUpdates struct {
	channelFile        string
	path, platform     string
	enabled            bool
	state              updateState
	baseline, previous preparedImage
	prepare            func(context.Context, imagechannel.Recipe, bool) (preparedImage, error)
	logger             *slog.Logger
	prune              func(context.Context, string) error
	rebuild            func(context.Context) (string, func() error, error)
	managed            func(string) bool
	manualSource       string
	pruneSource        func(context.Context, string) error
	imageBytes         func(string) (uint64, error)
	cacheMax           uint64
	extraRetained      int
}

func newImageUpdates(cfg config.Config, root string, selection backend.Selection, logger *slog.Logger, build backend.ImageBuilder, channelFile string, managedBaseline ...string) *imageUpdates {
	custom := cfg.VM.Backend == "tart" && cfg.VM.ImageRef != config.MacDefaults().VM.ImageRef
	if cfg.VM.Backend == "qemu" {
		_, override := cfg.ImageSources[cfg.VM.Image][cfg.VM.Arch]
		custom = cfg.VM.Image != "ubuntu" || override
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	u := &imageUpdates{channelFile: channelFile, path: filepath.Join(root, "image-channel", "state.json"), platform: platform, logger: logger,
		enabled: !custom && cfg.Runner.ImageAutoUpdate != nil && *cfg.Runner.ImageAutoUpdate, rebuild: selection.PrepareImage, cacheMax: uint64(cfg.VM.CacheMaxGiB) * gib, extraRetained: cfg.VM.ExtraRetainedImages}
	if localName, ok := strings.CutPrefix(cfg.VM.ImageRef, "local:"); ok && cfg.VM.Backend == "tart" {
		// Keep the local recipe separate from rollback records of previously used sources.
		u.path = filepath.Join(root, "image-channel", localName+"-state.json")
	} else if custom {
		u.path = filepath.Join(root, "image-channel", "custom-state.json")
	}
	managedCache := filepath.Join(root, "image-channel", "cache")
	if cfg.VM.ImageCacheDir != "" {
		managedCache = filepath.Join(cfg.VM.ImageCacheDir, "channel")
	}
	if selection.VM != nil && cfg.VM.Backend == "tart" {
		tart := selection.VM.(*vm.Tart)
		u.managed = func(id string) bool { return strings.HasPrefix(id, "ephemera-base-") }
		u.prune = tart.RemoveCachedImage
		u.pruneSource = tart.RemoveCachedSource
		u.imageBytes = func(id string) (uint64, error) {
			return allocatedImageBytes(filepath.Join(tart.StoragePath(), "vms", id))
		}
	} else {
		keyedDirectory := regexp.MustCompile(`^ubuntu-(amd64|arm64)-[a-f0-9]{64}(\.repair-[0-9]+)?$`)
		u.managed = func(id string) bool {
			dir := filepath.Dir(id)
			cache := filepath.Dir(dir)
			owned := cache == managedCache || !custom && slices.Contains(managedBaseline, cache)
			return owned && filepath.Base(id) == "image.qcow2" && keyedDirectory.MatchString(filepath.Base(dir))
		}
		u.imageBytes = func(id string) (uint64, error) { return allocatedImageBytes(filepath.Dir(id)) }
		u.prune = func(_ context.Context, id string) error {
			if !u.managed(id) {
				return errors.New("refusing to remove unmanaged image")
			}
			return os.RemoveAll(filepath.Dir(id))
		}
	}
	u.prepare = func(ctx context.Context, recipe imagechannel.Recipe, smoke bool) (preparedImage, error) {
		if err := recipe.Validate(platform, version.Version); err != nil {
			return preparedImage{}, err
		}
		if smoke {
			spacePath := root
			if recipe.Ubuntu != nil {
				spacePath = managedCache
			}
			if spacePath == "" {
				spacePath = "."
			}
			if err := ensureImageSpace(spacePath); err != nil {
				return preparedImage{}, err
			}
		}
		if recipe.Ubuntu != nil {
			imageCfg, err := cfg.ImageConfig()
			if err != nil {
				return preparedImage{}, err
			}
			imageCfg.Source = *recipe.Ubuntu
			id, check, err := build(ctx, managedCache, imageCfg, logger)
			if err != nil {
				return preparedImage{}, err
			}
			id, err = filepath.Abs(id)
			if err != nil {
				return preparedImage{}, err
			}
			return preparedImage{&recipe, id, check}, check()
		}
		tart := selection.VM.(*vm.Tart)
		id := recipe.TartRef
		id, check, err := tart.PrepareImage(ctx, id, recipe.RunnerVersion, recipe.RunnerSHA256, cfg.VM.AppleToolchain, nil)
		if err != nil {
			return preparedImage{}, err
		}
		return preparedImage{&recipe, id, check}, check()
	}
	return u
}

func (u *imageUpdates) recover(ctx context.Context, baseline preparedImage) preparedImage {
	u.baseline, u.previous = baseline, baseline
	data, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return baseline
	}
	if err == nil {
		err = json.Unmarshal(data, &u.state)
	}
	if err != nil {
		u.state = updateState{}
		u.logger.Warn("Read image update state", "error", err)
		return baseline
	}
	if u.state.Current == nil {
		return baseline
	}
	if u.state.Previous != nil {
		previous, err := u.prepare(ctx, *u.state.Previous, false)
		if err == nil {
			u.previous = previous
		} else {
			u.logger.Warn("Recover previous runner image", "error", err)
		}
	}
	active, err := u.prepare(ctx, *u.state.Current, false)
	if err == nil {
		return active
	}
	u.logger.Warn("Recover active runner image", "error", err)
	if u.previous.id == "" {
		return preparedImage{}
	}
	next := u.state
	next.Current, next.Previous, next.Failures, next.RolledBack = u.previous.recipe, nil, 0, true
	next.FailureImage = ""
	next.RollbackReason = "active image could not be recovered"
	next.Rejected = append(next.Rejected, *u.state.Current)
	if err := u.save(next); err != nil {
		u.logger.Error("Persist recovered runner image", "error", err)
	}
	return u.previous
}

func (u *imageUpdates) save(next updateState) error {
	if err := writeImageState(u.path, next); err != nil {
		return err
	}
	u.state = next
	dir := filepath.Dir(u.path)
	directory, err := os.Open(dir)
	if err == nil {
		defer directory.Close()
		err = directory.Sync()
	}
	if err != nil {
		u.logger.Error("Sync image update directory", "error", err)
	}
	return nil
}

func writeImageState(path string, state any) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (u *imageUpdates) watch(ctx context.Context, r *reconciler, start updateState) {
	delay := time.Duration(rand.IntN(300)) * time.Second
	failures := 0
	if u.channelFile != "" {
		delay = 0
	}
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		checked := time.Now()
		manifest, err := u.fetch(ctx)
		var candidate preparedImage
		if err == nil {
			recipe, ok := manifest.Images[u.platform]
			if ok && recipe.Revision > start.Highest && !rejected(start, recipe) {
				if err = recipe.Validate(u.platform, version.Version); err == nil {
					work, cancel := context.WithTimeout(ctx, 55*time.Minute)
					candidate, err = u.prepare(work, recipe, true)
					cancel()
				}
			}
		}
		reply := make(chan updateState, 1)
		if r.send(ctx, runnerEvent{kind: imageChecked, image: candidate, err: err, checked: checked, imageReply: reply}) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case start = <-reply:
		}
		if err != nil {
			failures++
			delay = min(time.Hour, time.Minute<<min(failures-1, 6))
		} else {
			failures = 0
			delay = 24*time.Hour + time.Duration(rand.IntN(900))*time.Second
		}
		if u.channelFile != "" {
			delay = min(delay, 5*time.Second)
		}
	}
}

func (u *imageUpdates) fetch(ctx context.Context) (imagechannel.Manifest, error) {
	if u.channelFile == "" {
		return imagechannel.Fetch(ctx, imagechannel.URL)
	}
	f, err := os.Open(u.channelFile)
	if err != nil {
		return imagechannel.Manifest{}, err
	}
	defer f.Close()
	return imagechannel.Decode(f)
}

func rejected(s updateState, candidate imagechannel.Recipe) bool {
	for _, ignored := range s.Rejected {
		if ignored.TartRef == candidate.TartRef && ignored.RunnerSHA256 == candidate.RunnerSHA256 &&
			(ignored.Ubuntu == nil && candidate.Ubuntu == nil || ignored.Ubuntu != nil && candidate.Ubuntu != nil && *ignored.Ubuntu == *candidate.Ubuntu) {
			return true
		}
	}
	return false
}

func ensureImageSpace(path string) error {
	for {
		var fs unix.Statfs_t
		err := unix.Statfs(path, &fs)
		if errors.Is(err, os.ErrNotExist) && filepath.Dir(path) != path {
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return err
		}
		if uint64(fs.Bavail)*uint64(fs.Bsize) < 20<<30 {
			return errors.New("runner image preparation requires 20 GiB free")
		}
		return nil
	}
}

func (r *reconciler) setImage(p preparedImage) {
	r.active.Store(&p)
	clear(r.exitedRunners)
	r.template.Image = p.id
	r.imageBlocked.Store(false)
	r.imageReason = ""
	r.template.ImageRevision, r.template.RunnerVersion, r.template.RunnerSHA256 = "", "", ""
	if p.recipe != nil {
		r.template.ImageRevision = strconv.FormatUint(p.recipe.Revision, 10)
		r.template.RunnerVersion, r.template.RunnerSHA256 = p.recipe.RunnerVersion, p.recipe.RunnerSHA256
	}
}

func (r *reconciler) retire(state *updateState, previous preparedImage) {
	if previous.id != "" && (previous.recipe != nil || r.images.managed != nil && r.images.managed(previous.id)) {
		state.Retired = append(append([]string(nil), state.Retired...), previous.id)
	}
}

func (r *reconciler) pruneImages() {
	u := r.images
	if u.prune == nil || len(u.state.Retired) == 0 {
		return
	}
	next := u.state
	next.Retired = nil
	retained := u.retainExtra(u.state.Retired, r.template.Image, u.previous.id, u.baseline.id)
	for _, id := range u.state.Retired {
		keep := retained[id] || id == r.template.Image || id == u.previous.id || id == u.baseline.id
		for _, instance := range r.instances {
			keep = keep || !instance.LocalReclaimed && instance.Image == id
		}
		if !keep {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := u.prune(ctx, id)
			cancel()
			if err != nil {
				u.logger.Warn("Remove obsolete runner image", "error", err)
				keep = true
			}
		}
		if keep && !slices.Contains(next.Retired, id) {
			next.Retired = append(next.Retired, id)
		}
	}
	if len(next.Retired) != len(u.state.Retired) {
		if err := u.save(next); err != nil {
			u.logger.Error("Persist image cleanup", "error", err)
		}
	}
}

func (r *reconciler) imageChecked(event runnerEvent) {
	u := r.images
	defer func() {
		if !r.draining {
			r.pruneImages()
		}
		event.imageReply <- u.state
	}()
	next := u.state
	next.LastCheck = event.checked
	if event.err != nil {
		u.logger.Warn("Check runner image update", "error", event.err)
	}
	p := event.image
	if event.err == nil && p.recipe != nil && !r.draining && !rejected(next, *p.recipe) && p.recipe.Revision > next.Highest {
		r.retire(&next, u.previous)
		next.RollbackReason = ""
		next.Previous, next.Current, next.Highest, next.Failures, next.RolledBack = next.Current, p.recipe, p.recipe.Revision, 0, false
		next.FailureImage = ""
		if err := u.save(next); err != nil {
			u.logger.Error("Save image update", "error", err)
			return
		}
		u.previous = *r.active.Load()
		r.setImage(p)
		u.logger.Info("Activated runner image", "revision", p.recipe.Revision)
	} else {
		u.state.LastCheck = event.checked
		if event.err != nil && p.recipe != nil && p.id != "" {
			r.retire(&next, p)
			if err := u.save(next); err != nil {
				u.logger.Error("Track failed candidate cleanup", "error", err)
			}
		}
	}
}

func (r *reconciler) imageOutcome(i vm.Instance, event runnerEvent) {
	u := r.images
	if u == nil || r.draining || i.ImageRevision != r.template.ImageRevision || i.Image != r.template.Image {
		return
	}
	next := u.state
	if event.kind == jobTerminal {
		if next.Failures == 0 {
			return
		}
		next.Failures, next.FailureImage = 0, ""
	} else {
		if !errors.Is(event.err, vm.ErrImageInfrastructure) {
			return
		}
		if next.FailureImage != "" && next.FailureImage != i.Image {
			next.Failures = 0
		}
		next.FailureImage = i.Image
		next.Failures++
	}
	if next.Failures >= 3 {
		if next.Current == nil || u.previous.id == i.Image || u.previous.check == nil || u.previous.check() != nil {
			r.blockFailedImage()
			if err := u.save(next); err != nil {
				u.logger.Error("Save image failure", "error", err)
			}
			return
		}
		r.retire(&next, *r.active.Load())
		next.RollbackReason = event.stage
		next.Rejected = append(append([]imagechannel.Recipe(nil), next.Rejected...), *next.Current)
		next.Current, next.Previous, next.RolledBack, next.Failures = u.previous.recipe, nil, true, 0
		next.FailureImage = ""
		if err := u.save(next); err != nil {
			u.logger.Error("Save image rollback", "error", err)
			return
		}
		r.setImage(u.previous)
		u.previous = u.baseline
		u.logger.Warn("Rolled back runner image", "reason", event.stage, "revision", i.ImageRevision)
		return
	}
	if err := u.save(next); err != nil {
		u.logger.Error("Save image outcome", "error", err)
		return
	}
	if next.Failures == 0 && r.imageReason == failedImageQuarantine {
		r.imageReason = ""
		r.imageBlocked.Store(false)
	}
}

const failedImageQuarantine = "runner image failed repeatedly and no verified rollback is available"

func (r *reconciler) blockFailedImage() {
	r.imageBlocked.Store(true)
	r.imageReason = failedImageQuarantine
}

func (u *imageUpdates) snapshot() *status.ImageUpdate {
	if u == nil {
		return nil
	}
	s := &status.ImageUpdate{Enabled: u.enabled, Current: "bundled", LastCheck: u.state.LastCheck, RolledBack: u.state.RolledBack, RollbackReason: u.state.RollbackReason}
	if u.state.Current != nil {
		s.Current = fmt.Sprint(u.state.Current.Revision)
	}
	if u.state.Previous != nil {
		s.Previous = fmt.Sprint(u.state.Previous.Revision)
	} else if u.state.Current != nil {
		s.Previous = "bundled"
	}
	return s
}
