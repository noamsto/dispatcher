package rosterrender

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
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

// maxText bounds every bus- or tmux-sourced string that reaches the diagram.
const maxText = 24

// sectorAnchor is each class's home angle in degrees clockwise from 12 o'clock.
var sectorAnchor = map[string]float64{classBlocked: 0, classWorking: 90, classPROpen: 180, classDone: 270}

// tierSize is a planet's diameter in inches; an unknown tier draws as standard.
var tierSize = map[string]float64{"deep": 0.9, "standard": 0.75, "trivial": 0.6}

const standardSize = 0.75

var issueRe = regexp.MustCompile(`[0-9]+`)

// moon is one role pane of a drawn worker. x and y are set by the layout.
type moon struct {
	role    string
	working bool
	x, y    float64
}

// planet is one drawn worker. Every free-text field has been through clean.
type planet struct {
	branch string
	name   string
	issue  string
	label  string // name, then "\n" and the issue when the branch has one
	tier   string
	state  string
	class  string
	failed bool
	host   string
	base   string
	size   float64
	moons  []moon
}

// legend is the count line: every drawn or folded worker by class, plus the
// outstanding holds, which have no planet.
type legend struct {
	working, blocked, prOpen, done, failed, holds int
}

// buildPlanets reads the jq model into typed planets and legend counts. now is
// wall seconds; a done or failed row older than doneFold is counted but not
// returned. A row without a branch or a state is skipped; a state crew status
// learns tomorrow draws as working, so a worker never vanishes over a new word.
func buildPlanets(m map[string]any, now float64) ([]planet, legend) {
	var lg legend
	moons := roleMoons(m["roles"])
	holds, _ := m["holds"].([]any)
	lg.holds = len(holds)

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
		if class == classDone && now*1000-number(row["ts"]) > float64(doneFold.Milliseconds()) {
			continue
		}

		name, _ := row["name"].(string)
		tier, _ := row["tier"].(string)
		base, _ := row["base"].(string)
		p := planet{
			branch: branch,
			name:   clean(name),
			issue:  clean(issueOf(branch)),
			tier:   tier,
			state:  state,
			class:  class,
			failed: failed,
			base:   base,
			size:   standardSize,
		}
		if p.name == "" {
			p.name = "?"
		}
		if s, ok := tierSize[tier]; ok {
			p.size = s
		}
		p.label = p.name
		if p.issue != "" {
			p.label += "\n" + p.issue
		}
		if host, _ := row["host"].(string); host != "" {
			if p.host = clean(host); p.host == "" {
				p.host = "?"
			}
		}
		if class != classDone && class != classPROpen {
			p.moons = moons[branch]
		}
		out = append(out, p)
	}
	return out, lg
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

// number reads a jq number the way liveCount does: json.Number or float64.
func number(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return 0
}
