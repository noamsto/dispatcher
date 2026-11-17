package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
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
	srcBus
	srcFleet
	numSources
)

var sourceNames = [numSources]string{"settings", "budget", "retro", "ratings", "roster", "bus", "fleet"}

func (s source) String() string { return sourceNames[s] }

// Loaders collects each source; main.go binds them to the data.Collect*
// functions. A nil loader loads an empty section.
type Loaders struct {
	Settings func(context.Context) data.SettingsSection
	Budget   func(context.Context) data.BudgetSection
	Retro    func(context.Context) data.RunsSection
	Ratings  func(context.Context) data.RunsSection
	Roster   func(context.Context) data.RosterSection
	Bus      func(context.Context) data.BusDigest
	// Fleet is not loaded at Init: the Fleet tab requests it on first activation.
	// Fleet streams accumulated partial inputs through emit and returns the
	// final; emit must not block.
	Fleet func(ctx context.Context, emit func(vm.FleetInputs)) vm.FleetInputs
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
	bus   *data.BusDigest // set only by srcBus
	fleet *vm.FleetInputs // set only by srcFleet
}

// fleetPartialMsg carries a non-final fleet partial; next waits for the
// stream's following message.
type fleetPartialMsg struct {
	in   vm.FleetInputs
	next tea.Cmd
}

// fleetCmd runs the fleet stream in the background and returns the wait
// chain over it. Partials pass through a one-slot channel that a newer
// partial replaces, so the collector never blocks on a slow UI. The channel
// closes before the final is published, so no partial can follow it.
func (l Loaders) fleetCmd(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		if l.Fleet == nil {
			return sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &vm.FleetInputs{}}
		}
		parts := make(chan vm.FleetInputs, 1)
		final := make(chan vm.FleetInputs, 1)
		go func() {
			f := l.Fleet(ctx, func(in vm.FleetInputs) {
				for {
					select {
					case <-ctx.Done():
						return
					case parts <- in:
						return
					default:
					}
					select {
					case <-parts:
					default:
					}
				}
			})
			close(parts)
			final <- f
		}()
		return waitFleetStream(parts, final)()
	}
}

func waitFleetStream(parts <-chan vm.FleetInputs, final <-chan vm.FleetInputs) tea.Cmd {
	return func() tea.Msg {
		in, ok := <-parts
		if ok {
			return fleetPartialMsg{in: in, next: waitFleetStream(parts, final)}
		}
		f := <-final
		return sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &f}
	}
}

func (l Loaders) cmd(ctx context.Context, s source) tea.Cmd {
	if s == srcFleet {
		return l.fleetCmd(ctx)
	}
	return func() tea.Msg {
		msg := sourceMsg{src: s, apply: l.collect(ctx, s)}
		if s == srcBus && l.Bus != nil {
			d := l.Bus(ctx)
			msg.bus = &d
		}
		return msg
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
