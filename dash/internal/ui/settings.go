package ui

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// node is one row of the settings tree: either a branch (a path prefix with
// children) or a leaf (one data.SettingRow).
type node struct {
	isLeaf    bool
	name      string
	path      []string
	depth     int
	row       data.SettingRow
	parent    *node
	children  []*node
	collapsed bool
}

// buildSettingsTree turns the flat --show-origin rows (document order) into
// a tree: a leaf's path prefix walks/creates branch nodes along the way, so
// siblings sharing a prefix share one branch (mirrors the once renderer's
// settingsBody indentation, but kept navigable/collapsible).
func buildSettingsTree(rows []data.SettingRow) *node {
	root := &node{}
	for _, r := range rows {
		cur := root
		for i := 0; i < len(r.Path)-1; i++ {
			name := r.Path[i]
			var child *node
			for _, c := range cur.children {
				if !c.isLeaf && c.name == name {
					child = c
					break
				}
			}
			if child == nil {
				child = &node{
					name:   name,
					path:   append([]string{}, r.Path[:i+1]...),
					depth:  i,
					parent: cur,
				}
				cur.children = append(cur.children, child)
			}
			cur = child
		}
		leaf := &node{
			isLeaf: true,
			name:   r.Path[len(r.Path)-1],
			path:   r.Path,
			depth:  len(r.Path) - 1,
			row:    r,
			parent: cur,
		}
		cur.children = append(cur.children, leaf)
	}
	return root
}

func flattenTree(n *node, out *[]*node) {
	for _, c := range n.children {
		*out = append(*out, c)
		if !c.isLeaf && !c.collapsed {
			flattenTree(c, out)
		}
	}
}

// filterKeep marks n and every leaf-or-ancestor that matches query
// (case-insensitive substring of the dot-path or the compact value).
func filterKeep(n *node, query string, keep map[*node]bool) bool {
	if n.isLeaf {
		if leafMatches(n, query) {
			keep[n] = true
			return true
		}
		return false
	}
	matched := false
	for _, c := range n.children {
		if filterKeep(c, query, keep) {
			matched = true
		}
	}
	if matched {
		keep[n] = true
	}
	return matched
}

func leafMatches(n *node, query string) bool {
	dotPath := strings.ToLower(strings.Join(n.path, "."))
	if strings.Contains(dotPath, query) {
		return true
	}
	return strings.Contains(strings.ToLower(compactJSON(n.row.Value)), query)
}

func flattenFiltered(n *node, keep map[*node]bool, out *[]*node) {
	for _, c := range n.children {
		if !keep[c] {
			continue
		}
		*out = append(*out, c)
		if !c.isLeaf {
			flattenFiltered(c, keep, out)
		}
	}
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

type settingsView struct {
	layers   *data.Layers
	warnings []string
	err      *string

	root   *node
	flat   []*node
	cursor int

	filter   textinput.Model
	filterOn bool
	query    string
}

func newSettingsView(snap data.Snapshot) settingsView {
	v := settingsView{
		layers:   snap.Settings.Layers,
		warnings: snap.Settings.Warnings,
		err:      snap.Settings.Error,
		root:     buildSettingsTree(snap.Settings.Rows),
	}
	v.filter = textinput.New()
	v.filter.Prompt = "/"
	v.refreshFlat()
	return v
}

// refreshFlat recomputes the visible node list from the current tree and
// filter state, then re-anchors the cursor on the node it was pointing at
// (falling back to a clamp when that node is no longer visible).
func (v *settingsView) refreshFlat() {
	var prev *node
	if v.cursor >= 0 && v.cursor < len(v.flat) {
		prev = v.flat[v.cursor]
	}

	v.flat = nil
	if v.query == "" {
		flattenTree(v.root, &v.flat)
	} else {
		keep := map[*node]bool{}
		filterKeep(v.root, v.query, keep)
		flattenFiltered(v.root, keep, &v.flat)
	}

	if prev != nil {
		for i, n := range v.flat {
			if n == prev {
				v.cursor = i
				return
			}
		}
		// prev is no longer visible (its ancestor collapsed): land on the
		// nearest visible ancestor instead of losing the cursor.
		for a := prev.parent; a != nil; a = a.parent {
			for i, n := range v.flat {
				if n == a {
					v.cursor = i
					return
				}
			}
		}
	}
	v.cursor = clamp(v.cursor, 0, len(v.flat)-1)
}

func (v settingsView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		snap := data.Snapshot(msg)
		v.layers = snap.Settings.Layers
		v.warnings = snap.Settings.Warnings
		v.err = snap.Settings.Error
		v.root = buildSettingsTree(snap.Settings.Rows)
		v.refreshFlat()
		return v, nil

	case tea.KeyMsg:
		if v.filterOn {
			switch msg.String() {
			case "esc", "ctrl+c":
				v.filterOn = false
				v.filter.Blur()
				v.filter.SetValue("")
				v.query = ""
				v.refreshFlat()
				return v, nil
			case "enter":
				v.filterOn = false
				v.filter.Blur()
				return v, nil
			}
			var cmd tea.Cmd
			v.filter, cmd = v.filter.Update(msg)
			if q := strings.ToLower(v.filter.Value()); q != v.query {
				v.query = q
				v.refreshFlat()
			}
			return v, cmd
		}
		return v.handleKey(msg)
	}
	return v, nil
}

func (v settingsView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "/":
		v.filterOn = true
		v.filter.Focus()
		return v, textinput.Blink
	case "j", "down":
		v.cursor = clamp(v.cursor+1, 0, len(v.flat)-1)
	case "k", "up":
		v.cursor = clamp(v.cursor-1, 0, len(v.flat)-1)
	case "g":
		v.cursor = 0
	case "G":
		v.cursor = len(v.flat) - 1
	case "enter", " ":
		if n := v.cur(); n != nil && !n.isLeaf {
			n.collapsed = !n.collapsed
			v.refreshFlat()
		}
	case "right":
		if n := v.cur(); n != nil && !n.isLeaf && n.collapsed {
			n.collapsed = false
			v.refreshFlat()
		}
	case "left":
		n := v.cur()
		if n == nil {
			break
		}
		target := n
		if n.isLeaf || n.collapsed {
			target = n.parent
		}
		if target != nil && !target.isLeaf {
			target.collapsed = true
			v.refreshFlat()
		}
	}
	return v, nil
}

func (v settingsView) cur() *node {
	if v.cursor < 0 || v.cursor >= len(v.flat) {
		return nil
	}
	return v.flat[v.cursor]
}

func (v settingsView) Keys() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys("enter", " "), key.WithHelp("enter", "expand/collapse")),
		key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	}
}

func (v settingsView) Selection() data.Selection {
	n := v.cur()
	if n == nil {
		return data.SettingRow{}
	}
	if n.isLeaf {
		return n.row
	}
	return data.SettingRow{Path: n.path}
}

func (v settingsView) Capturing() bool {
	return v.filterOn
}

func (v settingsView) View(w, h int) string {
	var lines []string
	lines = append(lines, v.headerLines(w)...)

	bodyH := max0(h - len(lines))
	if v.err != nil {
		lines = append(lines, truncateLine("unavailable: "+cleanText(*v.err), w))
		return strings.Join(normalizeFrame(strings.Join(lines, "\n"), h, w), "\n")
	}

	lines = append(lines, v.treeLines(bodyH, w)...)
	return strings.Join(normalizeFrame(strings.Join(lines, "\n"), h, w), "\n")
}

func (v settingsView) headerLines(w int) []string {
	var out []string
	if v.layers != nil {
		locked := "none"
		if v.layers.Locked != nil {
			locked = *v.layers.Locked
		}
		present := "absent"
		if v.layers.User.Present {
			present = "present"
		}
		out = append(out,
			truncateLine(headerStyle.Render("layers:")+" default "+v.layers.Base, w),
			truncateLine("        user    "+v.layers.User.Path+" ("+present+")", w),
			truncateLine("        locked  "+locked, w),
		)
	}
	for _, warn := range v.warnings {
		out = append(out, truncateLine(warnStyle.Render("warning: "+warn), w))
	}
	if v.filterOn {
		out = append(out, truncateLine(v.filter.View(), w))
	} else if v.query != "" {
		out = append(out, truncateLine("filter: "+v.query+"  (esc clears)", w))
	}
	return out
}

func (v settingsView) treeLines(bodyH, w int) []string {
	if bodyH <= 0 {
		return nil
	}
	total := len(v.flat)
	offset := 0
	if total > bodyH {
		offset = clamp(v.cursor-bodyH/2, 0, total-bodyH)
	}
	end := offset + bodyH
	if end > total {
		end = total
	}
	out := make([]string, 0, end-offset)
	for i := offset; i < end; i++ {
		out = append(out, v.renderNode(v.flat[i], i == v.cursor, w))
	}
	return out
}

func (v settingsView) renderNode(n *node, selected bool, w int) string {
	prefix := "  "
	if selected {
		prefix = cursorStyle.Render("›") + " "
	}
	indent := strings.Repeat("  ", n.depth)

	if !n.isLeaf {
		arrow := "▾"
		if n.collapsed {
			arrow = "▸"
		}
		left := indent + arrow + " " + n.name
		leftOut, _ := alignRight(left, "", max0(w-2))
		return prefix + branchStyle.Render(leftOut)
	}

	left := indent + n.name + ": " + compactJSON(n.row.Value)
	badge := badgeText(n.row.Origin)
	leftOut, badgeOut := alignRight(left, badge, max0(w-2))
	st := leafStyle
	if n.row.Origin == "locked" {
		st = badgeLockedStyle
	}
	return prefix + st.Render(leftOut) + badgeStyle(n.row.Origin).Render(badgeOut)
}
