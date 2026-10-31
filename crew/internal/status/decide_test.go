package status

import (
	"bytes"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

func (f *fixture) decide(text string) (int, string) {
	var stderr bytes.Buffer
	code := RunDecide(f.t.Context(), text, f.paths, &stderr, f.opts())
	return code, stderr.String()
}

func TestDecideAppendsADispatcherRow(t *testing.T) {
	f := newFixture(t)
	if code, stderr := f.decide("use sqlite"); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(`{"ts":` + tsText + `,"crew_id":"c1","from":"dispatcher:c1","kind":"decision","body":"use sqlite"}`)
	if got := f.lines()[0]; got != peer.DecisionRow(1700000000500, "c1", "use sqlite") {
		t.Errorf("row differs from peer.DecisionRow: %s", got)
	}
}

func TestDecideRefusesWorkerAndRole(t *testing.T) {
	for key, val := range map[string]string{"CREW_WORKER_ID": worker, "CREW_ROLE_ID": rev} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			f.env[key] = val
			code, stderr := f.decide("x")
			want := "crew: decide: refusing sender 'dispatcher:c1' — " + sender.Check(t.Context(), "dispatcher:c1", crewID, f.opts().Sender) + "\n"
			if code != 1 || stderr != want {
				t.Errorf("got %d %q", code, stderr)
			}
			f.wantNoRow()
		})
	}
	t.Run("stamped pane", func(t *testing.T) {
		f := newFixture(t)
		f.anchored(rolePane)
		code, stderr := f.decide("x")
		if code != 1 || !strings.HasPrefix(stderr, "crew: decide: refusing sender 'dispatcher:c1' — ") {
			t.Errorf("got %d %q", code, stderr)
		}
		f.wantNoRow()
	})
}

func TestDecideRefusesEmptyAndOversize(t *testing.T) {
	for name, text := range map[string]string{
		"empty":    "",
		"oversize": strings.Repeat("a", peer.MaxBody+1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.decide(text)
			if code != 1 || !strings.HasPrefix(stderr, "crew: decide: ") {
				t.Errorf("got %d %q", code, stderr)
			}
			f.wantNoRow()
		})
	}
}

func TestDecideRefusesALineOverTheBusLimit(t *testing.T) {
	f := newFixture(t)
	// 3000 control bytes encode to 6 bytes each, past bus.LineMax, under MaxBody.
	code, stderr := f.decide(strings.Repeat("\x01", 3000))
	if code != 1 || !strings.Contains(stderr, "bus line") {
		t.Errorf("got %d %q", code, stderr)
	}
	f.wantNoRow()
}
