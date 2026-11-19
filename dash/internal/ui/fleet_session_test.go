package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func sessionRow(session string) vm.FleetCrewRow {
	return vm.FleetCrewRow{Dispatcher: &vm.FleetCrewDispatcher{State: "live", Session: session}}
}

func TestCrewSessionLabel(t *testing.T) {
	sub := sessionRow(vm.SessionNeedsYou)
	sub.Sub = true
	for _, tc := range []struct {
		name string
		row  vm.FleetCrewRow
		want string
	}{
		{"needs you", sessionRow(vm.SessionNeedsYou), "needs you"},
		{"standby", sessionRow(vm.SessionStandby), "standby"},
		{"idle", sessionRow(vm.SessionIdle), "awaiting you"},
		{"working", sessionRow(vm.SessionWorking), ""},
		{"unknown", sessionRow(vm.SessionUnknown), ""},
		{"unset", sessionRow(""), ""},
		{"sub row", sub, ""},
		{"peer row", vm.FleetCrewRow{}, ""},
	} {
		if got := crewSession(tc.row); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDispatcherGlyphNeedsYouIsBlocked(t *testing.T) {
	if got := dispatcherGlyphState(sessionRow(vm.SessionNeedsYou)); got != "blocked" {
		t.Errorf("needs_you glyph state = %q", got)
	}
	if got := dispatcherGlyphState(sessionRow(vm.SessionStandby)); got != "working" {
		t.Errorf("standby glyph state = %q", got)
	}
}

// longLabelInputs is the fleet fixture with a long host and repo on every
// repo, so head labels reach the label column's cap, and with c1 of repo a
// waiting on the operator and c5 of repo b idle behind a worker.
func longLabelInputs(withPaneStates bool) vm.FleetInputs {
	in := viewtest.FleetInputs()
	for i := range in.Fleet.Status.Repos {
		r := &in.Fleet.Status.Repos[i]
		r.Host, r.Repo = "workstation", "noamsto/dispatcher-long"
		if !withPaneStates {
			continue
		}
		for j := range r.Crews {
			c := &r.Crews[j]
			switch {
			case r.Common == "/r/a/.git" && c.CrewID == "c1":
				c.Dispatcher.PaneState = new("waiting")
			case r.Common == "/r/b/.git" && c.CrewID == "c5":
				c.Dispatcher.PaneState = new("idle")
			}
		}
	}
	return in
}

var (
	crewCountsRe   = regexp.MustCompile(`\s(\d+(?:w|b|pr|d|f|x|m|\?))(?:\s|$)`)
	sessionLabelRe = regexp.MustCompile(`needs you|standby|awaiting you`)
)

// crewsPanelRows is the text of each Crews panel body row, without borders.
func crewsPanelRows(t *testing.T, out string) []string {
	t.Helper()
	var rows []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, "CREWS"):
			in = true
		case in && strings.HasPrefix(l, "╰"):
			return rows
		case in:
			body := strings.TrimPrefix(l, "│")
			rows = append(rows, body[:max(0, strings.Index(body, "│"))])
		}
	}
	t.Fatalf("no Crews panel in:\n%s", out)
	return nil
}

// countsCells is the display cell where each worker-counts column starts, one
// per row that has counts.
func countsCells(rows []string) []int {
	var cells []int
	for _, r := range rows {
		if loc := crewCountsRe.FindStringSubmatchIndex(r); loc != nil {
			cells = append(cells, ansi.StringWidth(r[:loc[2]]))
		}
	}
	return cells
}

func rowFor(t *testing.T, rows []string, name string) string {
	t.Helper()
	for _, r := range rows {
		if strings.Contains(r, name) {
			return r
		}
	}
	t.Fatalf("no row containing %q in %q", name, rows)
	return ""
}

func fleetSessionSizes() [][2]int { return [][2]int{{80, 24}, {100, 30}, {120, 40}} }

// fleetSessionView renders fv; at 80 columns the layout shows one panel, so
// the Crews panel is focused there.
func fleetSessionView(fv vm.FleetView, w, h int) string {
	v := fleetFixtureView(fv)
	if w < 100 {
		v.st.focus = fpCrews
	}
	return v.View(w, h)
}

func TestFleetSessionColumnKeepsMarks(t *testing.T) {
	asciiProfile(t)
	with := vm.BuildFleet(longLabelInputs(true))
	without := vm.BuildFleet(longLabelInputs(false))
	for _, sz := range fleetSessionSizes() {
		w, h := sz[0], sz[1]
		rowsWith := crewsPanelRows(t, fleetSessionView(with, w, h))
		rowsWithout := crewsPanelRows(t, fleetSessionView(without, w, h))

		if got := strings.Join(rowsWith, "\n"); !strings.Contains(got, "needs you") {
			t.Errorf("%dx%d: no needs you label:\n%s", w, h, got)
		}
		if got := strings.Join(rowsWith, "\n"); !strings.Contains(got, "standby") {
			t.Errorf("%dx%d: no standby label:\n%s", w, h, got)
		}

		cells := countsCells(rowsWith)
		if len(cells) < 5 {
			t.Fatalf("%dx%d: only %d rows with counts: %q", w, h, len(cells), rowsWith)
		}
		for _, c := range cells[1:] {
			if c != cells[0] {
				t.Errorf("%dx%d: counts column starts at %v, want one cell:\n%s", w, h, cells, strings.Join(rowsWith, "\n"))
				break
			}
		}

		// A waiting dispatcher adds one attention item to c1 (4 to 5).
		for _, mark := range []struct{ row, base, text string }{
			{"c1 @", " 4!", " 5!"}, {"c5 @", " 2!", " 2!"}, {"c3 @", " 1! orphaned", " 1! orphaned"},
		} {
			if !strings.HasSuffix(strings.TrimRight(rowFor(t, rowsWithout, mark.row), " "), mark.base) {
				t.Fatalf("%dx%d: baseline row %q does not end in %q", w, h, mark.row, mark.base)
			}
			if got := rowFor(t, rowsWith, mark.row); !strings.HasSuffix(strings.TrimRight(got, " "), mark.text) {
				t.Errorf("%dx%d: row %q does not end in %q after the session column: %q", w, h, mark.row, mark.text, got)
			}
		}
	}
}

func TestFleetSessionColumnWidthIsFixed(t *testing.T) {
	asciiProfile(t)
	const w, h = 120, 40
	starts := map[string]int{}
	for name, session := range map[string]string{
		"needs you":    vm.SessionNeedsYou,
		"standby":      vm.SessionStandby,
		"awaiting you": vm.SessionIdle,
	} {
		fv := vm.BuildFleet(viewtest.FleetInputs())
		for i := range fv.Crews {
			if d := fv.Crews[i].Dispatcher; d != nil && !fv.Crews[i].Sub && d.State == "live" {
				d.Session = session
			}
		}
		rows := crewsPanelRows(t, fleetSessionView(fv, w, h))
		if !sessionLabelRe.MatchString(strings.Join(rows, "\n")) {
			t.Fatalf("%s: no label rendered:\n%s", name, strings.Join(rows, "\n"))
		}
		cells := countsCells(rows)
		starts[name] = cells[0]
		for _, c := range cells[1:] {
			if c != cells[0] {
				t.Errorf("%s: counts column not aligned: %v", name, cells)
			}
		}
	}
	for name, c := range starts {
		if c != starts["awaiting you"] {
			t.Errorf("%s: counts start at %d, awaiting you at %d", name, c, starts["awaiting you"])
		}
	}
}

func TestFleetSessionGolden(t *testing.T) {
	asciiProfile(t)
	fv := vm.BuildFleet(longLabelInputs(true))
	checkGolden(t, "fleet_session_100x30.golden", fleetSessionView(fv, 100, 30), 100, 30)
}
