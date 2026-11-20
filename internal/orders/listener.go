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
	"time"

	"github.com/actions/scaleset"
)

type Config struct {
	ScopeURL       string
	ScaleSetName   string
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
}

// Update carries the latest demand and the lifecycle events in a single batch.
// Handlers must tolerate replay. A successful return acknowledges the batch.
type Update struct {
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
	set, err := findScaleSet(ctx, client, cfg.ScaleSetName, cfg.Create)
	if err != nil {
		return err
	}
	info.ScaleSetID = set.ID
	client.SetSystemInfo(info)

	var options []scaleset.HTTPOption
	if cfg.Capacity != nil {
		// The SDK maps zero to its default retry count. An ambiguous
		// acquisition must return to the caller to rebuild the session.
		options = append(options, scaleset.WithRetryMax(-1))
	}
	s, err := client.MessageSessionClient(ctx, set.ID, cfg.Owner, options...)
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
	logger.Info("Listening for GitHub jobs", "scale_set", set.Name, "scale_set_id", set.ID, "claim", cfg.Claim)
	err = pollWithCapacity(ctx, s, cfg.MaxConcurrent, cfg.Claim, cfg.CheckImage, cfg.Capacity, func(ctx context.Context, update Update) error {
		update.ScaleSetID = set.ID
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
	set, err := lookupScaleSet(ctx, client, cfg.ScaleSetName)
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

func lookupScaleSet(ctx context.Context, client scaleSets, name string) (*scaleset.RunnerScaleSet, error) {
	// The SDK interpolates the name into the query string.
	set, err := client.GetRunnerScaleSet(ctx, defaultGroupID, url.QueryEscape(name))
	if err != nil {
		return nil, fmt.Errorf("find Scale Set %q: %w", name, err)
	}
	return set, nil
}

func findScaleSet(ctx context.Context, client scaleSets, name string, create bool) (*scaleset.RunnerScaleSet, error) {
	set, err := lookupScaleSet(ctx, client, name)
	if err != nil {
		return nil, err
	}
	if set != nil {
		return set, nil
	}
	if !create {
		return nil, fmt.Errorf("Scale Set %q was not found in the default runner group; use --create to register it", name)
	}
	set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: defaultGroupID,
		Labels:        []scaleset.Label{{Name: name}},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return nil, fmt.Errorf("create Scale Set %q: %w", name, err)
	}
	return set, nil
}

func poll(ctx context.Context, s session, limit int, claim bool, checkImage func() error, handle func(context.Context, Update) error) error {
	return pollWithCapacity(ctx, s, limit, claim, checkImage, nil, handle)
}

func pollWithCapacity(ctx context.Context, s session, limit int, claim bool, checkImage func() error, local func() int, handle func(context.Context, Update) error) error {
	initialSession := s.Session()
	stats := initialSession.Statistics
	if stats == nil {
		return errors.New("GitHub returned no initial Scale Set statistics")
	}
	if local != nil && checkImage != nil {
		if err := checkImage(); err != nil {
			return fmt.Errorf("runner image unavailable: %w", err)
		}
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
	acquired := make(map[int64]bool)
	var pending int
	for ctx.Err() == nil {
		if checkImage != nil {
			if err := checkImage(); err != nil {
				return fmt.Errorf("runner image unavailable: %w", err)
			}
		}
		if local != nil {
			// The capacity header describes total scale set capacity,
			// including already occupied slots.
			occupied := max(stats.TotalAssignedJobs, stats.TotalAcquiredJobs)
			capacity = min(limit, occupied+pending+local())
		}
		msg, err := s.GetMessage(ctx, lastMessageID, capacity)
		if err != nil {
			return fmt.Errorf("poll Scale Set: %w", err)
		}
		if local != nil && s.Session().Statistics != initialSession.Statistics {
			return errors.New("Scale Set session refreshed during polling")
		}
		if msg == nil {
			continue
		}
		if msg.Statistics != nil && msg.MessageID > statisticsMessageID {
			statisticsMessageID = msg.MessageID
			pending = 0
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
			occupied := max(stats.TotalAssignedJobs, stats.TotalAcquiredJobs)
			slots := limit - occupied - pending
			offers := msg.JobAvailableMessages
			if local != nil {
				slots = min(slots, local())
				offers = nil
				for _, offer := range msg.JobAvailableMessages {
					if offer != nil && !acquired[offer.RunnerRequestID] {
						offers = append(offers, offer)
					}
				}
			}
			ids := claimable(offers, slots)
			if len(ids) > 0 {
				if checkImage != nil {
					if err := checkImage(); err != nil {
						return fmt.Errorf("runner image unavailable: %w", err)
					}
				}
				update.Acquired, err = s.AcquireJobs(ctx, ids)
				if err != nil {
					return fmt.Errorf("acquire GitHub jobs: %w", err)
				}
				if local != nil && s.Session().Statistics != initialSession.Statistics {
					return errors.New("Scale Set session refreshed during acquisition")
				}
				for _, id := range update.Acquired {
					if !acquired[id] {
						acquired[id] = true
						pending++
					}
				}
			}
		}
		if local != nil && len(update.Acquired) > 0 {
			update.Assigned, update.Started, update.Completed = nil, nil, nil
			if err := handle(ctx, update); err != nil {
				return fmt.Errorf("handle GitHub acquisitions: %w", err)
			}
		}
		if local == nil {
			if err := handle(ctx, update); err != nil {
				return fmt.Errorf("handle Scale Set message %d: %w", msg.MessageID, err)
			}
		}
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = s.DeleteMessage(ackCtx, msg.MessageID)
		cancel()
		if err != nil {
			return fmt.Errorf("acknowledge Scale Set message %d: %w", msg.MessageID, err)
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
