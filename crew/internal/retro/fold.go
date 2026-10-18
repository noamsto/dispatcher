package retro

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

// "plan_required_unaudited" is synthesized by `crew retro` itself, not authored
// by a worker or the dispatcher: do not add it to a protocol retro-notes table.
var vocab = []string{
	"command_not_found", "gate_thrash", "approach_abandoned", "consult_failed",
	"rung_blocked", "review_unavailable", "plan_required_unaudited", "other",
	"misrouted", "fanout_binder", "spec_too_thin", "session_summary",
}

const (
	none        = "—"
	sampleRunes = 60
)

// note is one retro note. The tag is cleaned once so grouping, the vocabulary
// check and every output name the same value.
type note struct {
	Seam   string `json:"seam"`
	Tag    string `json:"tag"`
	Detail string `json:"detail"`
}

// row is one run (a dispatch with the events its window owns) or the
// dispatcher's notes for one crew.
type row struct {
	Kind    string  `json:"kind"`
	Crew    string  `json:"crew"`
	Branch  string  `json:"branch"`
	Engine  string  `json:"engine"`
	Model   string  `json:"model"`
	Tier    string  `json:"tier"`
	Outcome string  `json:"outcome"`
	T0      float64 `json:"t0"`
	Notes   []note  `json:"notes"`
}

type group struct {
	Tag        string   `json:"tag"`
	Known      bool     `json:"known"`
	Hits       int      `json:"hits"`
	Runs       int      `json:"runs"`
	Engines    []string `json:"engines"`
	NondonePct *int     `json:"nondone_pct"`
	Details    []string `json:"details"`
}

func doc(e bus.Event) map[string]any {
	m, _ := e.Doc.(map[string]any)
	return m
}

// ts is an event's timestamp, 0 when it has none.
func ts(e bus.Event) float64 {
	f, _ := e.TS.AsFloat()
	return f
}

func sortedByTS(evs []bus.Event) []bus.Event {
	out := slices.Clone(evs)
	sort.SliceStable(out, func(i, j int) bool { return ts(out[i]) < ts(out[j]) })
	return out
}

// sessionBranch is a sender's branch: no "worker:" prefix, no "#session" suffix.
func sessionBranch(from string) string {
	from = strings.TrimPrefix(from, "worker:")
	if i := strings.LastIndex(from, "#"); i >= 0 {
		return from[:i]
	}
	return from
}

// bodyObj is a message body that is a JSON string holding an object. Any other
// body costs that one note, not the fold (#25).
func bodyObj(e bus.Event) map[string]any {
	s, _ := doc(e)["body"].(string)
	var o map[string]any
	if json.Unmarshal([]byte(s), &o) != nil {
		return nil
	}
	return o
}

// text is a column's text: a string as is, anything else compact JSON; null and
// false have none.
func text(v any) string {
	if !jsonval.Truthy(v) {
		return none
	}
	return jsonval.RawText(v)
}

// notesOf turns note objects into notes, dropping repeats of the whole triple:
// a worker keeps its mid-execute seam note in the final snapshot too, while
// {seam,tag} repeats legitimately with a different detail.
func notesOf(objs []map[string]any) []note {
	var out []note
	for _, o := range objs {
		n := note{Tag: clean(jsonval.RawText(o["tag"]))}
		if o["seam"] != nil {
			n.Seam = jsonval.RawText(o["seam"])
		}
		if jsonval.Truthy(o["detail"]) {
			n.Detail = jsonval.RawText(o["detail"])
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func bodies(evs []bus.Event) (out []map[string]any) {
	for _, e := range sortedByTS(evs) {
		if o := bodyObj(e); o != nil {
			out = append(out, o)
		}
	}
	return out
}

// runs is one row per dispatch. Run i owns [runs[i].ts, runs[i+1].ts) of its
// branch, so a re-dispatch on a reused branch cannot inherit the notes of the
// run before it.
func runs(events []bus.Event) []row {
	var ds []bus.Event
	for _, e := range events {
		if e.Kind == bus.KindDispatch {
			ds = append(ds, e)
		}
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Branch != ds[j].Branch {
			return ds[i].Branch < ds[j].Branch
		}
		return ts(ds[i]) < ts(ds[j])
	})
	out := make([]row, len(ds))
	for i, d := range ds {
		t1 := math.Inf(1)
		if i+1 < len(ds) && ds[i+1].Branch == d.Branch {
			t1 = ts(ds[i+1])
		}
		out[i] = buildRun(events, d, t1)
	}
	return out
}

func buildRun(events []bus.Event, d bus.Event, t1 float64) row {
	var statuses, metrics, retros, reviews []bus.Event
	resumed := false
	for _, e := range events {
		if ts(e) < ts(d) || ts(e) >= t1 {
			continue
		}
		// A `dispatch resume` row has no `from`, so it is matched by branch.
		resumed = resumed || e.Kind == bus.KindResume && e.Branch == d.Branch
		if e.Kind == bus.KindDispatch || sessionBranch(e.From) != d.Branch {
			continue
		}
		switch {
		case e.Kind == bus.KindStatus:
			statuses = append(statuses, e)
		case e.Kind != bus.KindMsg:
		case strings.HasPrefix(e.To, "metrics:"):
			metrics = append(metrics, e)
		case strings.HasPrefix(e.To, "retro:") && strings.HasPrefix(e.From, "worker:"):
			retros = append(retros, e)
		case strings.HasPrefix(e.To, "review:"):
			reviews = append(reviews, e)
		}
	}

	// A state that is not a scalar costs this run its outcome, not the fold (#25).
	var state any
	if s := sortedByTS(statuses); len(s) > 0 {
		body, _ := doc(s[len(s)-1])["body"].(map[string]any)
		state = body["state"]
	}
	switch state.(type) {
	case []any, map[string]any:
		state = nil
	}

	// Latest snapshot only: its notes supersede an earlier one's, so a resumed
	// run does not duplicate them.
	var m map[string]any
	objs := bodies(retros)
	if s := sortedByTS(metrics); len(s) > 0 {
		m = bodyObj(s[len(s)-1])
		snap, _ := m["notes"].([]any)
		for _, x := range snap {
			if o, ok := x.(map[string]any); ok {
				objs = append(objs, o)
			}
		}
	}
	if unaudited(doc(d), m, resumed, reviews) {
		objs = append(objs, map[string]any{
			"seam": "plan", "tag": "plan_required_unaudited",
			"detail": "plan: required but no plan seam and plan_critic_first_pass is null in the metrics snapshot",
		})
	}

	return row{
		Kind: "run", Crew: d.CrewID, Branch: d.Branch,
		Engine: text(doc(d)["engine"]), Model: text(doc(d)["model"]), Tier: text(doc(d)["tier"]),
		Outcome: text(state), T0: ts(d), Notes: notesOf(objs),
	}
}

// unaudited flags `plan: required` with a null plan-critic verdict: a skipped
// gate (#179). Exempt are runs with no plan phase, resumed runs, runs that
// never reached the review gate, and runs with a valid plan seam on the bus
// (the snapshot may have forgotten the verdict).
func unaudited(d, m map[string]any, resumed bool, reviews []bus.Event) bool {
	if d["plan"] != "required" || d["tier"] == "trivial" || d["task_kind"] == "review" ||
		resumed || d["resume"] == true || m == nil || m["plan_critic_first_pass"] != nil || m["review_mode"] == "none" {
		return false
	}
	for _, e := range reviews {
		o := bodyObj(e)
		if _, tagged := o["tag"]; o == nil || tagged || o["seam"] != "plan" {
			continue
		}
		if slices.Contains([]any{"accept", "revise", "reject"}, o["plan_critic_first_pass"]) {
			return false
		}
	}
	return true
}

// dispatcherRows are the dispatcher's notes. They are crew-scoped, not
// run-scoped: their `from` matches no branch, so no run window can hold them.
func dispatcherRows(events []bus.Event) (out []row) {
	byTo := map[string][]bus.Event{}
	for _, e := range events {
		if e.Kind == bus.KindMsg && strings.HasPrefix(e.To, "retro:") && strings.HasPrefix(e.From, "dispatcher:") {
			byTo[e.To] = append(byTo[e.To], e)
		}
	}
	tos := make([]string, 0, len(byTo))
	for to := range byTo {
		tos = append(tos, to)
	}
	sort.Strings(tos)
	for _, to := range tos {
		crew := strings.TrimPrefix(to, "retro:")
		out = append(out, row{
			Kind: "dispatcher", Crew: crew, Branch: "dispatcher:" + crew,
			Engine: none, Model: none, Tier: none, Outcome: none,
			T0: ts(sortedByTS(byTo[to])[0]), Notes: notesOf(bodies(byTo[to])),
		})
	}
	return out
}

// collect is every row with notes: runs by branch then dispatch, then the
// dispatcher's. A clean run is silent.
func collect(events []bus.Event) []row {
	rows := []row{}
	for _, r := range append(runs(events), dispatcherRows(events)...) {
		if len(r.Notes) > 0 {
			rows = append(rows, r)
		}
	}
	return rows
}

// clean makes agent-authored text safe for a padded row. Tab/newline/CR become
// a space to keep the columns; the other control characters go, because a raw
// ESC repaints the table (one note could erase or forge the row above it), and
// the bidi overrides let a string misrepresent itself in place.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20, r >= 0x7f && r <= 0x9f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

// groups rolls the notes up by tag: known tags in vocabulary order, then
// unknown ones alphabetically.
func groups(rows []row) ([]group, []string) {
	byTag := map[string]*group{}
	notDone := map[string]map[string]bool{} // tag -> run -> outcome is not done
	for _, r := range rows {
		for _, n := range r.Notes {
			g := byTag[n.Tag]
			if g == nil {
				g = &group{Tag: n.Tag, Known: slices.Contains(vocab, n.Tag), Engines: []string{}, Details: []string{}}
				byTag[n.Tag], notDone[n.Tag] = g, map[string]bool{}
			}
			g.Hits++
			g.Details = append(g.Details, n.Detail)
			if r.Kind != "run" {
				continue
			}
			if !slices.Contains(g.Engines, r.Engine) {
				g.Engines = append(g.Engines, r.Engine)
			}
			run := r.Branch + "@" + strconv.FormatFloat(r.T0, 'f', -1, 64)
			if _, seen := notDone[n.Tag][run]; !seen {
				notDone[n.Tag][run] = r.Outcome != "done"
			}
		}
	}
	out := []group{}
	for tag, g := range byTag {
		sort.Strings(g.Engines)
		if g.Runs = len(notDone[tag]); g.Runs > 0 {
			nondone := 0
			for _, nd := range notDone[tag] {
				if nd {
					nondone++
				}
			}
			pct := int(math.Round(float64(nondone*100) / float64(g.Runs)))
			g.NondonePct = &pct
		}
		out = append(out, *g)
	}
	rank := func(g group) int {
		if i := slices.Index(vocab, g.Tag); i >= 0 {
			return i
		}
		return len(vocab)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].Tag < out[j].Tag
	})
	unknown := []string{}
	for _, g := range out {
		if !g.Known {
			unknown = append(unknown, g.Tag)
		}
	}
	return out, unknown
}

// reportJSON is the --report --json object.
func reportJSON(rows []row) any {
	tags, unknown := groups(rows)
	return struct {
		Tags    []group  `json:"tags"`
		Unknown []string `json:"unknown"`
		Rows    []row    `json:"rows"`
	}{tags, unknown, rows}
}

// reportTable is the padded --report table. Widths and padding are computed
// here, never `column -t`: BSD and util-linux pad "—" differently.
func reportTable(rows []row) string {
	left := []bool{true, false, false, true, false, true}
	tags, unknown := groups(rows)
	table := [][]string{{"tag", "hits", "runs", "engines", "nondone", "sample"}}
	for _, g := range tags {
		engines, pct, sample := none, none, ""
		if len(g.Engines) > 0 {
			engines = strings.Join(g.Engines, ",")
		}
		if g.NondonePct != nil {
			pct = strconv.Itoa(*g.NondonePct)
		}
		if i := slices.IndexFunc(g.Details, func(d string) bool { return d != "" }); i >= 0 {
			sample = clean(g.Details[i])
		}
		if r := []rune(sample); len(r) > sampleRunes {
			sample = string(r[:sampleRunes]) + "…"
		}
		table = append(table, []string{g.Tag, strconv.Itoa(g.Hits), strconv.Itoa(g.Runs), engines, pct, sample})
	}
	widths := make([]int, len(left))
	for _, cells := range table {
		for c, cell := range cells {
			widths[c] = max(widths[c], utf8.RuneCountInString(cell))
		}
	}
	lines := make([]string, len(table))
	for i, cells := range table {
		for c, cell := range cells {
			pad := strings.Repeat(" ", widths[c]-utf8.RuneCountInString(cell))
			switch {
			case !left[c]:
				cells[c] = pad + cell
			case c < len(cells)-1:
				cells[c] = cell + pad
			}
		}
		lines[i] = strings.Join(cells, "  ")
	}
	if len(unknown) > 0 {
		lines = append(lines, fmt.Sprintf("%d unrecognized tag(s): %s", len(unknown), strings.Join(unknown, ", ")))
	}
	return strings.Join(lines, "\n")
}

var tsvEscape = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// bareRows is one tab-separated row per run with notes.
func bareRows(rows []row) string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		counts := map[string]int{}
		var tags []string
		for _, n := range r.Notes {
			if counts[n.Tag]++; counts[n.Tag] == 1 {
				tags = append(tags, n.Tag)
			}
		}
		for j, t := range tags {
			if counts[t] > 1 {
				tags[j] = fmt.Sprintf("%s x%d", t, counts[t])
			}
		}
		cells := []string{r.Branch, r.Engine, r.Model, r.Tier, r.Outcome, strings.Join(tags, ",")}
		for c, cell := range cells {
			cells[c] = tsvEscape.Replace(cell)
		}
		lines[i] = strings.Join(cells, "\t")
	}
	return strings.Join(lines, "\n")
}
