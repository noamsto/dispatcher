package ui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// inspectMsg is an inspect command's result. id is the attention item it was
// run for: a result applies only while that item's overlay is still open.
type inspectMsg struct {
	id  string
	out string
	err string
}

// inspectState is the open inspect overlay for a crew-level attention item.
type inspectState struct {
	open bool
	item vm.AttentionItem
	done bool
	out  string
	err  string
}

// inspectable reports whether a's action is the item's own keyless read-only
// inspect command.
func inspectable(a vm.AttentionItem) bool {
	return a.Action != nil && a.Action.Key == "" && a.Action.Verb == "inspect" && len(a.Action.Argv) > 0
}

// startInspect opens the inspect overlay for a and runs its Argv off the
// Update loop. An item whose command holds control bytes runs nothing.
func (p *actionPrompt) startInspect(a vm.AttentionItem) tea.Cmd {
	if hasControl(*a.Action) {
		return statusCmd("run: "+controlNotice, false)
	}
	p.insp = inspectState{open: true, item: a}
	p.mode = ovDetail
	ctx, r, argv, dir, id := p.acts.ctx, p.acts.runner, a.Action.Argv, deref(a.Action.Dir), a.ID
	return func() tea.Msg {
		out, err := runArgv(ctx, r, dir, argv)
		if err != nil {
			return inspectMsg{id: id, err: err.Error()}
		}
		return inspectMsg{id: id, out: out}
	}
}

// applyInspect records a result, dropping one whose overlay is gone or was
// replaced by another item's.
func (p *actionPrompt) applyInspect(msg inspectMsg) {
	if !p.insp.open || p.mode != ovDetail || p.insp.item.ID != msg.id {
		return
	}
	p.insp.done, p.insp.out, p.insp.err = true, msg.out, msg.err
}

// inspectKey closes the overlay on esc or enter, consuming the key; any other
// key dismisses it and falls through to act on the view as usual.
func (p *actionPrompt) inspectKey(s string) (consumed bool) {
	if !p.insp.open || p.mode != ovDetail {
		return false
	}
	p.insp, p.mode = inspectState{}, ovNormal
	return s == "esc" || s == "enter"
}

func (s inspectState) lines(iw int) []string {
	a := s.item
	out := wrapLines(" "+a.Message, iw, 0)
	for _, kv := range [][2]string{
		{"kind", string(a.Kind)}, {"severity", string(a.Severity)}, {"crew", a.Crew},
		{"host", a.Host}, {"age", ovAge(a.AgeSec)}, {"command", shownCommand(*a.Action)},
	} {
		if kv[1] != "" {
			out = append(out, wrapLines(kv[0]+": "+kv[1], iw, 3)...)
		}
	}
	out = append(out, section("OUTPUT"))
	switch {
	case !s.done:
		out = append(out, "   "+fg(colDim).Render("running…"))
	case s.err != "":
		out = append(out, wrapLines("error: "+s.err, iw, 3)...)
	default:
		for _, l := range inspectOutput(s.out) {
			out = append(out, wrapLines(l, iw, 3)...)
		}
	}
	return out
}

// wrapLines is text cleaned and word-wrapped to iw columns, every line
// indented by indent spaces.
func wrapLines(text string, iw, indent int) []string {
	lines := strings.Split(ansi.Wrap(cleanText(ansi.Strip(text)), max(iw-indent, 1), ""), "\n")
	pad := strings.Repeat(" ", indent)
	for i, l := range lines {
		lines[i] = pad + l
	}
	return lines
}

// inspectOutput is out as key: value lines when it is one JSON object, else
// its lines.
func inspectOutput(out string) []string {
	var obj map[string]any
	if json.Unmarshal([]byte(out), &obj) == nil && obj != nil {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		lines := make([]string, len(keys))
		for i, k := range keys {
			lines[i] = k + ": " + jsonValue(obj[k])
		}
		return lines
	}
	var tab strings.Builder
	tw := tabwriter.NewWriter(&tab, 0, 1, 2, ' ', 0)
	_, _ = tw.Write([]byte(strings.TrimRight(out, "\r\n")))
	_ = tw.Flush()
	lines := strings.Split(tab.String(), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return []string{"(no output)"}
	}
	return lines
}

func jsonValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
