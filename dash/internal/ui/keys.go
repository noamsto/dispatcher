package ui

import "github.com/charmbracelet/bubbles/key"

// KeyMap is the root model's global keymap — the keys every view shares
// (tab switching, refresh, help, quit). A view's own keys are appended
// alongside these in the help bar (see helpKeyMap in model.go).
type KeyMap struct {
	Tab1     key.Binding
	Tab2     key.Binding
	Tab3     key.Binding
	Tab4     key.Binding
	Tab5     key.Binding
	Tab6     key.Binding
	Tab7     key.Binding
	Tab8     key.Binding
	Next     key.Binding
	Prev     key.Binding
	PrevView key.Binding
	NextView key.Binding
	Refresh  key.Binding
	Help     key.Binding
	Quit     key.Binding
}

func DefaultKeyMap() KeyMap {
	return KeyMap{
		Tab1:     key.NewBinding(key.WithKeys("1"), key.WithHelp("1-8", "views")),
		Tab2:     key.NewBinding(key.WithKeys("2")),
		Tab3:     key.NewBinding(key.WithKeys("3")),
		Tab4:     key.NewBinding(key.WithKeys("4")),
		Tab5:     key.NewBinding(key.WithKeys("5")),
		Tab6:     key.NewBinding(key.WithKeys("6")),
		Tab7:     key.NewBinding(key.WithKeys("7")),
		Tab8:     key.NewBinding(key.WithKeys("8")),
		Next:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next view")),
		Prev:     key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "prev view")),
		PrevView: key.NewBinding(key.WithKeys("[")),
		NextView: key.NewBinding(key.WithKeys("]"), key.WithHelp("[/]", "prev/next")),
		Refresh:  key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp is the global row shown collapsed.
func (k KeyMap) ShortHelp() []key.Binding { return k.shortHelp(false, false) }

// FullHelp is the global row shown expanded (`?`).
func (k KeyMap) FullHelp() [][]key.Binding { return k.fullHelp(false, false) }

// shortHelp and fullHelp drop the view-switching tab and digit keys while the
// active view owns them, since they act on its selection instead.
func (k KeyMap) shortHelp(ownsTab, ownsDigits bool) []key.Binding {
	return k.switching(ownsTab, ownsDigits, k.Next)
}

func (k KeyMap) fullHelp(ownsTab, ownsDigits bool) [][]key.Binding {
	return [][]key.Binding{k.switching(ownsTab, ownsDigits, k.Next, k.Prev)}
}

func (k KeyMap) switching(ownsTab, ownsDigits bool, tabKeys ...key.Binding) []key.Binding {
	var out []key.Binding
	if !ownsDigits {
		out = append(out, k.Tab1)
	}
	if !ownsTab {
		out = append(out, tabKeys...)
	}
	return append(out, k.NextView, k.Refresh, k.Help, k.Quit)
}

// tabIndex maps a KeyMsg's string to a 0-based view index, or -1.
func tabIndex(s string) int {
	if len(s) == 1 && s[0] >= '1' && s[0] <= '0'+byte(len(tabTitles)) {
		return int(s[0] - '1')
	}
	return -1
}
