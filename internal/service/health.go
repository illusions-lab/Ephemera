package service

import (
	"fmt"
	"log/slog"
	"net"
	"os"
)

func reportHealth(logger *slog.Logger, state string) error {
	if socket := os.Getenv("NOTIFY_SOCKET"); socket != "" {
		conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
		if err != nil {
			return fmt.Errorf("notify systemd: %w", err)
		}
		defer conn.Close()
		message := "STATUS=" + state
		switch state {
		case "Healthy":
			message += "\nREADY=1"
		case "Stopping":
			message += "\nSTOPPING=1"
		}
		if _, err := conn.Write([]byte(message)); err != nil {
			return fmt.Errorf("notify systemd: %w", err)
		}
	}
	logger.Info("Service health", "state", state)
	return nil
}
