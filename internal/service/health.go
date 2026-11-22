package service

import (
	"log/slog"

	"github.com/illusions-lab/Ephemera/internal/host"
)

func reportHealth(logger *slog.Logger, state, failedPhase string) error {
	if err := host.NotifyHealth(state, failedPhase); err != nil {
		return err
	}
	logger.Info("Service health", "state", state)
	return nil
}
