// Package orders translates GitHub Scale Set messages into runner demand.
package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
)

type Config struct {
	ScopeURL       string
	ScaleSetName   string
	RunnerGroup    string
	Labels         []string
	ReplaceLabels  bool
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
	MaxConcurrent  int
	Owner          string
	Create         bool
	Claim          bool
	CheckImage     func() error
	Capacity       func() int
	Disconnected   func()
	Polled         func()
	Acquisitions   *Acquisitions
	BeginAcquire   func() (func(), bool)
}

// Update carries the latest demand and the lifecycle events in a single batch.
// Handlers must tolerate replay. A successful return acknowledges the batch.
type Update struct {
	RunnerGroup      string
	Labels           *[]string
	ScaleSetID       int
	TargetRunners    int
	AssignedJobs     int
	RunningJobs      int
	AvailableJobs    int
	DemandObservedAt time.Time
	Acquired         []int64
	Assigned         []*scaleset.JobAssigned
	Started          []*scaleset.JobStarted
	Completed        []*scaleset.JobCompleted
}

type session interface {
	Session() scaleset.RunnerScaleSetSession
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error)
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	DeleteMessage(ctx context.Context, messageID int) error
}

type scaleSets interface {
	GetRunnerGroupByName(context.Context, string) (*scaleset.RunnerGroup, error)
	UpdateRunnerScaleSet(context.Context, int, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	GetRunnerScaleSet(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
}

// Listen runs one message session. The caller may restart it after a terminal error.
func Listen(ctx context.Context, cfg Config, logger *slog.Logger, handle func(context.Context, Update) error) error {
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > math.MaxInt32 {
		return errors.New("runner.max_concurrent must be between 1 and 2147483647")
	}
	if cfg.Claim && cfg.CheckImage == nil {
		return errors.New("claiming jobs requires a prepared runner image")
	}
	info := scaleset.SystemInfo{System: "ephemera", Subsystem: "listener"}
	client, err := newGitHubClient(cfg, info)
	if err != nil {
		return fmt.Errorf("configure GitHub client: %w", err)
	}
	set, err := findScaleSet(ctx, client, cfg)
	if err != nil {
		return err
	}
	reportScaleSetLabels(logger, cfg, set)
	info.ScaleSetID = set.ID
	client.SetSystemInfo(info)

	var options []scaleset.HTTPOption
	if cfg.Capacity != nil {
		// The SDK maps zero to its default retry count. An ambiguous
		// acquisition must return to the caller to rebuild the session.
		options = append(options, scaleset.WithRetryMax(-1))
	}
	var s *scaleset.MessageSessionClient
	openCtx, cancelOpen := context.WithTimeout(ctx, 90*time.Second)
	err = awaitSessionHandoff(openCtx, 3*time.Second, func(ctx context.Context) error {
		var err error
		s, err = client.MessageSessionClient(ctx, set.ID, cfg.Owner, options...)
		return err
	}, func() {
		logger.Info("Waiting for the previous GitHub Scale Set session to close", "scale_set_id", set.ID)
	})
	cancelOpen()
	if err != nil {
		return fmt.Errorf("open Scale Set session: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := s.Close(closeCtx); err != nil {
			logger.Warn("Could not close Scale Set session", "scale_set_id", set.ID, "error", err)
		}
	}()
	logger.Info("Listening for GitHub jobs", "scale_set", set.Name, "scale_set_id", set.ID, "intake_enabled", cfg.Claim || cfg.Capacity != nil)
	group := set.RunnerGroupName
	if group == "" {
		group = cfg.RunnerGroup
		if group == "" {
			group = "Default"
		}
	}
	var labels *[]string
	if set.Labels != nil {
		names := make([]string, 0, len(set.Labels))
		for _, label := range set.Labels {
			names = append(names, label.Name)
		}
		labels = &names
	}
	err = pollTracked(ctx, s, cfg.MaxConcurrent, cfg.Claim, cfg.CheckImage, cfg.Capacity, cfg.Polled, cfg.Acquisitions, cfg.BeginAcquire, func(ctx context.Context, update Update) error {
		update.ScaleSetID = set.ID
		update.RunnerGroup, update.Labels = group, labels
		return handle(ctx, update)
	})
	if cfg.Disconnected != nil {
		cfg.Disconnected()
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

// ProbeScaleSet authenticates and checks for the configured scale set without
// creating a scale set or opening a message session.
func ProbeScaleSet(ctx context.Context, cfg Config) (bool, error) {
	client, err := newGitHubClient(cfg, scaleset.SystemInfo{System: "ephemera", Subsystem: "doctor"})
	if err != nil {
		return false, fmt.Errorf("configure GitHub client: %w", err)
	}
	set, err := lookupScaleSet(ctx, client, cfg)
	return set != nil, err
}

func newGitHubClient(cfg Config, info scaleset.SystemInfo, options ...scaleset.HTTPOption) (*scaleset.Client, error) {
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: cfg.ScopeURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       strconv.FormatInt(cfg.AppID, 10),
			InstallationID: cfg.InstallationID,
			PrivateKey:     string(cfg.PrivateKey),
		},
		SystemInfo: info,
	}, options...)
}

const defaultGroupID = 1

// A replacement daemon can start before GitHub releases the old owner's lease.
func awaitSessionHandoff(ctx context.Context, delay time.Duration, open func(context.Context) error, waiting func()) error {
	reported := false
	for {
		err := open(ctx)
		if err == nil || !strings.Contains(err.Error(), "409 Conflict") || !strings.Contains(err.Error(), "RunnerScaleSetSessionConflictException") {
			return err
		}
		if !reported {
			waiting()
			reported = true
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), err)
		case <-timer.C:
		}
	}
}

func lookupScaleSet(ctx context.Context, client scaleSets, cfg Config) (*scaleset.RunnerScaleSet, error) {
	groupID, err := runnerGroupID(ctx, client, cfg.RunnerGroup)
	if err != nil {
		return nil, err
	}
	return lookupScaleSetInGroup(ctx, client, groupID, cfg.ScaleSetName)
}

func runnerGroupID(ctx context.Context, client scaleSets, name string) (int, error) {
	if name == "" || strings.EqualFold(name, "Default") {
		return defaultGroupID, nil
	}
	group, err := client.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("find runner group %q: %w", name, err)
	}
	if group == nil || group.ID <= 0 {
		return 0, fmt.Errorf("runner group %q was not found", name)
	}
	return group.ID, nil
}

func lookupScaleSetInGroup(ctx context.Context, client scaleSets, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	// The SDK interpolates the name into the query string.
	set, err := client.GetRunnerScaleSet(ctx, groupID, url.QueryEscape(name))
	if err != nil {
		return nil, fmt.Errorf("find Scale Set %q: %w", name, err)
	}
	return set, nil
}

func findScaleSet(ctx context.Context, client scaleSets, cfg Config) (*scaleset.RunnerScaleSet, error) {
	name := cfg.ScaleSetName
	groupID, err := runnerGroupID(ctx, client, cfg.RunnerGroup)
	if err != nil {
		return nil, err
	}
	set, err := lookupScaleSetInGroup(ctx, client, groupID, name)
	if err != nil {
		return nil, err
	}
	if set == nil && !cfg.Create {
		return nil, fmt.Errorf("Scale Set %q was not found in runner group %q; use --create to register it", name, cfg.RunnerGroup)
	}
	if set != nil && !cfg.Create {
		return set, nil
	}
	desired := append([]string{name}, cfg.Labels...)
	current := []scaleset.Label(nil)
	if set != nil {
		current = set.Labels
	}
	labels, changed := scaleSetLabels(desired, current, cfg.ReplaceLabels)
	if set != nil {
		if !changed {
			return set, nil
		}
		updated, err := client.UpdateRunnerScaleSet(ctx, set.ID, &scaleset.RunnerScaleSet{Labels: labels, RunnerSetting: set.RunnerSetting})
		if err != nil {
			return nil, fmt.Errorf("update Scale Set %q labels: %w", name, err)
		}
		if updated.RunnerGroupName == "" {
			updated.RunnerGroupName = set.RunnerGroupName
		}
		return updated, nil
	}
	set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name: name, RunnerGroupID: groupID, Labels: labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return nil, fmt.Errorf("create Scale Set %q: %w", name, err)
	}
	return set, nil
}

func reportScaleSetLabels(logger *slog.Logger, cfg Config, set *scaleset.RunnerScaleSet) {
	desired := append([]string{cfg.ScaleSetName}, cfg.Labels...)
	if _, missing := scaleSetLabels(desired, set.Labels, false); missing {
		var effective []string
		for _, label := range set.Labels {
			effective = append(effective, label.Name)
		}
		logger.Warn("GitHub did not return all requested Scale Set labels",
			"scale_set", set.Name, "scale_set_id", set.ID,
			"requested_labels", desired, "effective_labels", effective)
	}
}

func scaleSetLabels(desired []string, current []scaleset.Label, replace bool) ([]scaleset.Label, bool) {
	existing := make(map[string]scaleset.Label, len(current))
	for _, label := range current {
		existing[strings.ToLower(label.Name)] = label
	}
	var labels []scaleset.Label
	used := make(map[string]bool, len(desired))
	changed := false
	for _, name := range desired {
		key := strings.ToLower(name)
		if used[key] {
			continue
		}
		used[key] = true
		label, found := existing[key]
		if !found {
			label = scaleset.Label{Name: name}
			changed = true
		}
		labels = append(labels, label)
	}
	if !replace {
		for _, label := range current {
			if !used[strings.ToLower(label.Name)] {
				labels = append(labels, label)
			}
		}
	}
	return labels, changed || len(labels) != len(current)
}

func poll(ctx context.Context, s session, limit int, claim bool, checkImage func() error, handle func(context.Context, Update) error) error {
	return pollWithCapacity(ctx, s, limit, claim, checkImage, nil, handle)
}

func pollWithCapacity(ctx context.Context, s session, limit int, claim bool, checkImage func() error, local func() int, handle func(context.Context, Update) error) error {
	return pollSession(ctx, s, limit, claim, checkImage, local, nil, handle)
}

func pollSession(ctx context.Context, s session, limit int, claim bool, checkImage func() error, local func() int, polled func(), handle func(context.Context, Update) error) error {
	return pollTracked(ctx, s, limit, claim, checkImage, local, polled, nil, nil, handle)
}

type acquisitionState uint8

const (
	acquisitionUnknown acquisitionState = iota + 1
	acquisitionAccepted
	acquisitionTerminal
)

// Acquisitions belongs to the serial listener loop and survives session changes.
type Acquisitions struct {
	requests map[int64]acquisitionState
}

func pollTracked(ctx context.Context, s session, limit int, claim bool, checkImage func() error, local func() int, polled func(), tracker *Acquisitions, begin func() (func(), bool), handle func(context.Context, Update) error) error {
	const maxTrackedAcquisitions = 65536
	initialSession := s.Session()
	stats := initialSession.Statistics
	if stats == nil {
		return errors.New("GitHub returned no initial Scale Set statistics")
	}

	observedAt := time.Now().UTC()
	initial := demand(stats, limit)
	initial.DemandObservedAt = observedAt
	if err := handle(ctx, initial); err != nil {
		return fmt.Errorf("handle initial runner demand: %w", err)
	}
	capacity := 0
	if claim {
		capacity = limit
	}
	var lastMessageID int
	var statisticsMessageID int
	if tracker == nil {
		tracker = &Acquisitions{}
	}
	if tracker.requests == nil {
		tracker.requests = make(map[int64]acquisitionState)
	}
	acquired := tracker.requests
	pending := make(map[int64]struct{})
	available := func() int {
		occupied := max(stats.TotalAssignedJobs, stats.TotalAcquiredJobs)
		slots := min(limit-occupied-len(pending), maxTrackedAcquisitions-len(acquired))
		if local != nil {
			// The reconciler budgets against assigned demand. Acquired jobs
			// awaiting assignment, including replies after the last statistics,
			// also need a VM or a ready runner.
			unassigned := max(0, stats.TotalAcquiredJobs-stats.TotalAssignedJobs)
			slots = min(slots, local()-unassigned-len(pending))
		}
		return max(0, slots)
	}
	for ctx.Err() == nil {
		if local == nil && checkImage != nil {
			if err := checkImage(); err != nil {
				return fmt.Errorf("runner image unavailable: %w", err)
			}
		}
		if local != nil {
			// The capacity header describes total scale set capacity,
			// including already occupied slots.
			occupied := max(stats.TotalAssignedJobs, stats.TotalAcquiredJobs)
			capacity = min(limit, occupied+len(pending)+available())
		}
		msg, err := s.GetMessage(ctx, lastMessageID, capacity)
		if err != nil {
			return fmt.Errorf("poll Scale Set: %w", err)
		}
		if local != nil && s.Session().Statistics != initialSession.Statistics {
			return errors.New("Scale Set session refreshed during polling")
		}
		if polled != nil {
			polled()
		}
		if msg == nil {
			continue
		}
		if msg.Statistics != nil && msg.MessageID > statisticsMessageID {
			statisticsMessageID = msg.MessageID
			clear(pending)
			stats = msg.Statistics
			observedAt = time.Now().UTC()
		}
		update := demand(stats, limit)
		update.DemandObservedAt = observedAt
		update.Assigned = msg.JobAssignedMessages
		update.Started = msg.JobStartedMessages
		update.Completed = msg.JobCompletedMessages
		if local != nil {
			if err := handle(ctx, update); err != nil {
				return fmt.Errorf("handle Scale Set message %d: %w", msg.MessageID, err)
			}
		}
		if (claim || local != nil) && msg.MessageID > lastMessageID && len(msg.JobAvailableMessages) > 0 {
			if local == nil && msg.Statistics == nil {
				return fmt.Errorf("Scale Set message %d contains job offers without current statistics", msg.MessageID)
			}
			slots := available()
			offers := msg.JobAvailableMessages
			if local != nil {
				offers = nil
				for _, offer := range msg.JobAvailableMessages {
					if offer != nil && acquired[offer.RunnerRequestID] == 0 {
						offers = append(offers, offer)
					}
				}
			}
			ids := claimable(offers, slots)
			if len(ids) > 0 && checkImage != nil {
				if err := checkImage(); err != nil {
					if local == nil {
						return fmt.Errorf("runner image unavailable: %w", err)
					}
					ids = nil
				}
			}
			if len(ids) > 0 {
				release, allowed := func() {}, true
				if begin != nil {
					release, allowed = begin()
				}
				if allowed {
					for _, id := range ids {
						acquired[id] = acquisitionUnknown
					}
					update.Acquired, err = s.AcquireJobs(ctx, ids)
					if err != nil {
						release()
						return fmt.Errorf("acquire GitHub jobs: %w", err)
					}
					if local != nil && s.Session().Statistics != initialSession.Statistics {
						release()
						return errors.New("Scale Set session refreshed during acquisition")
					}
					for _, id := range ids {
						delete(acquired, id)
					}
					for _, id := range update.Acquired {
						acquired[id] = acquisitionAccepted
						pending[id] = struct{}{}
					}
					if local != nil && len(update.Acquired) > 0 {
						update.Assigned, update.Started, update.Completed = nil, nil, nil
						err = handle(ctx, update)
					}
					release()
					if err != nil {
						return fmt.Errorf("handle GitHub acquisitions: %w", err)
					}
				}
			}
		}
		if local == nil {
			if err := handle(ctx, update); err != nil {
				return fmt.Errorf("handle Scale Set message %d: %w", msg.MessageID, err)
			}
		}
		for _, job := range msg.JobAssignedMessages {
			if job != nil && acquired[job.RunnerRequestID] == acquisitionUnknown {
				acquired[job.RunnerRequestID] = acquisitionAccepted
			}
		}
		for _, job := range msg.JobCompletedMessages {
			if job != nil && acquired[job.RunnerRequestID] != 0 {
				acquired[job.RunnerRequestID] = acquisitionTerminal
				delete(pending, job.RunnerRequestID)
			}
		}
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = s.DeleteMessage(ackCtx, msg.MessageID)
		cancel()
		if err != nil {
			return fmt.Errorf("acknowledge Scale Set message %d: %w", msg.MessageID, err)
		}
		if msg.MessageID > lastMessageID {
			for _, job := range msg.JobCompletedMessages {
				if job != nil && acquired[job.RunnerRequestID] == acquisitionTerminal {
					delete(acquired, job.RunnerRequestID)
				}
			}
		}
		lastMessageID = max(lastMessageID, msg.MessageID)
	}
	return ctx.Err()
}

func demand(stats *scaleset.RunnerScaleSetStatistic, limit int) Update {
	return Update{
		TargetRunners: min(limit, max(0, stats.TotalAssignedJobs)),
		AssignedJobs:  stats.TotalAssignedJobs,
		RunningJobs:   stats.TotalRunningJobs,
		AvailableJobs: stats.TotalAvailableJobs,
	}
}

func claimable(offers []*scaleset.JobAvailable, slots int) []int64 {
	var ids []int64
	seen := make(map[int64]bool)
	for _, offer := range offers {
		if len(ids) >= slots {
			break
		}
		if offer == nil || offer.RunnerRequestID <= 0 || seen[offer.RunnerRequestID] {
			continue
		}
		ids = append(ids, offer.RunnerRequestID)
		seen[offer.RunnerRequestID] = true
	}
	return ids
}
