package orders

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/actions/scaleset"
)

type fakeSession struct {
	initial *scaleset.RunnerScaleSetStatistic
	message *scaleset.RunnerScaleSetMessage
	stop    context.CancelFunc
	calls   []string
	claimed []int64
	pollCap int
}

func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{Statistics: s.initial}
}

func (s *fakeSession) GetMessage(_ context.Context, cursor, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.calls = append(s.calls, "poll")
	s.pollCap = capacity
	return s.message, nil
}

func (s *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.calls = append(s.calls, "claim")
	s.claimed = ids
	return ids[:1], nil
}

func (s *fakeSession) DeleteMessage(ctx context.Context, id int) error {
	if ctx.Err() != nil || id != s.message.MessageID {
		return errors.New("invalid acknowledgment")
	}
	s.calls = append(s.calls, "ack")
	s.stop()
	return nil
}

func TestPollClaimsWithinCapacityAndAcksAfterHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeSession{
		initial: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 4},
		message: &scaleset.RunnerScaleSetMessage{
			MessageID: 12,
			Statistics: &scaleset.RunnerScaleSetStatistic{
				TotalAssignedJobs: 1, TotalRunningJobs: 1,
			},
			JobAvailableMessages: []*scaleset.JobAvailable{offer(1), offer(1), offer(2), offer(3)},
			JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "runner-1"}},
		},
		stop: cancel,
	}
	var updates []Update
	err := poll(ctx, s, 3, true, nil, func(_ context.Context, update Update) error {
		s.calls = append(s.calls, "handle")
		updates = append(updates, update)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll returned %v", err)
	}
	if want := []string{"handle", "poll", "claim", "handle", "ack"}; !reflect.DeepEqual(s.calls, want) {
		t.Fatalf("calls = %v, want %v", s.calls, want)
	}
	if s.pollCap != 3 || !reflect.DeepEqual(s.claimed, []int64{1, 2}) {
		t.Fatalf("capacity = %d, claimed = %v", s.pollCap, s.claimed)
	}
	if updates[0].TargetRunners != 3 || updates[1].TargetRunners != 1 ||
		!reflect.DeepEqual(updates[1].Acquired, []int64{1}) || len(updates[1].Started) != 1 {
		t.Fatalf("unexpected updates: %+v", updates)
	}
}

func TestPollObservationDoesNotClaimOrAckOnHandlerError(t *testing.T) {
	s := &fakeSession{
		initial: &scaleset.RunnerScaleSetStatistic{},
		message: &scaleset.RunnerScaleSetMessage{
			MessageID:            1,
			Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAvailableJobs: 1},
			JobAvailableMessages: []*scaleset.JobAvailable{offer(1)},
		},
	}
	failure := errors.New("handler failed")
	err := poll(context.Background(), s, 2, false, nil, func(_ context.Context, update Update) error {
		if update.AvailableJobs == 1 {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) || s.pollCap != 0 || !reflect.DeepEqual(s.calls, []string{"poll"}) {
		t.Fatalf("err = %v, capacity = %d, calls = %v", err, s.pollCap, s.calls)
	}
}

func offer(id int64) *scaleset.JobAvailable {
	return &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: id}}
}

type fakeScaleSets struct {
	found   *scaleset.RunnerScaleSet
	lookup  string
	created *scaleset.RunnerScaleSet
}

func (s *fakeScaleSets) GetRunnerScaleSet(_ context.Context, _ int, name string) (*scaleset.RunnerScaleSet, error) {
	s.lookup = name
	return s.found, nil
}

func (s *fakeScaleSets) CreateRunnerScaleSet(_ context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	s.created = set
	return set, nil
}

func TestFindScaleSetCreatesOnlyWhenRequestedAndReusesExisting(t *testing.T) {
	const name = "test+linux"
	ctx := context.Background()
	api := &fakeScaleSets{}
	if _, err := findScaleSet(ctx, api, name, false); err == nil || api.created != nil {
		t.Fatal("missing set should require explicit creation")
	}
	set, err := findScaleSet(ctx, api, name, true)
	if err != nil || set.Name != name || set.RunnerGroupID != 1 ||
		!reflect.DeepEqual(set.Labels, []scaleset.Label{{Name: name}}) || api.lookup != "test%2Blinux" {
		t.Fatalf("set = %+v, lookup = %q, err = %v", set, api.lookup, err)
	}

	api = &fakeScaleSets{found: &scaleset.RunnerScaleSet{ID: 42, Name: name}}
	set, err = findScaleSet(ctx, api, name, true)
	if err != nil || set != api.found || api.created != nil {
		t.Fatalf("existing set should be reused, got %+v, err = %v", set, err)
	}
}

func TestPollStopsBeforeClaimWhenImageDisappears(t *testing.T) {
	s := &fakeSession{
		initial: &scaleset.RunnerScaleSetStatistic{},
		message: &scaleset.RunnerScaleSetMessage{
			MessageID:            9,
			Statistics:           &scaleset.RunnerScaleSetStatistic{},
			JobAvailableMessages: []*scaleset.JobAvailable{offer(1)},
		},
	}
	failure := errors.New("image disappeared")
	checks := 0
	err := poll(context.Background(), s, 2, true, func() error {
		checks++
		if checks > 1 {
			return failure
		}
		return nil
	}, func(context.Context, Update) error { return nil })
	if !errors.Is(err, failure) || !reflect.DeepEqual(s.calls, []string{"poll"}) {
		t.Fatalf("err = %v, calls = %v; expected polling to stop without acquisition", err, s.calls)
	}
}
