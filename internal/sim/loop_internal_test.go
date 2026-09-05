package sim

import (
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// newRunnerForBoundsTest builds a run whose schedule and resolved retryBackoff are enough
// to exercise selectionInputs, without driving a full simulation. selectionInputs is
// unexported (it must stay a private mirror of the controller's selInputs, not a second
// public API), so this test lives in package sim rather than alongside wire_test.go's
// black-box tests in package sim_test.
func newRunnerForBoundsTest(t *testing.T) *run {
	t.Helper()
	sched, err := window.New([]policy.MaintenanceWindow{{
		Timezone: "UTC",
		Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		Start:    "05:00",
		End:      "06:30",
	}})
	if err != nil {
		t.Fatalf("window.New: %v", err)
	}
	return &run{
		sched: sched,
		res:   Resolved{RetryBackoff: 30 * time.Minute},
	}
}

// The simulator drives the same selection predicate as the controller, so it must
// supply the same occurrence bounds. Without them a failed node's retry is not
// clamped and the simulated cadence diverges from the real one (issue #320).
func TestSimSuppliesOccurrenceBoundsToSelection(t *testing.T) {
	r := newRunnerForBoundsTest(t) // 05:00-06:30 UTC window, retryBackoff 30m
	now := time.Date(2026, 9, 4, 5, 30, 0, 0, time.UTC)
	in := r.selectionInputs(now)
	if in.WindowStart.IsZero() || in.WindowEnd.IsZero() {
		t.Fatalf("the simulator must carry the occurrence bounds; got [%v, %v)", in.WindowStart, in.WindowEnd)
	}
	if want := time.Date(2026, 9, 4, 6, 30, 0, 0, time.UTC); !in.WindowEnd.Equal(want) {
		t.Errorf("WindowEnd: got %s, want %s", in.WindowEnd, want)
	}
}
