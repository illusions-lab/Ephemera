package orders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
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
	found             *scaleset.RunnerScaleSet
	lookup            string
	created           *scaleset.RunnerScaleSet
	updated           *scaleset.RunnerScaleSet
	groupName         string
	groupID           int
	ignoreLabelUpdate bool
}

func (s *fakeScaleSets) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	s.groupName = name
	return &scaleset.RunnerGroup{ID: 7}, nil
}

func (s *fakeScaleSets) UpdateRunnerScaleSet(_ context.Context, id int, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	s.updated = set
	result := *s.found
	result.ID, result.Labels = id, set.Labels
	if !s.ignoreLabelUpdate {
		s.found = &result
	}
	return &result, nil
}

func (s *fakeScaleSets) GetRunnerScaleSet(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	s.lookup, s.groupID = name, groupID
	return s.found, nil
}

func (s *fakeScaleSets) CreateRunnerScaleSet(_ context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	s.created, s.found = set, set
	return set, nil
}

func TestFindScaleSetCreatesOnlyWhenRequestedAndReusesExisting(t *testing.T) {
	const name = "test+linux"
	ctx := context.Background()
	api := &fakeScaleSets{}
	if _, err := findScaleSet(ctx, api, Config{ScaleSetName: name}); err == nil || api.created != nil {
		t.Fatal("missing set should require explicit creation")
	}
	set, err := findScaleSet(ctx, api, Config{ScaleSetName: name, Create: true})
	if err != nil || set.Name != name || set.RunnerGroupID != 1 ||
		!reflect.DeepEqual(set.Labels, []scaleset.Label{{Name: name}}) || api.lookup != "test%2Blinux" {
		t.Fatalf("set = %+v, lookup = %q, err = %v", set, api.lookup, err)
	}

	api = &fakeScaleSets{found: &scaleset.RunnerScaleSet{ID: 42, Name: name, Labels: []scaleset.Label{{Name: name}}}}
	set, err = findScaleSet(ctx, api, Config{ScaleSetName: name, Create: true})
	if err != nil || set != api.found || api.created != nil {
		t.Fatalf("existing set should be reused, got %+v, err = %v", set, err)
	}
}

func TestFindScaleSetUpdatesLabelsInSelectedGroup(t *testing.T) {
	api := &fakeScaleSets{found: &scaleset.RunnerScaleSet{ID: 42, Name: "mac-build",
		Labels: []scaleset.Label{{Name: "mac-build"}, {Name: "manual"}},
	}}
	cfg := Config{ScaleSetName: "mac-build", RunnerGroup: "Dedicated Macs", Create: true,
		Labels: []string{"self-hosted", "macOS", "ARM64", "MACOS", "build"},
	}
	set, err := findScaleSet(t.Context(), api, cfg)
	want := []scaleset.Label{{Name: "mac-build"}, {Name: "self-hosted"}, {Name: "macOS"}, {Name: "ARM64"}, {Name: "build"}, {Name: "manual"}}
	if err != nil || set.ID != 42 || api.groupName != "Dedicated Macs" || api.groupID != 7 ||
		api.created != nil || !reflect.DeepEqual(set.Labels, want) {
		t.Fatalf("set = %+v, API = %+v, error = %v", set, api, err)
	}
	api.updated = nil
	if _, err := findScaleSet(t.Context(), api, cfg); err != nil || api.updated != nil {
		t.Fatalf("unchanged labels triggered an update: %v", err)
	}
	cfg.ReplaceLabels = true
	set, err = findScaleSet(t.Context(), api, cfg)
	if err != nil || !reflect.DeepEqual(set.Labels, want[:len(want)-1]) {
		t.Fatalf("explicit labels did not remove old labels: set = %+v, error = %v", set, err)
	}
}

func TestScaleSetLabelRegistrationAndIgnoredUpdate(t *testing.T) {
	cfg := Config{ScaleSetName: "mac-build", Create: true, Labels: []string{"self-hosted", "macOS", "ARM64"}}
	want := []scaleset.Label{{Name: "mac-build"}, {Name: "self-hosted"}, {Name: "macOS"}, {Name: "ARM64"}}
	api := &fakeScaleSets{}
	set, err := findScaleSet(t.Context(), api, cfg)
	if err != nil || api.created == nil || !reflect.DeepEqual(api.created.Labels, want) {
		t.Fatalf("create did not send the workflow labels: %+v, %v", api.created, err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	reportScaleSetLabels(logger, cfg, set)
	if output.Len() != 0 {
		t.Fatalf("effective labels unexpectedly triggered a warning: %s", &output)
	}

	api = &fakeScaleSets{ignoreLabelUpdate: true, found: &scaleset.RunnerScaleSet{
		ID: 42, Name: "mac-build", Labels: []scaleset.Label{{Name: "mac-build"}},
	}}
	set, err = findScaleSet(t.Context(), api, cfg)
	if err != nil || api.updated == nil || !reflect.DeepEqual(api.updated.Labels, want) || len(set.Labels) != 1 {
		t.Fatalf("ignored update did not preserve the GitHub response: %+v, %v", set, err)
	}
	if err := verifyScaleSetLabels(cfg, set); err == nil || !strings.Contains(err.Error(), "self-hosted, macOS, ARM64") {
		t.Fatalf("expected a routing error for the persisted labels, got %v", err)
	}
	reportScaleSetLabels(logger, cfg, set)
	if !strings.Contains(output.String(), "GitHub did not return all requested Scale Set labels") ||
		!strings.Contains(output.String(), "effective_labels=[mac-build]") {
		t.Fatalf("missing label warning: %s", &output)
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

type sequenceSession struct {
	fakeSession
	messages   []*scaleset.RunnerScaleSetMessage
	requests   [][]int64
	capacities []int
	acquireErr error
	next       int
}

func (s *sequenceSession) GetMessage(_ context.Context, _, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.capacities = append(s.capacities, capacity)
	if s.next >= len(s.messages) {
		return nil, errors.New("end of messages")
	}
	message := s.messages[s.next]
	s.next++
	return message, nil
}

func (s *sequenceSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.requests = append(s.requests, ids)
	return ids, s.acquireErr
}

func (s *sequenceSession) DeleteMessage(context.Context, int) error {
	s.calls = append(s.calls, "ack")
	return nil
}

func TestExecutionPollReplaysAndTracksPendingAcquisitions(t *testing.T) {
	s := &sequenceSession{
		fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{TotalAcquiredJobs: 1, TotalAssignedJobs: 1}},
		messages: []*scaleset.RunnerScaleSetMessage{
			{MessageID: 1, JobAvailableMessages: []*scaleset.JobAvailable{offer(10), offer(20)}},
			{MessageID: 1, JobAvailableMessages: []*scaleset.JobAvailable{offer(10), offer(20)}},
			{MessageID: 2, JobAvailableMessages: []*scaleset.JobAvailable{offer(20)}},
			{MessageID: 3, Statistics: &scaleset.RunnerScaleSetStatistic{}, JobAvailableMessages: []*scaleset.JobAvailable{offer(10), offer(20)}},
		},
	}
	handle := func(context.Context, Update) error { return nil }
	_ = pollWithCapacity(context.Background(), s, 2, false, nil, func() int { return 1 }, handle)
	if want := [][]int64{{10}, {20}}; !reflect.DeepEqual(want, s.requests) || len(s.calls) != 4 || s.capacities[0] != 2 {
		t.Fatalf("acquisition requests = %v, acknowledgments = %v, capacity = %v", s.requests, s.calls, s.capacities)
	}
}

func TestExecutionPollLeavesAmbiguousAcquisitionUnacknowledged(t *testing.T) {
	failure := errors.New("connection dropped after sending request")
	s := &sequenceSession{
		fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{}},
		messages: []*scaleset.RunnerScaleSetMessage{
			{MessageID: 1, JobAvailableMessages: []*scaleset.JobAvailable{offer(1)}},
		},
		acquireErr: failure,
	}
	err := pollWithCapacity(context.Background(), s, 2, false, nil, func() int { return 1 }, func(context.Context, Update) error { return nil })
	if !errors.Is(err, failure) || len(s.calls) != 0 {
		t.Fatalf("poll error = %v, acknowledgments = %v", err, s.calls)
	}
}

func TestAwaitSessionHandoffRetriesOnlyOwnerConflict(t *testing.T) {
	ownerConflict := errors.New("409 Conflict: RunnerScaleSetSessionConflictException")
	for _, firstError := range []error{ownerConflict, errors.New("409 Conflict: unrelated conflict")} {
		attempts, notices := 0, 0
		err := awaitSessionHandoff(t.Context(), 0, func(context.Context) error {
			attempts++
			if attempts < 3 {
				return firstError
			}
			return nil
		}, func() { notices++ })
		if firstError == ownerConflict {
			if err != nil || attempts != 3 || notices != 1 {
				t.Fatalf("handoff: %v, %d attempts, %d notices", err, attempts, notices)
			}
		} else if !errors.Is(err, firstError) || attempts != 1 || notices != 0 {
			t.Fatalf("unexpected conflict retry: %v, %d attempts, %d notices", err, attempts, notices)
		}
	}
}

func TestValidateMessageQueueRejectsEmptyBrokerEndpoint(t *testing.T) {
	var s scaleset.RunnerScaleSetSession
	if err := json.Unmarshal([]byte(`{"statistics":{}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Statistics == nil {
		t.Fatal("expected broker statistics to decode without the queue field")
	}
	if err := validateMessageQueue(s); err == nil || !strings.Contains(err.Error(), "empty messageQueueUrl") {
		t.Fatalf("expected a precise empty queue error, got %v", err)
	}
	s.MessageQueueURL = "http://broker.example"
	if err := validateMessageQueue(s); err == nil {
		t.Fatal("accepted an insecure broker endpoint")
	}
	s.MessageQueueURL = "https://broker.example/queue"
	if err := validateMessageQueue(s); err == nil {
		t.Fatal("accepted a session without a queue token")
	}
	s.MessageQueueAccessToken = "synthetic"
	if err := validateMessageQueue(s); err != nil {
		t.Fatal(err)
	}
}
