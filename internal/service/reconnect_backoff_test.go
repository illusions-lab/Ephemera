package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/orders"
)

func TestRepeatedFirstPollFailuresIncreaseReconnectDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	attempts := make([]time.Time, 0, 4)
	poll := func(_ context.Context, cfg orders.Config, _ *slog.Logger, _ func(context.Context, orders.Update) error) error {
		attempts = append(attempts, time.Now())
		switch len(attempts) {
		case 3:
			// A session that has polled successfully starts a fresh backoff series.
			cfg.Polled()
		case 4:
			cancel()
		}
		return errors.New("poll connection dropped")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listenForExecution(ctx, orders.Config{Disconnected: func() {}}, logger, poll, &reconciler{})
	if len(attempts) != 4 {
		t.Fatalf("expected four attempts, got %d", len(attempts))
	}
	delays := []time.Duration{attempts[1].Sub(attempts[0]), attempts[2].Sub(attempts[1]), attempts[3].Sub(attempts[2])}
	if delays[0] < time.Second || delays[1] < 2*time.Second || delays[2] < time.Second {
		t.Fatalf("unexpected reconnect delays: %v", delays)
	}
	if delays[2] >= 2*time.Second {
		t.Fatalf("successful poll did not reset backoff: %v", delays)
	}
}
