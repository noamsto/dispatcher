package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

const overlayMaxWidth = 110

// composite draws box over base with its left edge at column x and its top row
// at line y. The base text left and right of the box is kept with its own
// colours; the box is clipped to w columns and to the base height.
func composite(base, box []string, x, y, w int) []string {
	out := append([]string(nil), base...)
	const reset = "\x1b[m"
	for i, b := range box {
		row := y + i
		if row < 0 || row >= len(out) || x >= w {
			continue
		}
		b = ansi.Truncate(b, w-x, "")
		bw := ansi.StringWidth(b)
		// A wide character straddling either box edge is dropped and its
		// visible half replaced by a space, so the row keeps exactly w cells.
		left := padRight(ansi.Truncate(out[row], x, ""), x)
		right := ansi.Cut(out[row], x+bw, w)
		if ansi.StringWidth(right) > w-x-bw {
			right = " " + ansi.Cut(out[row], x+bw+1, w)
		}
		out[row] = left + reset + b + reset + right
	}
	return out
}

// drawOverlay composites the panel content(iw) builds over base: centred,
// its top anchored h/6 down, or base unchanged when the body is too small.
func drawOverlay(base []string, w, h int, mode pillMode, content func(iw int) (panelSpec, []string)) string {
	bw := min(w-4, overlayMaxWidth)
	y := h / 6
	if bw < 8 || h-y < 3 {
		return strings.Join(base, "\n")
	}
	spec, lines := content(bw - 2)
	bh := min(len(lines)+2, h-y)
	box := renderPanel(bw, bh, spec, mode, lines)
	return strings.Join(composite(base, box, (w-bw)/2, y, w), "\n")
}

func (v overviewView) overlay(base []string, w, h int) string {
	return drawOverlay(base, w, h, v.pills, func(iw int) (panelSpec, []string) {
		switch v.mode {
		case ovDetail:
			spec := panelSpec{title: "DETAIL", color: colTeal, focused: true}
			var content []string
			if v.insp.open {
				return spec, v.insp.lines(iw)
			}
			if s := v.sel(); s.worker >= 0 {
				content = detailLines(v.model, s.worker, iw)
				spec.count = cleanText(workerLabel(v.model.Workers[s.worker]))
			}
			return spec, content
		case ovHelp:
			return panelSpec{title: "HELP", color: colTeal, focused: true}, helpLines(overviewHelp)
		}
		return v.promptPanel(iw)
	})
}

// promptPanel is the open reply, confirm or filter prompt as overlay content.
func (p actionPrompt) promptPanel(iw int) (panelSpec, []string) {
	spec := panelSpec{color: colTeal, focused: true}
	var content []string
	switch p.mode {
	case ovReply:
		spec.title, spec.color = "REPLY", colAmber
		in := p.input
		in.Prompt = "reply to " + ansi.Truncate(cleanText(p.pend.target.name), max(iw/2-12, 1), "…") + ": "
		in.Width = max(iw-ansi.StringWidth(in.Prompt)-1, 1)
		content = []string{in.View()}
	case ovConfirm:
		spec.title, spec.color = "CONFIRM", colAmber
		content = []string{" " + p.pend.prompt}
	case ovFilter:
		spec.title = "FILTER"
		in := p.input
		in.Width = max(iw-ansi.StringWidth(in.Prompt)-1, 1)
		content = []string{in.View()}
	}
	return spec, content
}

type helpKey struct{ keys, text string }

var overviewHelp = []helpKey{
	{"tab / shift+tab", "cycle focus: attention, checklists, agents, monitors (narrow: agents before checklists)"},
	{"h j k l / arrows", "h l: panels; j k: rows, crossing to or from the strip at an edge"},
	{"1-9", "act on the attention row with that number"},
	{"enter", "attention: select its worker, or inspect a crew item; else the detail overlay, esc closes"},
	{"p", "peek (crew where; switches to the pane inside tmux)"},
	{"n", "nudge (confirms)"},
	{"r", "reply (text input)"},
	{"m", "merge (confirms)"},
	{"/", "filter workers; enter keeps, esc clears"},
	{"[ ]", "views, R refresh"},
	{"?", "this help"},
	{"q", "quit"},
}

// helpLines is every key with its meaning, written "key meaning" so the text
// reads the same without colour.
func helpLines(keys []helpKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = " " + fg(colTeal).Render(k.keys) + " " + k.text
	}
	return out
}

func section(title string) string { return " " + fg(colDim).Bold(true).Render(title) }

// detailLines is the detail overlay for worker wi: header, phase timeline,
// acceptance, agent tree, its attention items and its recent events.
func detailLines(ov vm.OverviewView, wi, iw int) []string {
	w := ov.Workers[wi]
	plain := lipgloss.NewStyle()
	var out []string
	out = append(out,
		" "+fg(glyphColor(w.State)).Render(stateGlyph(w.State))+" "+fg(colText).Bold(true).Render(cleanText(workerLabel(w)))+
			fg(colDim).Render(cleanText("  "+w.Branch+"  "+ovStr(w.Tier, "?")+"  "+ovStr(w.Engine, "?")+"/"+ovStr(w.Model, "?")+"  "))+
			fg(glyphColor(w.State)).Render(cleanText(w.State)))
	if d := ovStr(w.Detail, ""); d != "" {
		out = append(out, "   "+fg(colDim).Render(cleanText(d)))
	}

	out = append(out, section("PHASE"))
	if w.Phase == nil {
		out = append(out, "   ?")
	} else {
		var b strings.Builder
		b.WriteString("  ")
		for _, s := range w.Phase.Steps {
			since := ""
			if s.SinceSec != nil {
				since = " " + ovAge(*s.SinceSec)
			}
			b.WriteString(" " + trackStyle(s.Status).Render(phaseGlyph(s.Status)+" "+cleanText(s.Name)) + fg(colDim).Render(since))
		}
		out = append(out, b.String())
	}

	ac := "—"
	if w.Acceptance != nil {
		ac = fmt.Sprintf("%d/%d", w.Acceptance.Done, w.Acceptance.Total)
	}
	out = append(out, section("ACCEPTANCE")+" "+ac)

	out = append(out, section("AGENTS"))
	if w.NeedsCleanup {
		out = append(out, "   "+fg(colDim).Render("none — stale, waiting on human cleanup"))
	}
	for _, a := range ov.Agents {
		if a.Crew != w.Crew || a.Branch != w.Branch {
			continue
		}
		out = append(out, "   lead "+cleanText(ovStr(a.Lead.Model, ovStr(a.Lead.Engine, "?"))+" "+ovStr(a.Lead.Tier, "?")+" "+a.Lead.State))
		if a.Roles == nil {
			out = append(out, "   roles ?")
		}
		for _, r := range a.Roles {
			st := roleState(r)
			out = append(out, "   "+fg(glyphColor(st)).Render(stateGlyph(st))+" "+cleanText(ovStr(r.Role, "?")+" "+r.State)+
				fg(colDim).Render(" "+cleanText(ovStr(r.Detail, ""))))
		}
		out = append(out, "   "+fg(colDim).Render("execute subagents: not reported"))
	}

	out = append(out, section("ATTENTION"))
	n := 0
	for _, a := range ov.Attention {
		if a.Crew != w.Crew || a.Branch != w.Branch {
			continue
		}
		n++
		cmd := "—"
		if a.Action != nil {
			cmd = shownCommand(*a.Action)
		}
		glyph, gc := severityGlyph(a.Severity)
		out = append(out, "   "+fg(gc).Render(glyph)+plain.Render(" "+cleanText(a.Message)+" ")+fg(colDim).Render("→ "+cmd))
		if a.Clears != "" {
			out = append(out, clearsLines(a.Clears, iw)...)
		}
	}
	if n == 0 {
		out = append(out, "   "+fg(colDim).Render("none"))
	}

	out = append(out, section("RECENT EVENTS"))
	n = 0
	for _, e := range ov.Events {
		if !eventOf(e.Worker, w.Branch) || n == 6 {
			continue
		}
		n++
		out = append(out, "   "+fg(colDim).Render(ovAge(ov.GeneratedAt-e.TS))+" "+cleanText(e.Text))
	}
	if n == 0 {
		out = append(out, "   "+fg(colDim).Render("none"))
	}
	return out
}

// clearsLines is the "To clear:" line word-wrapped to the overlay's inner
// width, continuation lines aligned under the text.
func clearsLines(clears string, iw int) []string {
	const label = "To clear: "
	indent := strings.Repeat(" ", 5+len(label))
	lines := strings.Split(ansi.Wrap(cleanText(clears), max(iw-len(indent), 1), ""), "\n")
	out := make([]string, len(lines))
	for i, l := range lines {
		prefix := indent
		if i == 0 {
			prefix = "     " + label
		}
		out[i] = fg(colDim).Render(prefix + l)
	}
	return out
}

// eventOf reports whether bus sender from is branch's worker (worker:<branch>
// with an optional #session) or one of its role panes (role:<branch>:<role>).
func eventOf(from, branch string) bool {
	if strings.HasPrefix(from, "role:"+branch+":") {
		return true
	}
	rest, ok := strings.CutPrefix(from, "worker:"+branch)
	return ok && (rest == "" || rest[0] == '#')
}

func phaseGlyph(status string) string {
	return phaseCells(&vm.Phase{Steps: []vm.PhaseStep{{Status: status}}})[0].glyph
}
