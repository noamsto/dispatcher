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
	Next    key.Binding
	Prev    key.Binding
	Refresh key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func DefaultKeyMap() KeyMap {
	return KeyMap{
		Tab1:    key.NewBinding(key.WithKeys("1"), key.WithHelp("1-4", "views")),
		Tab2:    key.NewBinding(key.WithKeys("2")),
		Tab3:    key.NewBinding(key.WithKeys("3")),
		Tab4:    key.NewBinding(key.WithKeys("4")),
		Next:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next view")),
		Prev:    key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "prev view")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp is the global row shown collapsed.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Tab1, k.Next, k.Refresh, k.Help, k.Quit}
}

// FullHelp is the global row shown expanded (`?`).
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Tab1, k.Next, k.Prev, k.Refresh, k.Help, k.Quit}}
}

// tabIndex maps a KeyMsg's string to a 0-based view index, or -1.
func tabIndex(s string) int {
	switch s {
	case "1":
		return 0
	case "2":
		return 1
	case "3":
		return 2
	case "4":
		return 3
	}
	return -1
}
