package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// sweepModel is a root Model on the Overview over the shared fixture plus 12
// blocked workers, so the strip always has more items than it can show.
func sweepModel() Model { return overviewModel(sweepInputs()) }

func sweepInputs() vm.Inputs {
	in := viewtest.Inputs()
	crew := &in.Snap.Roster.Crews[0]
	for i := range 12 {
		crew.Workers = append(crew.Workers, map[string]any{
			"branch": fmt.Sprintf("feat/x%02d", i),
			"from":   fmt.Sprintf("worker:feat/x%02d", i),
			"state":  "blocked",
			"detail": fmt.Sprintf("question %d for the operator, long enough to need truncating in a narrow strip", i),
			"ts":     int64(1794800000000 + i*1000),
			"engine": "claude", "model": "sonnet", "tier": "standard",
			"title": fmt.Sprintf("Task feat/x%02d with a deliberately long title that has to be cut", i),
		})
	}
	return in
}

// overviewModel is a loaded root Model on the Overview over in.
func overviewModel(in vm.Inputs) Model {
	m := NewModel(Deps{Now: in.Snap.Now, Crew: in.Crew}, func() time.Time { return time.UnixMilli(viewtest.NowMS) }, liveDeps{})
	m.snap, m.digest = in.Snap, in.Bus
	for i := range m.load {
		m.load[i] = loadState{loaded: true}
	}
	m.broadcast()
	m.active = overviewTab
	return m
}

func sweepSelect(m Model, branch string) Model {
	ov := m.views[overviewTab].(overviewView)
	for i, w := range ov.model.Workers {
		if w.Branch == branch {
			ov.st.focus = panelChecklists
			ov.setWorker(i)
		}
	}
	m.views[overviewTab] = ov
	return m
}

func TestOverviewSizeSweep(t *testing.T) {
	asciiProfile(t)
	typed := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("x", 300))}
	states := []struct {
		name  string
		title string // the overlay's top-border title; empty for none
		open  func(Model) Model
	}{
		{"normal", "", func(m Model) Model { return m }},
		{"detail", "DETAIL", func(m Model) Model { return sweepKeys(sweepSelect(m, "feat/a"), keyType(tea.KeyEnter)) }},
		{"help", "HELP", func(m Model) Model { return sweepKeys(m, keyRune('?')) }},
		{"reply", "REPLY", func(m Model) Model { return sweepKeys(sweepSelect(m, "feat/c"), keyRune('r'), typed) }},
		{"merge", "CONFIRM", func(m Model) Model { return sweepKeys(sweepSelect(m, "feat/b"), keyRune('m')) }},
		{"filter", "FILTER", func(m Model) Model { return sweepKeys(m, keyRune('/'), typed) }},
	}
	for w := 100; w <= 200; w += 10 {
		for h := 30; h <= 60; h += 5 {
			base := sized(sweepModel(), w, h)
			for _, st := range states {
				frame := st.open(base).View()
				name := fmt.Sprintf("%dx%d %s", w, h, st.name)
				if len(strings.Split(frame, "\n")) != h {
					t.Errorf("%s: frame has %d lines, want %d", name, len(strings.Split(frame, "\n")), h)
					continue
				}
				assertWidth(t, frame, w, h)
				if st.title != "" && !hasOverlayTop(frame, st.title) {
					t.Errorf("%s: no overlay top border titled %s", name, st.title)
				}
			}
		}
	}
}

func sweepKeys(m Model, keys ...tea.KeyMsg) Model {
	for _, k := range keys {
		nm, _ := m.Update(k)
		m = nm.(Model)
	}
	return m
}

// hasOverlayTop reports whether some line carries a rounded top corner and the
// overlay's title pill.
func hasOverlayTop(frame, title string) bool {
	for _, l := range strings.Split(stripped(frame), "\n") {
		if strings.Contains(l, "╭") && strings.Contains(l, title) {
			return true
		}
	}
	return false
}

// hostile is worker-controlled text carrying a newline, a clear-screen CSI and
// an OSC 52 clipboard write.
const hostile = "exit 1:\nFAIL \x1b[2J\x1b]52;c;cHduZWQ=\x07 done"

// hostileInputs puts hostile text in a blocked worker's detail, a title, a
// remote host, a role name and detail, and a bus event's sender and text.
func hostileInputs() vm.Inputs {
	in := viewtest.Inputs()
	for _, w := range in.Snap.Roster.Crews[0].Workers {
		switch w["branch"] {
		case "feat/a":
			w["title"] = hostile
		case "feat/c":
			w["detail"] = hostile
		case "feat/r":
			w["host"] = hostile
		}
	}
	in.Bus.Roles[data.RoleKey{Crew: "c1", Branch: "feat/a", Role: hostile}] = &data.RoleDigest{
		State: "failed", Detail: hostile, TS: viewtest.NowMS - 1000,
	}
	in.Bus.Recent = append(in.Bus.Recent,
		data.RecentEvent{TS: viewtest.NowMS - 2000, From: "worker:feat/a#s1", Kind: "msg", Detail: hostile},
		data.RecentEvent{TS: viewtest.NowMS - 1000, From: "role:feat/a:" + hostile, Kind: "msg", Detail: hostile})
	return in
}

func TestOverviewSanitizesWorkerText(t *testing.T) {
	asciiProfile(t)
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {150, 40}, {200, 60}} {
		w, h := sz[0], sz[1]
		v := newOverviewView(hostileInputs(), overviewNow)
		v.View(w, h)
		detail := v
		selectBranch(t, &detail, "feat/a")
		detail, _ = ovKeys(detail, "enter")
		if detail.mode != ovDetail {
			t.Fatalf("enter did not open the detail overlay")
		}
		if w >= 150 && !strings.Contains(detail.View(w, h), "exit 1: FAIL") {
			t.Errorf("%dx%d: the cleaned hostile text is not in the detail overlay", w, h)
		}
		for name, frame := range map[string]string{"normal": v.View(w, h), "detail": detail.View(w, h)} {
			if n := len(strings.Split(frame, "\n")); n != h {
				t.Errorf("%dx%d %s: %d lines, want %d", w, h, name, n, h)
			}
			assertWidth(t, frame, w, h)
			for _, esc := range []string{"\x1b]", "\x1b[2J", "\x07"} {
				if strings.Contains(frame, esc) {
					t.Errorf("%dx%d %s: frame contains %q", w, h, name, esc)
				}
			}
		}
	}
	m := overviewModel(hostileInputs())
	for _, k := range []tea.KeyMsg{keyType(tea.KeyTab), keyType(tea.KeyEnter)} {
		frame := sweepKeys(sized(m, 150, 40), k).View()
		if strings.Contains(frame, "\x1b]") || strings.Contains(frame, "\x1b[2J") {
			t.Errorf("root frame after %s carries an escape sequence", k)
		}
	}
}

// TestOverviewPoisonedViewRendersClean renders a view model whose every string
// carries escapes, a C1 and a bidi control, cleaned the way rebuild cleans it,
// in each mode at each size: the frame keeps its geometry, carries no control,
// and matches the frame of the unpoisoned fixture.
func TestOverviewPoisonedViewRendersClean(t *testing.T) {
	asciiProfile(t)
	modes := []struct {
		name   string
		mode   ovMode
		branch string
		keys   []string
	}{
		{"normal", ovNormal, "", nil},
		{"detail", ovDetail, "feat/a", []string{"enter"}},
		{"help", ovHelp, "", []string{"?"}},
		{"confirm", ovConfirm, "feat/b", []string{"m"}},
		{"reply", ovReply, "feat/c", []string{"r", "hi"}},
	}
	poisoned := cleanView(poisonedOverview())
	// Commands are raw and never cleaned: a poisoned one shows a notice, which
	// TestControlCommandNeverShownCleaned covers, so keep the fixture's here.
	for i, a := range vm.BuildOverview(viewtest.Inputs()).Attention {
		poisoned.Attention[i].Action = a.Action
	}
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {150, 40}, {200, 60}} {
		w, h := sz[0], sz[1]
		for _, md := range modes {
			name := fmt.Sprintf("%dx%d %s", w, h, md.name)
			render := func(model vm.OverviewView) string {
				v := newOverviewView(viewtest.Inputs(), overviewNow)
				v.model = model
				v.View(w, h)
				if md.branch != "" {
					selectBranch(t, &v, md.branch)
				}
				v, _ = ovKeys(v, md.keys...)
				if v.mode != md.mode {
					t.Fatalf("%s: mode %v, want %v", name, v.mode, md.mode)
				}
				return v.View(w, h)
			}
			frame := render(poisoned)
			lines := strings.Split(frame, "\n")
			if len(lines) != h {
				t.Errorf("%s: %d lines, want %d", name, len(lines), h)
			}
			assertWidth(t, frame, w, h)
			for i, l := range lines {
				if j := strings.IndexFunc(ansi.Strip(l), controlRune); j >= 0 {
					t.Errorf("%s: line %d carries control %U: %q", name, i, []rune(ansi.Strip(l)[j:])[0], l)
				}
			}
			if want := render(cleanView(vm.BuildOverview(viewtest.Inputs()))); frame != want {
				t.Errorf("%s: the poisoned frame differs from the fixture's:\n%s\n--- want ---\n%s", name, stripped(frame), stripped(want))
			}
		}
	}
}
