// Package once renders a data.Snapshot as the plain "--once" text: the four
// "== Pane ==" sections tests/fixtures/crew-dash/once.golden pins, byte for
// byte.
package once

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

type line struct {
	style string // "h" heading, "l" locked, "n" normal
	text  string
}

// Render is the pure Snapshot → string renderer. color adds the same SGR
// bash's `once` mode used: bold headings, bold-yellow locked settings rows.
func Render(snap data.Snapshot, color bool) string {
	var b strings.Builder
	for _, l := range onceLines(snap) {
		switch {
		case l.style == "h" && color:
			fmt.Fprintf(&b, "\x1b[1m%s\x1b[0m\n", l.text)
		case l.style == "l" && color:
			fmt.Fprintf(&b, "\x1b[1;33m%s\x1b[0m\n", l.text)
		default:
			fmt.Fprintf(&b, "%s\n", l.text)
		}
	}
	return b.String()
}

func onceLines(snap data.Snapshot) []line {
	panes := []struct{ key, title string }{
		{"settings", "Settings"},
		{"budget", "Budget"},
		{"runs", "Runs"},
		{"roster", "Roster"},
	}
	var out []line
	for i, p := range panes {
		if i > 0 {
			out = append(out, line{style: "n", text: ""})
		}
		out = append(out, line{style: "h", text: "== " + p.title + " =="})
		out = append(out, renderPane(snap, p.key)...)
	}
	return out
}

func renderPane(snap data.Snapshot, pane string) []line {
	switch pane {
	case "settings":
		return settingsPane(snap)
	case "budget":
		return budgetPane(snap)
	case "runs":
		return runsPane(snap)
	case "roster":
		return rosterPane(snap)
	}
	return nil
}

// ---------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------

func settingsPane(snap data.Snapshot) []line {
	var out []line
	out = append(out, layerLines(snap.Settings.Layers)...)
	for _, w := range snap.Settings.Warnings {
		out = append(out, line{style: "n", text: "warning: " + w})
	}
	if snap.Settings.Error != nil {
		out = append(out, line{style: "n", text: "unavailable: " + *snap.Settings.Error})
		return out
	}
	out = append(out, settingsBody(snap.Settings.Rows)...)
	return out
}

func layerLines(layers *data.Layers) []line {
	if layers == nil {
		return nil
	}
	locked := "none"
	if layers.Locked != nil {
		locked = *layers.Locked
	}
	present := "absent"
	if layers.User.Present {
		present = "present"
	}
	return []line{
		{style: "n", text: "layers: default " + layers.Base},
		{style: "n", text: "        user    " + layers.User.Path + " (" + present + ")"},
		{style: "n", text: "        locked  " + locked},
	}
}

func leafPrefix(r data.SettingRow) string {
	depth := len(r.Path) - 1
	last := r.Path[len(r.Path)-1]
	return strings.Repeat("  ", depth) + last + ": " + compactJSON(r.Value)
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func badgeOf(origin string) string {
	switch origin {
	case "base":
		return "default"
	case "user":
		return "user"
	case "env":
		return "env"
	case "locked":
		return "🔒 locked"
	default:
		return origin
	}
}

func commonLen(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func settingsBody(rows []data.SettingRow) []line {
	prefixes := make([]string, len(rows))
	maxlen := 0
	for i, r := range rows {
		prefixes[i] = leafPrefix(r)
		if n := utf8.RuneCountInString(prefixes[i]); n > maxlen {
			maxlen = n
		}
	}
	cap_ := maxlen
	if cap_ > 48 {
		cap_ = 48
	}

	var out []line
	var prev []string
	for idx, r := range rows {
		prefix := r.Path[:len(r.Path)-1]
		common := commonLen(prev, prefix)
		for d := common; d < len(prefix); d++ {
			out = append(out, line{style: "n", text: strings.Repeat("  ", d) + prefix[d]})
		}
		pfx := prefixes[idx]
		pad := cap_ + 2 - utf8.RuneCountInString(pfx)
		if pad < 2 {
			pad = 2
		}
		style := "n"
		if r.Origin == "locked" {
			style = "l"
		}
		out = append(out, line{style: style, text: pfx + strings.Repeat(" ", pad) + badgeOf(r.Origin)})
		prev = prefix
	}
	return out
}

// ---------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------

var engineOrder = []string{"claude", "codex", "cursor", "pi"}

func budgetPane(snap data.Snapshot) []line {
	b := snap.Budget
	if b.Error != nil {
		return []line{{style: "n", text: "unavailable: " + *b.Error}}
	}
	age := snap.Now - b.Report.FetchedEpoch
	fetchedLine := "fetched " + reltime(age) + " ago"
	if age > 7200 {
		fetchedLine += " (stale)"
	}
	out := []line{{style: "n", text: fetchedLine}}
	for _, e := range engineOrder {
		out = append(out, engineLines(e, b.Report.Engines[e])...)
	}
	return out
}

func engineLines(e string, v *data.EngineBudget) []line {
	if v == nil {
		return []line{{style: "n", text: e + ": unknown"}}
	}
	heading := e + " (" + v.Source + ")"
	if v.PlanType != nil {
		heading += " [" + *v.PlanType + "]"
	}

	table := [][]string{{"window", "used", "pace", "resets in", "verdict"}}
	for _, w := range v.Windows {
		table = append(table, windowRowCells(w))
	}
	widths := tableWidths(table)
	left := []bool{true, false, false, true, true}

	out := []line{{style: "h", text: heading}}
	for _, row := range table {
		out = append(out, line{style: "n", text: "  " + padRow(row, widths, left)})
	}

	if v.Source == "openrouter_key" {
		if v.TargetUSD != nil {
			out = append(out, line{style: "n", text: fmt.Sprintf(
				"  spend $%s of $%s target, %s%% of month elapsed",
				usd(f64(v.SpendUSD)), usd(*v.TargetUSD), formatNumber(f64(v.ElapsedPct)),
			)})
		} else {
			out = append(out, line{style: "n", text: "  spend $" + usd(f64(v.SpendUSD)) + " month-to-date (no target)"})
		}
		if v.Projection != nil {
			out = append(out, line{style: "n", text: "  " + *v.Projection})
		}
	}
	return out
}

func windowRowCells(w data.Window) []string {
	ahead := "—"
	if w.AheadPts != nil {
		if *w.AheadPts > 0 {
			ahead = fmt.Sprintf("+%d", *w.AheadPts)
		} else {
			ahead = strconv.FormatInt(*w.AheadPts, 10)
		}
	}
	resets := "—"
	if w.ResetsInS != nil {
		resets = reltime(*w.ResetsInS)
	}
	verdict := "—"
	if w.Verdict != nil {
		verdict = *w.Verdict
	}
	return []string{w.Key, formatNumber(w.UsedPct) + "%", ahead, resets, verdict}
}

func f64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------

type crewGroup struct {
	crew  string
	rows  []data.RetroRow
	maxT0 int64
}

func crewsGrouped(rows []data.RetroRow) []crewGroup {
	byCrew := map[string][]data.RetroRow{}
	for _, r := range rows {
		if r.Crew == nil {
			continue
		}
		byCrew[*r.Crew] = append(byCrew[*r.Crew], r)
	}
	crews := make([]string, 0, len(byCrew))
	for c := range byCrew {
		crews = append(crews, c)
	}
	sort.Strings(crews)

	groups := make([]crewGroup, 0, len(crews))
	for _, c := range crews {
		rs := append([]data.RetroRow{}, byCrew[c]...)
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].T0 < rs[j].T0 })
		maxT0 := rs[0].T0
		for _, r := range rs {
			if r.T0 > maxT0 {
				maxT0 = r.T0
			}
		}
		groups = append(groups, crewGroup{crew: c, rows: rs, maxT0: maxT0})
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].maxT0 > groups[j].maxT0 })
	if len(groups) > 5 {
		groups = groups[:5]
	}
	return groups
}

func tagstrDash(notes []data.Note) string {
	var order []string
	seen := map[string]struct{}{}
	counts := map[string]int{}
	for _, n := range notes {
		counts[n.Tag]++
		if _, ok := seen[n.Tag]; !ok {
			seen[n.Tag] = struct{}{}
			order = append(order, n.Tag)
		}
	}
	parts := make([]string, 0, len(order))
	for _, t := range order {
		clean := cleanText(t)
		if counts[t] > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", clean, counts[t]))
		} else {
			parts = append(parts, clean)
		}
	}
	return strings.Join(parts, ", ")
}

func crewNotesLines(g crewGroup) []line {
	var notes []data.Note
	for _, r := range g.rows {
		notes = append(notes, r.Notes...)
	}
	var summary *data.Note
	for i := range notes {
		if notes[i].Tag == "session_summary" {
			summary = &notes[i]
		}
	}
	var rest []data.Note
	for _, n := range notes {
		if n.Tag != "session_summary" {
			rest = append(rest, n)
		}
	}

	out := []line{{style: "h", text: "crew " + g.crew}}
	summaryText := "(none)"
	if summary != nil {
		summaryText = cleanText(summary.Detail)
	}
	out = append(out, line{style: "n", text: "  summary: " + summaryText})
	if len(rest) > 0 {
		out = append(out, line{style: "n", text: "  tags: " + tagstrDash(rest)})
	}
	last3 := rest
	if len(last3) > 3 {
		last3 = last3[len(last3)-3:]
	}
	for i := len(last3) - 1; i >= 0; i-- {
		n := last3[i]
		out = append(out, line{style: "n", text: "  " + cleanText(n.Tag) + ": " + cleanText(n.Detail)})
	}
	return out
}

func ratingsTable(groups []data.RatingGroup) []line {
	if len(groups) == 0 {
		return []line{{style: "n", text: "no runs swept for this repo yet"}}
	}

	sorted := append([]data.RatingGroup{}, groups...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		return a.Model < b.Model
	})

	headers := []string{"tier", "engine", "model", "n", "pr%", "merge%", "burn(med)"}
	rows := make([][]string, 0, len(sorted))
	for _, g := range sorted {
		rows = append(rows, []string{
			g.Tier, g.Engine, g.Model,
			renderAgg(g.N.K, g.N.N, optNumber(g.N.Value)),
			renderAgg(g.PrPct.K, g.PrPct.N, optNumber(g.PrPct.Value)),
			renderAgg(g.MergePct.K, g.MergePct.N, optNumber(g.MergePct.Value)),
			renderAgg(g.BurnMedian.K, g.BurnMedian.N, optFmt1(g.BurnMedian.Value)),
		})
	}
	all := append([][]string{headers}, rows...)
	widths := tableWidths(all)
	left := []bool{true, true, true, false, false, false, false}

	out := []line{{style: "n", text: padRow(headers, widths, left)}}
	for _, r := range rows {
		out = append(out, line{style: "n", text: padRow(r, widths, left)})
	}
	return out
}

func runsPane(snap data.Snapshot) []line {
	r := snap.Runs

	var partA []line
	if r.RetroError != nil {
		partA = []line{{style: "n", text: "unavailable: " + *r.RetroError}}
	} else {
		var rows []data.RetroRow
		if r.Retro != nil {
			rows = r.Retro.Rows
		}
		groups := crewsGrouped(rows)
		if len(groups) == 0 {
			partA = []line{{style: "n", text: "no retro notes yet"}}
		} else {
			for _, g := range groups {
				partA = append(partA, crewNotesLines(g)...)
			}
		}
	}

	var partB []line
	if r.RatingsError != nil {
		partB = []line{{style: "n", text: "unavailable: " + *r.RatingsError}}
	} else {
		partB = ratingsTable(r.RatingsGroups)
	}

	out := append([]line{}, partA...)
	out = append(out, line{style: "n", text: ""})
	out = append(out, partB...)
	return out
}

// ---------------------------------------------------------------------
// Roster
// ---------------------------------------------------------------------

func strField(w map[string]any, key string) (string, bool) {
	v, ok := w[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func workerCells(w map[string]any) []string {
	name := "—"
	if s, ok := strField(w, "name"); ok {
		name = s
	} else if s, ok := strField(w, "branch"); ok {
		name = s
	}
	state := "—"
	if s, ok := strField(w, "state"); ok {
		state = s
	}
	tier, engine, model := "—", "—", "—"
	if s, ok := strField(w, "tier"); ok {
		tier = s
	}
	if s, ok := strField(w, "engine"); ok {
		engine = s
	}
	if s, ok := strField(w, "model"); ok {
		model = s
	}
	var ageS int64
	switch v := w["age_s"].(type) {
	case int64:
		ageS = v
	case float64:
		ageS = int64(math.Floor(v))
	}
	prURL := "—"
	if s, ok := strField(w, "pr_url"); ok {
		prURL = cleanText(s)
	}
	return []string{cleanText(name), cleanText(state), tier + "/" + engine + "/" + model, reltime(ageS), prURL}
}

func rosterCrewLines(c data.RosterCrew) []line {
	out := []line{{style: "h", text: "crew " + c.ID}}
	if c.WorkersError != nil {
		out = append(out, line{style: "n", text: "  unavailable: " + *c.WorkersError})
	} else if len(c.Workers) > 0 {
		headers := []string{"name", "state", "tier/engine/model", "age", "pr"}
		wrows := make([][]string, 0, len(c.Workers))
		for _, w := range c.Workers {
			wrows = append(wrows, workerCells(w))
		}
		all := append([][]string{headers}, wrows...)
		widths := tableWidths(all)
		left := []bool{true, true, true, true, true}
		out = append(out, line{style: "n", text: "  " + padRow(headers, widths, left)})
		for _, r := range wrows {
			out = append(out, line{style: "n", text: "  " + padRow(r, widths, left)})
		}
	}
	if c.HoldsError != nil {
		out = append(out, line{style: "n", text: "  holds unavailable: " + *c.HoldsError})
	} else {
		for _, h := range c.Holds {
			out = append(out, line{style: "n", text: "  " + holdLine(h)})
		}
	}
	return out
}

func holdLine(h map[string]any) string {
	id := fmt.Sprintf("%v", h["id"])
	wait, _ := h["wait"].(map[string]any)
	task, _ := h["task"].(map[string]any)
	engine, _ := wait["engine"].(string)
	window, _ := wait["window"].(string)
	resetsAt, _ := wait["resets_at"].(float64)
	title := ""
	if task != nil {
		if s, ok := task["title"].(string); ok {
			title = s
		}
	}
	return "hold " + id + ": " + engine + " " + window + " until " + isoUTC(int64(resetsAt)) + " — " + cleanText(title)
}

func rosterPane(snap data.Snapshot) []line {
	r := snap.Roster
	if r.Error != nil {
		return []line{{style: "n", text: "unavailable: " + *r.Error}}
	}
	if len(r.Crews) == 0 {
		return []line{{style: "n", text: "no active crew"}}
	}
	var out []line
	for _, c := range r.Crews {
		out = append(out, rosterCrewLines(c)...)
	}
	return out
}
