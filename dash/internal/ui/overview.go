package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// overviewView is the Overview tab. It holds the latest view model, rebuilt
// from the root's inputs on every snapshot and on the 1 s tick so ages stay
// current.
type overviewView struct {
	now   func() time.Time
	in    vm.Inputs
	model vm.OverviewView
}

func newOverviewView(in vm.Inputs, now func() time.Time) overviewView {
	v := overviewView{now: now, in: in}
	v.model = vm.BuildOverview(v.in)
	return v
}

func (v overviewView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.in = msg.in
		v.model = vm.BuildOverview(v.in)
	case tickMsg:
		v.in.NowMS = v.now().UnixMilli()
		v.model = vm.BuildOverview(v.in)
	}
	return v, nil
}

func (v overviewView) View(w, h int) string {
	line := "all clear"
	if n := len(v.model.Attention); n > 0 {
		line = fmt.Sprintf("%d need action", n)
	}
	lines := make([]string, max0(h))
	for i := range lines {
		lines[i] = ""
	}
	if len(lines) > 0 {
		lines[0] = truncateLine(line, w)
	}
	return strings.Join(lines, "\n")
}

func (overviewView) Keys() []key.Binding       { return nil }
func (overviewView) Selection() data.Selection { return nil }
func (overviewView) Capturing() bool           { return false }

// ownsKey keeps the global switching, refresh and help keys for the Overview's
// own use.
func (overviewView) ownsKey(msg tea.KeyMsg) bool {
	switch s := msg.String(); s {
	case "tab", "shift+tab", "r", "?":
		return true
	default:
		return len(s) == 1 && s[0] >= '1' && s[0] <= '9'
	}
}

func (v overviewView) enter(data.Selection) (view, tea.Cmd) {
	return v, tea.EnableMouseCellMotion
}

func (v overviewView) leave() (view, tea.Cmd) { return v, tea.DisableMouse }
