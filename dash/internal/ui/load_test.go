package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// startCmds runs cmd and every Cmd of the BatchMsgs it yields in its own
// goroutine, as the Bubble Tea runtime does, sending each leaf message to
// out.
func startCmds(cmd tea.Cmd, out chan<- tea.Msg) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				startCmds(c, out)
			}
			return
		}
		out <- msg
	}()
}

func collectLoaders(r data.Runner, cfg data.Config) Loaders {
	return Loaders{
		Settings: func(ctx context.Context) data.SettingsSection { return data.CollectSettings(ctx, r, cfg) },
		Budget:   func(ctx context.Context) data.BudgetSection { return data.CollectBudget(ctx, r, cfg) },
		Retro:    func(ctx context.Context) data.RunsSection { return data.CollectRetro(ctx, r, cfg) },
		Ratings:  func(ctx context.Context) data.RunsSection { return data.CollectRatings(ctx, r, cfg) },
		Roster:   func(ctx context.Context) data.RosterSection { return data.CollectRoster(ctx, r, cfg) },
	}
}

func switchTab(t *testing.T, m Model, tab string) Model {
	t.Helper()
	nm, _ := m.Update(keyRune(rune(tab[0])))
	return nm.(Model)
}

func TestFirstViewBeforeAnySourceReturns(t *testing.T) {
	never := make(chan struct{})
	gated := func() data.FakeResponse { return data.FakeResponse{Wait: never} }
	r := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"dispatch-config --show-origin":  gated(),
		"dispatch-config --layers":       gated(),
		"refresh-budget --report --json": gated(),
		"crew retro --report --json":     gated(),
		"crew rate --report --json":      gated(),
		"crew crews":                     gated(),
	}}
	cfg := data.Config{Crew: []string{"crew"}, DispatchConfigBin: "dispatch-config", RefreshBudget: "refresh-budget"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := sized(loadingModel(Deps{Ctx: ctx, Load: collectLoaders(r, cfg), Now: 1}), 80, 24)
	results := make(chan tea.Msg, 16)
	startCmds(m.Init(), results)

	deadline := time.Now().Add(2 * time.Second)
	for len(r.CallsSnapshot()) < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("collectors did not all reach the runner: %v", r.CallsSnapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}

	for tab, want := range map[string][]string{
		"1": {"loading settings…"},
		"2": {"loading budget…"},
		"3": {"loading ratings…", "loading retro…"},
		"4": {"loading roster…"},
	} {
		frame := switchTab(t, m, tab).View()
		for _, w := range want {
			if !strings.Contains(frame, w) {
				t.Errorf("tab %s frame lacks %q:\n%s", tab, w, frame)
			}
		}
	}
	if !strings.Contains(m.View(), "loading settings, budget, retro, ratings, roster…") {
		t.Errorf("footer does not list the in-flight sources:\n%s", m.View())
	}
	select {
	case msg := <-results:
		t.Fatalf("a source returned before the gate opened: %T", msg)
	default:
	}
}

func TestAllLoadersStartConcurrently(t *testing.T) {
	started := make(chan source, numSources)
	release := make(chan struct{})
	block := func(s source) {
		started <- s
		<-release
	}
	loaders := Loaders{
		Settings: func(context.Context) data.SettingsSection { block(srcSettings); return data.SettingsSection{} },
		Budget:   func(context.Context) data.BudgetSection { block(srcBudget); return data.BudgetSection{} },
		Retro:    func(context.Context) data.RunsSection { block(srcRetro); return data.RunsSection{} },
		Ratings:  func(context.Context) data.RunsSection { block(srcRatings); return data.RunsSection{} },
		Roster:   func(context.Context) data.RosterSection { block(srcRoster); return data.RosterSection{} },
	}
	results := make(chan tea.Msg, 16)
	startCmds(loadingModel(Deps{Load: loaders}).Init(), results)

	seen := map[source]bool{}
	timeout := time.After(time.Second)
	for len(seen) < int(numSources) {
		select {
		case s := <-started:
			seen[s] = true
		case <-timeout:
			t.Fatalf("only %d of %d loaders started before any was released: %v", len(seen), numSources, seen)
		}
	}
	close(release)
	for range numSources {
		select {
		case <-results:
		case <-time.After(time.Second):
			t.Fatalf("loaders did not finish after release")
		}
	}
}

func TestRequestWhileInFlightSetsAgain(t *testing.T) {
	loads := map[source]int{}
	count := func(s source) { loads[s]++ }
	loaders := Loaders{
		Settings: func(context.Context) data.SettingsSection { count(srcSettings); return data.SettingsSection{} },
		Budget:   func(context.Context) data.BudgetSection { count(srcBudget); return data.BudgetSection{} },
		Retro:    func(context.Context) data.RunsSection { count(srcRetro); return data.RunsSection{} },
		Ratings:  func(context.Context) data.RunsSection { count(srcRatings); return data.RunsSection{} },
		Roster:   func(context.Context) data.RosterSection { count(srcRoster); return data.RosterSection{} },
	}
	snap := loadSnapshot(t)
	m := loadedModel(snap, Deps{Now: snap.Now, Load: loaders})

	nm, first := m.Update(keyRune('r'))
	m = nm.(Model)
	msgs := flattenCmd(first)
	if len(msgs) != int(numSources) {
		t.Fatalf("r started %d loads, want %d", len(msgs), numSources)
	}
	nm, second := m.Update(keyRune('r'))
	m = nm.(Model)
	if second != nil {
		t.Fatalf("second r while in flight returned a Cmd, want nil")
	}
	if !m.load[srcRoster].again {
		t.Fatalf("second r did not set again")
	}

	var rosterMsg sourceMsg
	for _, msg := range msgs {
		if sm := msg.(sourceMsg); sm.src == srcRoster {
			rosterMsg = sm
		}
	}
	nm, rerun := m.Update(rosterMsg)
	m = nm.(Model)
	again := flattenCmd(rerun)
	if len(again) != 1 || again[0].(sourceMsg).src != srcRoster {
		t.Fatalf("landing roster re-issued %v, want exactly one roster load", again)
	}
	if loads[srcRoster] != 2 {
		t.Fatalf("roster loaded %d times, want 2 (initial r + one again)", loads[srcRoster])
	}
	if m.load[srcRoster].again {
		t.Fatalf("again not cleared after re-issue")
	}
	if !m.load[srcRoster].inFlight {
		t.Fatalf("re-issued roster load not marked in flight")
	}
}

func TestBusDebounceRequestWhileInFlightSetsAgain(t *testing.T) {
	snap := loadSnapshot(t)
	m := loadedModel(snap, Deps{Now: snap.Now})

	nm, cmd := m.Update(requestRosterMsg{})
	m = nm.(Model)
	if cmd == nil || !m.load[srcRoster].inFlight {
		t.Fatalf("requestRosterMsg did not start a roster load")
	}
	nm, cmd = m.Update(requestRosterMsg{})
	m = nm.(Model)
	if cmd != nil || !m.load[srcRoster].again {
		t.Fatalf("requestRosterMsg in flight: cmd = %v, again = %v; want nil, true", cmd != nil, m.load[srcRoster].again)
	}
}

func TestRefreshIsNonBlocking(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	wait := func() { <-release }
	loaders := Loaders{
		Settings: func(context.Context) data.SettingsSection { wait(); return data.SettingsSection{} },
		Budget:   func(context.Context) data.BudgetSection { wait(); return data.BudgetSection{} },
		Retro:    func(context.Context) data.RunsSection { wait(); return data.RunsSection{} },
		Ratings:  func(context.Context) data.RunsSection { wait(); return data.RunsSection{} },
		Roster:   func(context.Context) data.RosterSection { wait(); return data.RosterSection{} },
	}
	snap := loadSnapshot(t)
	m := sized(loadedModel(snap, Deps{Now: snap.Now, Load: loaders}), 80, 24)

	done := make(chan tea.Cmd, 1)
	go func() {
		_, cmd := m.Update(keyRune('r'))
		done <- cmd
	}()
	select {
	case cmd := <-done:
		if cmd == nil {
			t.Fatalf("r returned no Cmd")
		}
	case <-time.After(time.Second):
		t.Fatalf("r blocked on the loaders")
	}
}

func TestRunsPlaceholdersAreIndependent(t *testing.T) {
	snap := loadFullSnapshot(t)
	m := sized(loadingModel(Deps{Now: snap.Now}), 80, 24)
	nm, _ := m.Update(sourceMsg{srcRatings, func(s *data.Snapshot) {
		s.Runs.Ratings, s.Runs.RatingsGroups, s.Runs.RatingsError = snap.Runs.Ratings, snap.Runs.RatingsGroups, snap.Runs.RatingsError
	}})
	frame := switchTab(t, nm.(Model), "3").View()
	if !strings.Contains(frame, "loading retro…") {
		t.Errorf("retro pane lacks its placeholder:\n%s", frame)
	}
	if strings.Contains(frame, "loading ratings…") {
		t.Errorf("ratings pane still shows a placeholder after landing:\n%s", frame)
	}
}

func TestSettingsTreeKeepsCollapseWhenOtherSourceLands(t *testing.T) {
	snap := loadSnapshot(t)
	m := switchTab(t, sized(testModel(snap), 120, 40), "1")
	flat := m.views[0].(settingsView).flat
	branch := -1
	for i, n := range flat {
		if !n.isLeaf {
			branch = i
			break
		}
	}
	if branch < 0 {
		t.Fatalf("fixture has no settings branch to collapse")
	}
	var nm tea.Model = m
	for range branch {
		nm, _ = nm.Update(keyRune('j'))
	}
	nm, _ = nm.Update(keyRune(' '))
	collapsed := len(nm.(Model).views[0].(settingsView).flat)
	if collapsed >= len(flat) {
		t.Fatalf("space did not collapse a branch")
	}
	nm, _ = nm.Update(sourceMsg{srcRoster, func(s *data.Snapshot) { s.Roster = snap.Roster }})
	if got := len(nm.(Model).views[0].(settingsView).flat); got != collapsed {
		t.Fatalf("an unrelated source landing changed the settings tree: %d visible nodes, want %d", got, collapsed)
	}
}
