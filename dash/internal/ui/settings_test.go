package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func newTestSettingsView(t *testing.T) settingsView {
	t.Helper()
	return newSettingsView(loadSnapshot(t))
}

func TestSettingsTreeAllExpandedInitially(t *testing.T) {
	v := newTestSettingsView(t)
	snap := loadSnapshot(t)
	if len(v.flat) != len(snap.Settings.Rows)+countBranches(v.root) {
		t.Fatalf("flat = %d nodes, want every branch + every leaf visible", len(v.flat))
	}
	for _, n := range v.flat {
		if !n.isLeaf && n.collapsed {
			t.Fatalf("branch %v collapsed at start", n.path)
		}
	}
}

func countBranches(n *node) int {
	c := 0
	for _, ch := range n.children {
		if !ch.isLeaf {
			c++
			c += countBranches(ch)
		}
	}
	return c
}

func findFlatIndex(v settingsView, isLeaf bool, name string) int {
	for i, n := range v.flat {
		if n.isLeaf == isLeaf && n.name == name {
			return i
		}
	}
	return -1
}

func TestSettingsCollapseExpand(t *testing.T) {
	v := newTestSettingsView(t)
	idx := findFlatIndex(v, false, "openrouter")
	if idx < 0 {
		t.Fatalf("openrouter branch not found")
	}
	before := len(v.flat)
	v.cursor = idx

	nv, _ := v.Update(keyRune(' '))
	v = nv.(settingsView)
	if !v.root.children[findChildIdx(v.root, "openrouter")].collapsed {
		t.Fatalf("space did not collapse openrouter")
	}
	if len(v.flat) >= before {
		t.Fatalf("flat did not shrink after collapse: %d >= %d", len(v.flat), before)
	}
	if v.cursor != idx {
		t.Fatalf("cursor moved off the collapsed branch: %d, want %d", v.cursor, idx)
	}

	nv, _ = v.Update(keyType(tea.KeyRight))
	v = nv.(settingsView)
	if len(v.flat) != before {
		t.Fatalf("right did not re-expand: flat = %d, want %d", len(v.flat), before)
	}

	nv, _ = v.Update(keyType(tea.KeyLeft))
	v = nv.(settingsView)
	if len(v.flat) >= before {
		t.Fatalf("left did not collapse again")
	}

	// enter toggles collapse too (currently collapsed by the left above).
	nv, _ = v.Update(keyType(tea.KeyEnter))
	v = nv.(settingsView)
	if len(v.flat) != before {
		t.Fatalf("enter did not re-expand: flat = %d, want %d", len(v.flat), before)
	}
}

func findChildIdx(root *node, name string) int {
	for i, c := range root.children {
		if c.name == name {
			return i
		}
	}
	return -1
}

func TestSettingsCursorClampsOnCollapse(t *testing.T) {
	v := newTestSettingsView(t)
	openrouterIdx := findFlatIndex(v, false, "openrouter")
	keyFileIdx := findFlatIndex(v, true, "keyFile")
	if openrouterIdx < 0 || keyFileIdx < 0 || keyFileIdx <= openrouterIdx {
		t.Fatalf("fixture shape unexpected: openrouter=%d keyFile=%d", openrouterIdx, keyFileIdx)
	}
	v.cursor = keyFileIdx

	nv, _ := v.Update(keyType(tea.KeyLeft))
	v = nv.(settingsView)
	if v.cursor != openrouterIdx {
		t.Fatalf("cursor did not clamp to the collapsed parent: got %d, want %d", v.cursor, openrouterIdx)
	}
	if n := v.flat[v.cursor]; n.isLeaf || n.name != "openrouter" {
		t.Fatalf("cursor not on openrouter after clamp: %+v", n)
	}
}

func TestSettingsCursorMovement(t *testing.T) {
	v := newTestSettingsView(t)
	total := len(v.flat)

	nv, _ := v.Update(keyRune('G'))
	v = nv.(settingsView)
	if v.cursor != total-1 {
		t.Fatalf("G: cursor = %d, want %d", v.cursor, total-1)
	}
	nv, _ = v.Update(keyRune('g'))
	v = nv.(settingsView)
	if v.cursor != 0 {
		t.Fatalf("g: cursor = %d, want 0", v.cursor)
	}
	nv, _ = v.Update(keyRune('j'))
	v = nv.(settingsView)
	if v.cursor != 1 {
		t.Fatalf("j: cursor = %d, want 1", v.cursor)
	}
	nv, _ = v.Update(keyRune('k'))
	v = nv.(settingsView)
	if v.cursor != 0 {
		t.Fatalf("k: cursor = %d, want 0", v.cursor)
	}
	// k at top clamps at 0
	nv, _ = v.Update(keyRune('k'))
	v = nv.(settingsView)
	if v.cursor != 0 {
		t.Fatalf("k at top: cursor = %d, want 0", v.cursor)
	}
}

func TestSettingsFilter(t *testing.T) {
	v := newTestSettingsView(t)
	nv, _ := v.Update(keyRune('/'))
	v = nv.(settingsView)
	if !v.Capturing() {
		t.Fatalf("/ did not enter capturing mode")
	}
	for _, r := range "keyfile" {
		nv, _ = v.Update(keyRune(r))
		v = nv.(settingsView)
	}
	if v.query != "keyfile" {
		t.Fatalf("query = %q, want keyfile", v.query)
	}

	var leafNames []string
	for _, n := range v.flat {
		if n.isLeaf {
			leafNames = append(leafNames, strings.Join(n.path, "."))
		}
	}
	if len(leafNames) != 1 || leafNames[0] != "openrouter.keyFile" {
		t.Fatalf("filtered leaves = %v, want [openrouter.keyFile]", leafNames)
	}
	if findFlatIndex(v, false, "openrouter") < 0 {
		t.Fatalf("ancestor branch openrouter not shown while filtering")
	}
}

func TestSettingsFilterEscClears(t *testing.T) {
	v := newTestSettingsView(t)
	total := len(v.flat)
	nv, _ := v.Update(keyRune('/'))
	v = nv.(settingsView)
	for _, r := range "keyfile" {
		nv, _ = v.Update(keyRune(r))
		v = nv.(settingsView)
	}
	nv, _ = v.Update(keyType(tea.KeyEsc))
	v = nv.(settingsView)
	if v.Capturing() {
		t.Fatalf("esc did not clear capturing")
	}
	if v.query != "" {
		t.Fatalf("esc did not clear query: %q", v.query)
	}
	if len(v.flat) != total {
		t.Fatalf("esc did not restore full tree: %d, want %d", len(v.flat), total)
	}
}

func TestSettingsBadgesDistinctUnderTrueColor(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	seen := map[string]string{}
	for _, origin := range []string{"base", "user", "env", "locked"} {
		rendered := badgeStyle(origin).Render(badgeText(origin))
		for otherOrigin, otherRendered := range seen {
			if rendered == otherRendered {
				t.Fatalf("origin %q and %q render identically: %q", origin, otherOrigin, rendered)
			}
		}
		seen[origin] = rendered
	}
	if !strings.Contains(seen["locked"], "🔒") {
		t.Fatalf("locked badge missing lock glyph: %q", seen["locked"])
	}
}

func TestSettingsSelectionIsCursorRow(t *testing.T) {
	v := newTestSettingsView(t)
	idx := findFlatIndex(v, true, "keyFile")
	if idx < 0 {
		t.Fatalf("keyFile leaf not found")
	}
	v.cursor = idx
	sel, ok := v.Selection().(data.SettingRow)
	if !ok {
		t.Fatalf("Selection() = %T, want data.SettingRow", v.Selection())
	}
	if strings.Join(sel.Path, ".") != "openrouter.keyFile" || sel.Origin != "locked" || sel.Editable {
		t.Fatalf("Selection() = %+v, want openrouter.keyFile/locked/not editable", sel)
	}
}
