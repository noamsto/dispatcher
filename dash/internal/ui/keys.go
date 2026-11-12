package ui

import "github.com/charmbracelet/bubbles/key"

// KeyMap is the root model's global keymap — the keys every view shares
// (tab switching, refresh, help, quit). A view's own keys are appended
// alongside these in the help bar (see helpKeyMap in model.go).
type KeyMap struct {
	Tab1    key.Binding
	Tab2    key.Binding
	Tab3    key.Binding
	Tab4    key.Binding
	Tab5    key.Binding
	Tab6    key.Binding
	Next    key.Binding
	Prev    key.Binding
	Refresh key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func DefaultKeyMap() KeyMap {
	return KeyMap{
		Tab1:    key.NewBinding(key.WithKeys("1"), key.WithHelp("1-6", "views")),
		Tab2:    key.NewBinding(key.WithKeys("2")),
		Tab3:    key.NewBinding(key.WithKeys("3")),
		Tab4:    key.NewBinding(key.WithKeys("4")),
		Tab5:    key.NewBinding(key.WithKeys("5")),
		Tab6:    key.NewBinding(key.WithKeys("6")),
		Next:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next view")),
		Prev:    key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "prev view")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp is the global row shown collapsed.
func (k KeyMap) ShortHelp() []key.Binding { return k.shortHelp(false) }

// FullHelp is the global row shown expanded (`?`).
func (k KeyMap) FullHelp() [][]key.Binding { return k.fullHelp(false) }

// shortHelp and fullHelp drop the view-switching tab keys while the active view
// owns them, since they move its selection instead.
func (k KeyMap) shortHelp(ownsTab bool) []key.Binding {
	if ownsTab {
		return []key.Binding{k.Tab1, k.Refresh, k.Help, k.Quit}
	}
	return []key.Binding{k.Tab1, k.Next, k.Refresh, k.Help, k.Quit}
}

func (k KeyMap) fullHelp(ownsTab bool) [][]key.Binding {
	if ownsTab {
		return [][]key.Binding{{k.Tab1, k.Refresh, k.Help, k.Quit}}
	}
	return [][]key.Binding{{k.Tab1, k.Next, k.Prev, k.Refresh, k.Help, k.Quit}}
}

// tabIndex maps a KeyMsg's string to a 0-based view index, or -1.
func tabIndex(s string) int {
	if len(s) == 1 && s[0] >= '1' && s[0] <= '0'+byte(len(tabTitles)) {
		return int(s[0] - '1')
	}
	return -1
}
