package rosterrender

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// The four sector classes a planet's ring colour comes from. failed and exited
// share the done sector and carry planet.failed.
const (
	classWorking = "working"
	classBlocked = "blocked"
	classPROpen  = "pr_open"
	classDone    = "done"
)

// doneFold is how long a finished worker stays drawn, fading, before it is
// folded into the legend count.
const doneFold = 5 * time.Minute

// staleStatus is how long a working or blocked worker may stay silent before it
// is likely wedged. pr_open is exempt because silence is its normal state.
const staleStatus = 45 * time.Minute

// maxText bounds every bus- or tmux-sourced string that reaches the diagram.
const maxText = 24

// maxFreeText bounds the free-text title and detail before they are wrapped.
const maxFreeText = 200

// classRank is a class's place in the ring, clockwise from 12 o'clock.
var classRank = map[string]int{classBlocked: 0, classWorking: 1, classPROpen: 2, classDone: 3}

var issueRe = regexp.MustCompile(`[0-9]+`)

// foldIDRe is the id shape the roster fold appends to a colliding codename as
// `name·id`.
var foldIDRe = regexp.MustCompile(`\A(?:[A-Za-z]+-[0-9]+|[0-9]+)\z`)

// moon is one role pane of a drawn worker. x and y are set by the layout.
type moon struct {
	role    string
	working bool
	x, y    float64
}

// planet is one drawn worker. Every field has been through clean or
// sanitizeText.
type planet struct {
	branch string
	name   string
	issue  string
	tier   string
	title  string // free text: sanitizeText, not clean
	engine string
	model  string // short alias
	detail string // free text: sanitizeText, not clean
	state  string
	class  string
	failed bool
	host   string
	base   string
	ts     float64 // the row's last-activity ms
	moons  []moon

	repo      string // the bus's repo identity (its git common dir)
	repoLabel string
	home      bool // from the bus the renderer was run in
	prURL     string
	waiting   bool // blocked on a human answer
	stale     bool
	ci        string // "", "pass", "pending", "fail" or "none"
	rounds    int    // completed review-then-fix rounds
}

// queuedHold is one outstanding hold: a task waiting on an engine quota window
// (engine and window set) or on the fleet queue (both empty).
type queuedHold struct {
	id, label, engine, window string
}

// queuedTask is one outstanding queue entry: a task filed but not dispatched.
// after holds the raw branch or number targets it waits on; they are only
// compared with planets, never written into the diagram.
type queuedTask struct {
	id, label, repo string
	after           []string
}

// engineReading is one engine's most-used quota window; a nil pct is unknown,
// never zero.
type engineReading struct {
	name, window string
	pct          *float64
}

// legend is the count line: every drawn or folded worker by class, plus the
// outstanding holds, which have no planet. waiting, stale, ci and repos count
// drawn planets only.
type legend struct {
	working, blocked, prOpen, done, failed, holds, queued int
	waiting, stale, ci, repos                             int
	weight                                                int  // tier weight of this crew's live rows
	budget                                                *int // fleet budget; nil is unknown
	engines                                               []engineReading
}

// buildPlanets reads the jq model into typed planets and legend counts. now is
// wall seconds; a done or failed row older than doneFold is counted but not
// returned. A row without a branch or a state is skipped; a state crew status
// learns tomorrow draws as working, so a worker never vanishes over a new word.
func buildPlanets(m map[string]any, now float64) ([]planet, legend) {
	var lg legend
	moons := roleMoons(m["roles"])
	rounds, _ := m["rounds"].(map[string]any)
	holds, _ := m["holds"].([]any)
	lg.holds = len(holds)
	queued, _ := m["queued"].([]any)
	lg.queued = len(queued)

	rows, _ := m["rows"].([]any)
	var out []planet
	for _, r := range rows {
		row, _ := r.(map[string]any)
		state, _ := row["state"].(string)
		branch, _ := row["branch"].(string)
		if branch == "" || state == "" {
			continue
		}
		class, failed := classify(state)
		switch {
		case failed:
			lg.failed++
		case class == classWorking:
			lg.working++
		case class == classBlocked:
			lg.blocked++
		case class == classPROpen:
			lg.prOpen++
		default:
			lg.done++
		}
		ts := number(row["ts"])
		if class == classDone && now*1000-ts > float64(doneFold.Milliseconds()) {
			continue
		}

		name, _ := row["name"].(string)
		tier, _ := row["tier"].(string)
		base, _ := row["base"].(string)
		title, _ := row["title"].(string)
		engine, _ := row["engine"].(string)
		model, _ := row["model"].(string)
		detail, _ := row["detail"].(string)
		codename, suffix, _ := strings.Cut(name, "·")
		issue := issueOf(branch)
		if issue == "" && foldIDRe.MatchString(suffix) {
			issue = suffix
		}
		p := planet{
			branch: branch,
			name:   clean(codename),
			issue:  clean(issue),
			tier:   tier,
			title:  sanitizeText(title),
			engine: clean(engine),
			model:  clean(modelAlias(model)),
			detail: sanitizeText(detail),
			state:  state,
			class:  class,
			failed: failed,
			base:   base,
			ts:     ts,
		}
		if p.name == "" {
			p.name = "?"
		}
		if host, _ := row["host"].(string); host != "" {
			if p.host = clean(host); p.host == "" {
				p.host = "?"
			}
		}
		p.rounds = int(number(rounds[branch]))
		if class != classDone && class != classPROpen {
			p.moons = moons[branch]
		}
		if class == classPROpen {
			p.prURL, _ = row["pr_url"].(string)
		}
		p.waiting = waitingOn(class, row)
		p.stale = !p.waiting && (class == classWorking || class == classBlocked) &&
			ts > 0 && now*1000-ts > float64(staleStatus.Milliseconds())
		out = append(out, p)
	}
	tally(out, &lg)
	return out, lg
}

// buildHolds reads the model's outstanding holds. A quota hold names the engine
// window it waits on; a fleet hold waits on the queue and names neither.
func buildHolds(m map[string]any) []queuedHold {
	list, _ := m["holds"].([]any)
	var out []queuedHold
	for _, h := range list {
		row, _ := h.(map[string]any)
		task, _ := row["task"].(map[string]any)
		wait, _ := row["wait"].(map[string]any)
		id, _ := row["id"].(string)
		branch, _ := task["branch"].(string)
		ref, _ := task["ref"].(string)
		q := queuedHold{id: clean(id), label: clean(issueOf(branch))}
		if q.label == "" {
			q.label = clean(ref)
		}
		if q.label == "" {
			q.label = "hold"
		}
		if kind, _ := wait["kind"].(string); kind != "fleet" {
			engine, _ := wait["engine"].(string)
			window, _ := wait["window"].(string)
			q.engine, q.window = clean(engine), clean(window)
		}
		out = append(out, q)
	}
	return out
}

// maxAfter caps the targets one queued task draws edges for.
const maxAfter = 4

// buildQueued reads the model's queued tasks. The label is the branch's issue,
// else the ref, else "queued"; titles are never drawn.
func buildQueued(m map[string]any) []queuedTask {
	list, _ := m["queued"].([]any)
	var out []queuedTask
	for _, e := range list {
		row, _ := e.(map[string]any)
		task, _ := row["task"].(map[string]any)
		id, _ := row["id"].(string)
		branch, _ := task["branch"].(string)
		ref, _ := task["ref"].(string)
		q := queuedTask{id: clean(id), label: clean(issueOf(branch))}
		if q.label == "" {
			q.label = clean(ref)
		}
		if q.label == "" {
			q.label = "queued"
		}
		after, _ := row["after"].([]any)
		for _, a := range after {
			s, _ := a.(string)
			if s = strings.TrimSpace(s); s != "" && len(q.after) < maxAfter {
				q.after = append(q.after, s)
			}
		}
		out = append(out, q)
	}
	return out
}

// tierWeight is a tier's share of the fleet budget; an unknown tier weighs as
// standard.
func tierWeight(tier string) int {
	switch tier {
	case "trivial":
		return 1
	case "deep":
		return 3
	}
	return 2
}

// tally overwrites the drawn-planet counts of lg.
func tally(ps []planet, lg *legend) {
	lg.waiting, lg.stale, lg.ci = 0, 0, 0
	repos := map[string]bool{}
	for _, p := range ps {
		if p.waiting {
			lg.waiting++
		}
		if p.stale {
			lg.stale++
		}
		if p.ci != "" {
			lg.ci++
		}
		if p.repo != "" {
			repos[p.repo] = true
		}
	}
	lg.repos = len(repos)
}

// waitingOn reports a blocked worker parked on a human answer. A watchdog row
// is the watchdog's own report, not the worker asking.
func waitingOn(class string, row map[string]any) bool {
	if class != classBlocked {
		return false
	}
	if source, _ := row["source"].(string); source == "watchdog" {
		return false
	}
	detail, _ := row["detail"].(string)
	return strings.HasPrefix(detail, "permission:") || strings.HasPrefix(detail, "acceptance:")
}

// classify maps a row state to its sector class; failed marks the done-sector
// states that ended badly. Any other state is working.
func classify(state string) (class string, failed bool) {
	switch state {
	case "blocked":
		return classBlocked, false
	case "pr_open":
		return classPROpen, false
	case "done", "reaped":
		return classDone, false
	case "failed", "exited":
		return classDone, true
	}
	return classWorking, false
}

// roleMoons groups the model's role panes by branch, sorted by role name.
func roleMoons(roles any) map[string][]moon {
	list, _ := roles.([]any)
	out := map[string][]moon{}
	for _, r := range list {
		row, _ := r.(map[string]any)
		branch, _ := row["branch"].(string)
		role, _ := row["role"].(string)
		state, _ := row["state"].(string)
		out[branch] = append(out[branch], moon{role: role, working: state == "working"})
	}
	for _, ms := range out {
		slices.SortStableFunc(ms, func(a, b moon) int { return cmp.Compare(a.role, b.role) })
	}
	return out
}

// issueOf is the first digit run of the branch after an optional `type/` prefix.
func issueOf(branch string) string {
	if _, rest, ok := strings.Cut(branch, "/"); ok {
		branch = rest
	}
	return issueRe.FindString(branch)
}

// clean reduces bus- or tmux-sourced text to `[A-Za-z0-9._·-]` and cuts it to
// maxText runes, so no such text can carry DOT syntax past this file.
func clean(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxText {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '·':
			b.WriteRune(r)
			n++
		}
	}
	return b.String()
}

// modelAlias shortens a model id to its family word (opus, sonnet, haiku,
// fable) or, failing that, to the part after the last slash.
func modelAlias(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	lower := strings.ToLower(s)
	for _, f := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(lower, f) {
			return f
		}
	}
	return s
}

// sanitizeText reduces bus-sourced free text to printable runes with single
// spaces, cut to maxFreeText runes. DOT and HTML syntax stays in place:
// htmlText escapes it at emission.
func sanitizeText(s string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if n == maxFreeText {
			break
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// number reads a jq number the way liveCount does: json.Number or float64.
func number(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}
