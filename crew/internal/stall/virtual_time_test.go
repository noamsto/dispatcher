package stall

import (
	"slices"
	"strconv"
	"testing"
)

// The CREW_CLOCK case: a watch configured for ten real minutes runs its whole
// life on the virtual clock, so the harness never sleeps, and the detail from
// two virtual minutes in still rides the bus. The host load read is scripted
// empty (the bats case pinned the same through CREW_STALL_LOAD_CMD): D4 must
// not answer during its 300s window while the rest of the run is walked.

func TestTenMinuteWatchOnVirtualTime(t *testing.T) {
	req := Request{
		Pane: ptr("%9"), Interval: ptr("60"), Window: ptr("600"), Stall: ptr("120"),
		Idle: ptr("99999"), Dead: ptr("99999"), MaxLife: ptr("600"),
	}
	h, rows := watchRun(t, watchScript{frames: []string{"idle_box.txt"}, req: req})
	if want := []string{leadID + "|blocked|watchdog|stalled: no output for 120s"}; !slices.Equal(rows, want) {
		t.Fatalf("rows\n %q\nwant\n %q", rows, want)
	}
	// The run reached its --max-life: 600 virtual seconds, in no real ones.
	if got, want := h.clockText(), strconv.FormatInt(h.seed+600, 10); got != want {
		t.Errorf("clock %s, want %s (ten virtual minutes)", got, want)
	}
}
