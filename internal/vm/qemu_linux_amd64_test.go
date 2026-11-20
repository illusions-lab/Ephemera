package vm

import (
	"io"
	"strings"
	"testing"
)

func TestReadinessRequiresCompletePinnedRunnerLine(t *testing.T) {
	ready := make(chan struct{})
	parser := &readiness{ready: ready}
	for _, fragment := range []string{
		"output mentioning Listening for Jobs\r\n",
		strings.Repeat("x", 257) + "2026-11-20 12:00:00Z: Listening for Jobs\r\n",
		"2026-11-20 12:00:00Z: Listening for",
	} {
		if _, err := io.WriteString(parser, fragment); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ready:
			t.Fatal("accepted an invalid or partial readiness line")
		default:
		}
	}
	_, _ = io.WriteString(parser, " Jobs\r\n")
	select {
	case <-ready:
	default:
		t.Fatal("did not accept complete runner readiness line")
	}
}
