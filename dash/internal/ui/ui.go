// Package ui is the interactive Bubble Tea program. Run starts it; the rest
// of the package (model.go, keys.go, styles.go, layout.go, and one file per
// view) is unexported implementation.
package ui

import (
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Deps bundles everything Run needs beyond the initial Snapshot: the full
// re-collect (bound to "r"), the roster-only re-collect the live bus
// watcher triggers, and the bus log path RecentEvents/the watcher read —
// resolved once by the caller (main.go), since resolving it needs a
// Runner/Config this package doesn't own. EventsPathErr explains an empty
// EventsPath in the Roster view's status note.
type Deps struct {
	Snapshot      data.Snapshot
	Collect       func() data.Snapshot
	CollectRoster func() data.RosterSection
	EventsPath    string
	EventsPathErr error
}

// Run starts the interactive dashboard. It resolves the roster's live bus
// watcher itself, outside the Bubble Tea model, so it can be reliably
// closed exactly once after Program.Run returns regardless of which key
// quit the program — the model never owns watcher lifecycle.
func Run(deps Deps) error {
	if colorForcedAscii(os.Getenv) {
		lipgloss.SetColorProfile(termenv.Ascii)
	}

	var watcher busWatcher
	liveOff := ""
	if deps.EventsPath != "" {
		watcher = newBusWatcher(deps.EventsPath, nil)
	} else {
		reason := "bus log path unavailable"
		if deps.EventsPathErr != nil {
			reason = deps.EventsPathErr.Error()
		}
		liveOff = "live refresh off: " + reason
	}

	roster := rosterDeps{
		eventsPath:  deps.EventsPath,
		watcher:     watcher,
		liveOffNote: liveOff,
		collect:     deps.CollectRoster,
	}
	m := NewModel(deps.Snapshot, deps.Collect, time.Now, roster)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if watcher != nil {
		watcher.Close()
	}
	return err
}

// colorForcedAscii decides whether the interactive TUI must force the Ascii
// color profile. CREW_DASH_COLOR=always has no effect here (unlike --once's
// colorEnabled in main.go): the TUI otherwise auto-detects color support,
// and forcing it on would fight lipgloss's own terminal detection.
func colorForcedAscii(getenv func(string) string) bool {
	return getenv("NO_COLOR") != "" || getenv("CREW_DASH_COLOR") == "never"
}
