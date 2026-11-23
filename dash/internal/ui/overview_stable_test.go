package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// agedOverview is the fixture overview with every age pinned to base+i
// seconds at its own clock, so two calls differ only by the elapsed time.
func agedOverview(t *testing.T, base, elapsed int64) (vm.OverviewView, overviewState) {
	t.Helper()
	v := newOverviewView(narrowInputs(t), overviewNow)
	ov := v.model
	ov.GeneratedAt = viewtest.NowMS/1000 + elapsed
	ov.Events = append([]vm.EventRow(nil), ov.Events...)
	for i := range ov.Events {
		ov.Events[i].TS = ov.GeneratedAt - base - int64(i)
	}
	ov.Attention = append([]vm.AttentionItem(nil), ov.Attention...)
	for i := range ov.Attention {
		ov.Attention[i].AgeSec = base + elapsed + int64(i)
	}
	ov.Monitors.Watchdogs = append([]vm.WatchdogRow(nil), ov.Monitors.Watchdogs...)
	for i := range ov.Monitors.Watchdogs {
		ov.Monitors.Watchdogs[i].Label = "ok " + vm.Age(base+elapsed+int64(i))
	}
	return ov, v.st
}

func renderAged(t *testing.T, w, h int, base, elapsed int64) []string {
	t.Helper()
	ov, st := agedOverview(t, base, elapsed)
	return strings.Split(renderOverview(w, h, ov, st, pillRound), "\n")
}

// The events panel only fits at the taller sizes.
var stableSizes = [][2]int{{60, 30}, {60, 200}, {150, 40}, {200, 60}}

func TestOverviewRowsStableWithinAgeBucket(t *testing.T) {
	trueColor(t)
	for _, sz := range stableSizes {
		a := renderAged(t, sz[0], sz[1], 125, 0)
		b := renderAged(t, sz[0], sz[1], 125, 20)
		if len(a) != len(b) {
			t.Fatalf("%dx%d: %d vs %d lines", sz[0], sz[1], len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%dx%d line %d changed within one bucket:\n%q\n%q", sz[0], sz[1], i, a[i], b[i])
			}
		}
	}
	if x, y := vm.Age(125), vm.Age(145); x != y {
		t.Errorf("peer_stale lag text %q -> %q within one bucket", x, y)
	}
}

func TestOverviewAgeChangesStayInAgeColumn(t *testing.T) {
	asciiProfile(t)
	for _, sz := range stableSizes {
		a := renderAged(t, sz[0], sz[1], 125, 0)
		b := renderAged(t, sz[0], sz[1], 125, 60)
		changed := map[string]bool{}
		for i := range a {
			ra, rb := []rune(ansi.Strip(a[i])), []rune(ansi.Strip(b[i]))
			if len(ra) != len(rb) {
				t.Errorf("%dx%d line %d width changed: %q -> %q", sz[0], sz[1], i, string(ra), string(rb))
				continue
			}
			lo, hi := -1, -1
			for c := range ra {
				if ra[c] != rb[c] {
					if lo < 0 {
						lo = c
					}
					hi = c
				}
			}
			if lo < 0 {
				continue
			}
			if hi-lo >= 4 {
				t.Errorf("%dx%d line %d differs beyond a 4-cell age column (cols %d-%d):\n%q\n%q",
					sz[0], sz[1], i, lo, hi, string(ra), string(rb))
			}
			switch {
			case strings.Contains(string(ra), "ok 2m"):
				changed["watch"] = true
			case strings.Contains(string(ra), "  2m "):
				changed["row"] = true
			}
		}
		if !changed["watch"] {
			t.Errorf("%dx%d: no MONITORS watch line aged", sz[0], sz[1])
		}
		if !changed["row"] {
			t.Errorf("%dx%d: no event or attention row aged", sz[0], sz[1])
		}
	}
}

func TestOverviewEventColumnsAlign(t *testing.T) {
	asciiProfile(t)
	ages := []int64{3, 125, 700, 3000, 4000, 8000}
	for _, sz := range [][2]int{{60, 200}, {200, 60}} {
		ov, st := agedOverview(t, 0, 0)
		for i := range ov.Events {
			ov.Events[i].TS = ov.GeneratedAt - ages[i%len(ages)]
		}
		lines := strings.Split(ansi.Strip(renderOverview(sz[0], sz[1], ov, st, pillRound)), "\n")
		start := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "RECENT EVENTS") })
		if start < 0 {
			t.Fatalf("%dx%d: no events panel", sz[0], sz[1])
		}
		var cols []int
		for i, e := range ov.Events {
			if start+1+i >= len(lines) {
				break
			}
			ln := lines[start+1+i]
			prefix := string([]rune(e.Text)[:min(8, len([]rune(e.Text)))])
			j := strings.LastIndex(ln, prefix)
			if j < 0 {
				continue
			}
			cols = append(cols, ansi.StringWidth(ln[:j]))
		}
		if len(cols) < 5 {
			t.Fatalf("%dx%d: found %d event rows", sz[0], sz[1], len(cols))
		}
		for _, c := range cols {
			if c != cols[0] {
				t.Errorf("%dx%d: event text columns differ: %v", sz[0], sz[1], cols)
				break
			}
		}
	}
}

func TestOverviewCrewColumnOnlyWhenUnscoped(t *testing.T) {
	asciiProfile(t)
	ov, st := agedOverview(t, 125, 0)
	ov.Events[0].Crew = "crew-abcdefghij"
	out := ansi.Strip(renderOverview(200, 60, ov, st, pillRound))
	if !strings.Contains(out, "crew-abc ") || strings.Contains(out, "crew-abcd") {
		t.Errorf("unscoped events lack the 8-rune crew label:\n%s", out)
	}
	crew := "crew-abcdefghij"
	ov.Scope.Crew = &crew
	if out := ansi.Strip(renderOverview(200, 60, ov, st, pillRound)); strings.Contains(out, "crew-abc") {
		t.Errorf("scoped events carry a crew column:\n%s", out)
	}
}
