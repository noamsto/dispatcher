// Package ui is the interactive Bubble Tea program. Run starts it; the rest
// of the package (model.go, keys.go, styles.go, layout.go, and one file per
// view) is unexported implementation.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/buswatch"
	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Deps bundles everything Run needs. Nothing is collected up front: Load's
// per-source collectors run after the first frame is drawn. EventsPath is
// the bus log path RecentEvents/the watcher read — resolved once by the
// caller (main.go), since resolving it needs a Runner/Config this package
// doesn't own. EventsPathErr explains an empty EventsPath in the Live
// view's status note.
type Deps struct {
	Ctx            context.Context // passed to every loader; main cancels it on return
	Load           Loaders
	Runner         data.Runner
	Crew           []string // resolved crew argv prefix (data.Config.Crew)
	InTmux         bool
	EventsPath     string
	EventsPathErr  error
	Repo           string // repo label for the crew header, picked by the caller
	Now            int64  // collection clock (data.Config.Now) stamped into the snapshot
	Map            MapDeps
	DispatchConfig string // dispatch-config binary the Settings tab writes through
	Narrow         bool   // force the stacked narrow Overview layout at any width

	// FleetWatchPaths is the glob list the Fleet tab's change poll watches,
	// re-read every tick; nil disables it.
	FleetWatchPaths func() []string
}

// Run starts the interactive dashboard. It resolves the roster's live bus
// watcher itself, outside the Bubble Tea model, so it can be reliably
// closed exactly once after Program.Run returns regardless of which key
// quit the program — the model never owns watcher lifecycle.
func Run(deps Deps) error {
	if colorForcedAscii(os.Getenv) {
		lipgloss.SetColorProfile(termenv.Ascii)
	}

	var watcher buswatch.Watcher
	liveOff := ""
	if deps.EventsPath != "" {
		watcher = buswatch.New(deps.EventsPath)
	} else {
		reason := "bus log path unavailable"
		if deps.EventsPathErr != nil {
			reason = deps.EventsPathErr.Error()
		}
		liveOff = "live refresh off: " + reason
	}

	live := liveDeps{
		eventsPath:  deps.EventsPath,
		watcher:     watcher,
		liveOffNote: liveOff,
	}
	if deps.FleetWatchPaths != nil {
		live.fleetWatcher = buswatch.NewPoll(deps.FleetWatchPaths, time.Second)
	}
	applyPalette(resolveTheme(os.Getenv, tmuxFlavorFunc(deps.Ctx, deps.Runner, deps.InTmux), lipgloss.HasDarkBackground))
	m := NewModel(deps, time.Now, live)
	m.themePinned = themePinned(os.Getenv)
	p := tea.NewProgram(m, tea.WithAltScreen())
	notify := themeReportsWanted(os.Getenv)
	if notify {
		_, _ = fmt.Fprint(os.Stdout, themeReportsOn)
	}
	_, err := p.Run()
	if notify {
		_, _ = fmt.Fprint(os.Stdout, themeReportsOff)
	}
	deps.Map.deleteImages(deps.InTmux)
	if watcher != nil {
		_ = watcher.Close()
	}
	if live.fleetWatcher != nil {
		_ = live.fleetWatcher.Close()
	}
	return err
}

// DEC mode 2031 (theme-change reports) toggles, and the CSI ? 996 n query whose
// reply is the same CSI ? 997 ; N n report.
const (
	themeReportsOn  = "\x1b[?2031h"
	themeReportsOff = "\x1b[?2031l"
	themeQuery      = "\x1b[?996n"
)

// themeReportsWanted is whether the dash subscribes to theme-change reports.
func themeReportsWanted(getenv func(string) string) bool {
	return !colorForcedAscii(getenv) && !themePinned(getenv)
}

// themeAwareExec wraps an editor hand-off so theme reports stay off while the
// editor owns the tty, then are re-enabled and re-queried so a flip during
// the edit is picked up.
func themeAwareExec(inner func(*exec.Cmd, tea.ExecCallback) tea.Cmd, w io.Writer) func(*exec.Cmd, tea.ExecCallback) tea.Cmd {
	return func(c *exec.Cmd, cb tea.ExecCallback) tea.Cmd {
		run := inner(c, func(err error) tea.Msg {
			_, _ = fmt.Fprint(w, themeReportsOn+themeQuery)
			return cb(err)
		})
		return func() tea.Msg {
			_, _ = fmt.Fprint(w, themeReportsOff)
			return run()
		}
	}
}

// colorForcedAscii decides whether the interactive TUI must force the Ascii
// color profile. CREW_DASH_COLOR=always has no effect here (unlike --once's
// colorEnabled in main.go): the TUI otherwise auto-detects color support,
// and forcing it on would fight lipgloss's own terminal detection.
func colorForcedAscii(getenv func(string) string) bool {
	return getenv("NO_COLOR") != "" || getenv("CREW_DASH_COLOR") == "never"
}
