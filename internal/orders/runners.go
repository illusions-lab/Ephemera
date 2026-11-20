package orders

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/actions/scaleset"
)

type Runners struct {
	client *scaleset.Client
}

func NewRunners(cfg Config) (Runners, error) {
	client, err := newGitHubClient(cfg, scaleset.SystemInfo{System: "ephemera", Subsystem: "runners"}, scaleset.WithRetryMax(-1))
	return Runners{client: client}, err
}

func (r Runners) Register(ctx context.Context, set int, name string) (int, string, error) {
	jit, err := r.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name: name, WorkFolder: "_work",
	}, set)
	// SDK errors can contain response bodies. A partial response may contain JIT data.
	if err != nil {
		return 0, "", errors.New("GitHub JIT registration failed")
	}
	if jit == nil || jit.Runner == nil || jit.Runner.ID <= 0 || jit.Runner.Name != name ||
		(jit.Runner.RunnerScaleSetID != 0 && jit.Runner.RunnerScaleSetID != set) || jit.EncodedJITConfig == "" {
		return 0, "", errors.New("GitHub returned an invalid JIT registration")
	}
	return jit.Runner.ID, jit.EncodedJITConfig, nil
}

var ErrRunnerBusy = errors.New("GitHub runner is executing a job")

func (r Runners) Remove(ctx context.Context, set, id int, name string) error {
	var runner *scaleset.RunnerReference
	var err error
	if id > 0 {
		runner, err = r.client.GetRunner(ctx, id)
	} else {
		runner, err = r.client.GetRunnerByName(ctx, url.QueryEscape(name))
	}
	if missingRunner(err) || (err == nil && runner == nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up runner: %w", err)
	}
	if runner.ID <= 0 || runner.Name != name || runner.RunnerScaleSetID != set || (id > 0 && runner.ID != id) {
		return errors.New("GitHub runner identity does not match instance record")
	}
	err = r.client.RemoveRunner(ctx, int64(runner.ID))
	switch {
	case missingRunner(err):
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		return ErrRunnerBusy
	default:
		return err
	}
}

func missingRunner(err error) bool {
	// The SDK also returns plain HTTP 404s without a typed exception.
	return err != nil && (errors.Is(err, scaleset.RunnerNotFoundError) ||
		strings.Contains(err.Error(), `(status="404 `))
}
