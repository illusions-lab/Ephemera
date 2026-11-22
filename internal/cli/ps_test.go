package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/illusions-lab/Ephemera/internal/status"
)

func TestPrintInstances(t *testing.T) {
	now := time.Now()
	rows := []status.Instance{
		{ID: "a", State: "waiting", CreatedAt: now.Add(-time.Minute)},
		{ID: "b", State: "reclaiming"},
		{ID: "c", State: "running", CreatedAt: now},
		{ID: "d", State: "pending-deregistration"},
		{ID: "e", State: "finishing", CreatedAt: now},
		{ID: "f", State: "starting", CreatedAt: now},
	}
	for _, test := range []struct {
		all  bool
		want string
	}{
		{false, "c\ne\nf\na\n"},
		{true, "c\ne\nf\na\nb\nd\n"},
	} {
		var out bytes.Buffer
		if err := printInstances(&out, rows, test.all, true, now); err != nil || out.String() != test.want {
			t.Fatalf("all=%v: output %q, want %q, error=%v", test.all, out.String(), test.want, err)
		}
	}
	var out bytes.Buffer
	if err := printInstances(&out, rows, true, false, now); err != nil ||
		!strings.Contains(out.String(), "unknown") || !strings.Contains(out.String(), "1m0s") {
		t.Fatalf("ages: output %q, error=%v", out.String(), err)
	}
}
