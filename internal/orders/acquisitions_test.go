package orders

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/actions/scaleset"
)

func TestAcquisitionTrackingAcrossSessions(t *testing.T) {
	tracker := &Acquisitions{}
	lostReply := errors.New("acquisition response lost")
	first := &sequenceSession{
		fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{}},
		messages: []*scaleset.RunnerScaleSetMessage{
			{MessageID: 10, JobAvailableMessages: []*scaleset.JobAvailable{offer(100)}},
		},
		acquireErr: lostReply,
	}
	local := func() int { return 2 }
	if err := pollTracked(t.Context(), first, 2, false, nil, local, nil, tracker, nil, func(context.Context, Update) error { return nil }); !errors.Is(err, lostReply) {
		t.Fatalf("acquisition error = %v", err)
	}
	if tracker.requests[100] != acquisitionUnknown || len(first.calls) != 0 {
		t.Fatalf("lost reply should leave an unacknowledged acquisition: %v, calls %v", tracker.requests, first.calls)
	}

	second := &sequenceSession{
		fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{TotalAcquiredJobs: 1}},
		messages: []*scaleset.RunnerScaleSetMessage{
			{
				MessageID:            10,
				JobAssignedMessages:  []*scaleset.JobAssigned{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 100}}},
				JobAvailableMessages: []*scaleset.JobAvailable{offer(100), offer(200)},
			},
			{
				MessageID:            11,
				Statistics:           &scaleset.RunnerScaleSetStatistic{},
				JobCompletedMessages: []*scaleset.JobCompleted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 100}}},
			},
		},
	}
	handledTerminal := false
	_ = pollTracked(t.Context(), second, 2, false, nil, local, nil, tracker, nil, func(_ context.Context, update Update) error {
		if len(update.Completed) != 0 {
			handledTerminal = true
			if len(second.calls) != 1 || tracker.requests[100] != acquisitionAccepted {
				t.Fatalf("completion should be delivered before ack, with the assigned request tracked: %v, calls %v", tracker.requests, second.calls)
			}
		}
		return nil
	})
	if !handledTerminal || tracker.requests[100] != 0 || tracker.requests[200] != acquisitionAccepted {
		t.Fatalf("completion handled = %t, remaining requests = %v", handledTerminal, tracker.requests)
	}
	if len(second.requests) != 1 || !slices.Equal(second.requests[0], []int64{200}) || len(second.calls) != 2 {
		t.Fatalf("acquisitions = %v, acknowledgments = %v", second.requests, second.calls)
	}
}

func TestFullAcquisitionTableStillProcessesCompletion(t *testing.T) {
	const limit = 65536
	tracker := &Acquisitions{requests: make(map[int64]acquisitionState, limit)}
	for id := int64(1); id <= limit; id++ {
		tracker.requests[id] = acquisitionAccepted
	}
	s := &sequenceSession{
		fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{}},
		messages: []*scaleset.RunnerScaleSetMessage{
			{
				MessageID:            1,
				JobAvailableMessages: []*scaleset.JobAvailable{offer(limit + 1)},
				JobCompletedMessages: []*scaleset.JobCompleted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 1}}},
			},
			{MessageID: 2, JobAvailableMessages: []*scaleset.JobAvailable{offer(limit + 1)}},
		},
	}
	completions := 0
	_ = pollTracked(t.Context(), s, 1, false, nil, func() int { return 1 }, nil, tracker, nil, func(_ context.Context, update Update) error {
		completions += len(update.Completed)
		return nil
	})
	if completions != 1 || !slices.Equal(s.capacities, []int{0, 1, 1}) {
		t.Fatalf("completions = %d, capacities = %v", completions, s.capacities)
	}
	if len(s.requests) != 1 || !slices.Equal(s.requests[0], []int64{limit + 1}) || len(s.calls) != 2 {
		t.Fatalf("acquisitions = %v, acknowledgments = %v", s.requests, s.calls)
	}
}

func TestFirstPollFailureDoesNotReportConnectionSuccess(t *testing.T) {
	for range 3 {
		s := &sequenceSession{fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{}}}
		polled := false
		err := pollSession(t.Context(), s, 1, false, nil, func() int { return 1 }, func() { polled = true },
			func(context.Context, Update) error { return nil })
		if err == nil || polled {
			t.Fatalf("failed first poll returned %v, successful poll reported = %t", err, polled)
		}
	}
}

func TestAcquisitionHonorsLocalBudgetUntilAssignedDemandIsReflected(t *testing.T) {
	s := &sequenceSession{
		fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{}},
		messages: []*scaleset.RunnerScaleSetMessage{
			{MessageID: 1, JobAvailableMessages: []*scaleset.JobAvailable{offer(100)}},
			{MessageID: 2, JobAvailableMessages: []*scaleset.JobAvailable{offer(200)}},
			{
				MessageID: 3, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAcquiredJobs: 1},
				JobAvailableMessages: []*scaleset.JobAvailable{offer(200)},
			},
			{
				MessageID: 4, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAcquiredJobs: 1, TotalAssignedJobs: 1},
				JobAvailableMessages: []*scaleset.JobAvailable{offer(200)},
			},
			{
				MessageID: 5, Statistics: &scaleset.RunnerScaleSetStatistic{},
				JobCompletedMessages: []*scaleset.JobCompleted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 100}}},
				JobAvailableMessages: []*scaleset.JobAvailable{offer(300)},
			},
		},
	}
	slots := 1
	_ = pollTracked(t.Context(), s, 5, false, nil, func() int { return slots }, nil, &Acquisitions{}, nil, func(_ context.Context, update Update) error {
		// When a request is assigned, the reconciler commits the slot to the VM.
		slots = 1 - update.AssignedJobs
		return nil
	})
	if len(s.requests) != 2 || !slices.Equal(s.requests[0], []int64{100}) || !slices.Equal(s.requests[1], []int64{300}) {
		t.Fatalf("acquisitions = %v, want requests 100 and 300", s.requests)
	}
	if !slices.Equal(s.capacities, []int{1, 1, 1, 1, 1, 1}) || len(s.calls) != 5 {
		t.Fatalf("capacities = %v, acknowledgments = %v", s.capacities, s.calls)
	}
}

type lostAcknowledgmentSession struct {
	*sequenceSession
	err error
}

func (s lostAcknowledgmentSession) DeleteMessage(context.Context, int) error { return s.err }

func TestTerminalAcquisitionRemainsTrackedUntilAcknowledged(t *testing.T) {
	const id = 42
	tracker := &Acquisitions{requests: map[int64]acquisitionState{id: acquisitionAccepted}}
	completed := &scaleset.RunnerScaleSetMessage{
		MessageID:            2,
		JobCompletedMessages: []*scaleset.JobCompleted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: id}}},
	}
	freshSession := func() *sequenceSession {
		return &sequenceSession{
			fakeSession: fakeSession{initial: &scaleset.RunnerScaleSetStatistic{}},
			messages:    []*scaleset.RunnerScaleSetMessage{completed},
		}
	}
	local := func() int { return 1 }
	handled := 0
	handle := func(_ context.Context, update Update) error {
		handled += len(update.Completed)
		return nil
	}
	ackErr := errors.New("acknowledgment response lost")
	err := pollTracked(t.Context(), lostAcknowledgmentSession{freshSession(), ackErr}, 1, false, nil, local, nil, tracker, nil, handle)
	if !errors.Is(err, ackErr) || handled != 1 || tracker.requests[id] != acquisitionTerminal {
		t.Fatalf("poll = %v, handled = %d, request state = %d", err, handled, tracker.requests[id])
	}
	_ = pollTracked(t.Context(), freshSession(), 1, false, nil, local, nil, tracker, nil, handle)
	if handled != 2 || tracker.requests[id] != 0 {
		t.Fatalf("replayed completion handled %d times, request state = %d", handled, tracker.requests[id])
	}
}
