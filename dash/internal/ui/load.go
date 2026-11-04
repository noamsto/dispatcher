package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// source is one independently loaded slice of the Snapshot. The order is
// the footer's listing order.
type source int

const (
	srcSettings source = iota
	srcBudget
	srcRetro
	srcRatings
	srcRoster
	numSources
)

var sourceNames = [numSources]string{"settings", "budget", "retro", "ratings", "roster"}

func (s source) String() string { return sourceNames[s] }

// Loaders collects each source; main.go binds them to the data.Collect*
// functions. A nil loader loads an empty section.
type Loaders struct {
	Settings func(context.Context) data.SettingsSection
	Budget   func(context.Context) data.BudgetSection
	Retro    func(context.Context) data.RunsSection
	Ratings  func(context.Context) data.RunsSection
	Roster   func(context.Context) data.RosterSection
}

// loadState tracks one source. again records a request that arrived while a
// load was in flight, so the source reloads once when it lands instead of
// starting a second subprocess.
type loadState struct {
	inFlight, again, loaded bool
	at                      time.Time
}

// sourceMsg carries one source's result; apply folds it into the root
// model's Snapshot.
type sourceMsg struct {
	src   source
	apply func(*data.Snapshot)
}

func (l Loaders) cmd(ctx context.Context, s source) tea.Cmd {
	return func() tea.Msg {
		return sourceMsg{src: s, apply: l.collect(ctx, s)}
	}
}

func (l Loaders) collect(ctx context.Context, s source) func(*data.Snapshot) {
	switch s {
	case srcSettings:
		if l.Settings != nil {
			sec := l.Settings(ctx)
			return func(snap *data.Snapshot) { snap.Settings = sec }
		}
	case srcBudget:
		if l.Budget != nil {
			sec := l.Budget(ctx)
			return func(snap *data.Snapshot) { snap.Budget = sec }
		}
	case srcRetro:
		if l.Retro != nil {
			sec := l.Retro(ctx)
			return func(snap *data.Snapshot) {
				snap.Runs.Retro = sec.Retro
				snap.Runs.RetroError = sec.RetroError
			}
		}
	case srcRatings:
		if l.Ratings != nil {
			sec := l.Ratings(ctx)
			return func(snap *data.Snapshot) {
				snap.Runs.Ratings = sec.Ratings
				snap.Runs.RatingsGroups = sec.RatingsGroups
				snap.Runs.RatingsError = sec.RatingsError
			}
		}
	case srcRoster:
		if l.Roster != nil {
			sec := l.Roster(ctx)
			return func(snap *data.Snapshot) { snap.Roster = sec }
		}
	}
	return func(*data.Snapshot) {}
}

// loadingLine is the per-pane placeholder shown until a source has loaded.
func loadingLine(s source, w int) string {
	return truncateLine(loadingStyle.Render("loading "+s.String()+"…"), w)
}
