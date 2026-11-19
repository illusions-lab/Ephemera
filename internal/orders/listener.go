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
}

// Update carries the latest demand and the lifecycle events in a single batch.
// Handlers must tolerate replay. A successful return acknowledges the batch.
type Update struct {
	TargetRunners int
	AssignedJobs  int
	RunningJobs   int
	AvailableJobs int
	Acquired      []int64
	Assigned      []*scaleset.JobAssigned
	Started       []*scaleset.JobStarted
	Completed     []*scaleset.JobCompleted
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
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: cfg.ScopeURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       strconv.FormatInt(cfg.AppID, 10),
			InstallationID: cfg.InstallationID,
			PrivateKey:     string(cfg.PrivateKey),
		},
		SystemInfo: info,
	})
	if err != nil {
		return fmt.Errorf("configure GitHub client: %w", err)
	}
	set, err := findScaleSet(ctx, client, cfg.ScaleSetName, cfg.Create)
	if err != nil {
		return err
	}
	info.ScaleSetID = set.ID
	client.SetSystemInfo(info)

	s, err := client.MessageSessionClient(ctx, set.ID, cfg.Owner)
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
	err = poll(ctx, s, cfg.MaxConcurrent, cfg.Claim, cfg.CheckImage, handle)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func findScaleSet(ctx context.Context, client scaleSets, name string, create bool) (*scaleset.RunnerScaleSet, error) {
	// The SDK's default runner group has ID 1. Its v0.4 lookup interpolates
	// the name into the query string, so escape it before the lookup.
	const defaultGroupID = 1
	set, err := client.GetRunnerScaleSet(ctx, defaultGroupID, url.QueryEscape(name))
	if err != nil {
		return nil, fmt.Errorf("find Scale Set %q: %w", name, err)
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
	})
	if err != nil {
		return nil, fmt.Errorf("create Scale Set %q: %w", name, err)
	}
	return set, nil
}

func poll(ctx context.Context, s session, limit int, claim bool, checkImage func() error, handle func(context.Context, Update) error) error {
	stats := s.Session().Statistics
	if stats == nil {
		return errors.New("GitHub returned no initial Scale Set statistics")
	}
	if err := handle(ctx, demand(stats, limit)); err != nil {
		return fmt.Errorf("handle initial runner demand: %w", err)
	}
	capacity := 0
	if claim {
		capacity = limit
	}
	var lastMessageID int
	for ctx.Err() == nil {
		if checkImage != nil {
			if err := checkImage(); err != nil {
				return fmt.Errorf("runner image unavailable: %w", err)
			}
		}
		msg, err := s.GetMessage(ctx, lastMessageID, capacity)
		if err != nil {
			return fmt.Errorf("poll Scale Set: %w", err)
		}
		if msg == nil {
			continue
		}
		if msg.Statistics != nil {
			stats = msg.Statistics
		}
		update := demand(stats, limit)
		update.Assigned = msg.JobAssignedMessages
		update.Started = msg.JobStartedMessages
		update.Completed = msg.JobCompletedMessages
		if claim && len(msg.JobAvailableMessages) > 0 {
			if msg.Statistics == nil {
				return fmt.Errorf("Scale Set message %d contains job offers without current statistics", msg.MessageID)
			}
			occupied := max(stats.TotalAssignedJobs, stats.TotalAcquiredJobs)
			ids := claimable(msg.JobAvailableMessages, limit-occupied)
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
			}
		}
		if err := handle(ctx, update); err != nil {
			return fmt.Errorf("handle Scale Set message %d: %w", msg.MessageID, err)
		}
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = s.DeleteMessage(ackCtx, msg.MessageID)
		cancel()
		if err != nil {
			return fmt.Errorf("acknowledge Scale Set message %d: %w", msg.MessageID, err)
		}
		lastMessageID = msg.MessageID
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
