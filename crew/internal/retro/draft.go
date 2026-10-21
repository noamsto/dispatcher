package retro

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/identity"
)

// The draft keeps the shape the dispatcher's own session_summary notes have, so
// what it prints is a line the dispatcher already knows how to post:
//
//	crew msg "dispatcher:$CREW_ID" "retro:$CREW_ID" "$(crew retro --draft --all)"
const (
	summarySeam = "dispatch"
	summaryTag  = "session_summary"
	// patternPrefix names the detail's last part: the tags no single worker's
	// line can show.
	patternPrefix = "tags on ≥2 workers: "
	// missing is what the fold prints for an engine, model or tier a row does not
	// carry.
	missing = "—"
)

// note is the --draft's body: one JSON object, its fields in the order the
// dispatcher's own notes use them.
type note struct {
	Seam   string `json:"seam"`
	Tag    string `json:"tag"`
	Detail string `json:"detail"`
}

// worker is one branch's line in the detail.
type worker struct {
	branch   string
	codename string // what `crew roster` prints, collision suffix and all
	name     string // the codename on its own, which is what a note names
	issue    string // the branch's issue number, the other way a note names it
	tier     string
	engine   string
	model    string
	outcome  string
	tags     []string
	newest   float64 // newest dispatch row, which orders the lines
}

// line is one worker's segment of the detail.
func (w worker) line() string {
	line := fmt.Sprintf("%s {%s,%s,%s} %s", w.codename, w.tier, w.engine, w.model, w.outcome)
	if len(w.tags) > 0 {
		line += " [" + strings.Join(w.tags, ",") + "]"
	}
	return line
}

// draft prints the crew's summary as one line, or prints nothing at all when no
// branch is in scope.
func draft(crew string, all bool, events []bus.Event, o Options, stdout, stderr io.Writer) int {
	docs := make([]any, 0, len(events))
	for _, ev := range events {
		docs = append(docs, ev.Doc)
	}

	// codenames is each branch's name as `crew roster` prints it. A dispatched
	// branch the roster never printed — no worker ever posted a status for it —
	// falls back to the identity roster itself reads names off.
	codenames := map[string]string{}
	for _, row := range o.Roster(docs, crew) {
		branch, _ := row["branch"].(string)
		if name, ok := row["name"].(string); ok && name != "" {
			codenames[branch] = name
		}
	}

	boundary := math.Inf(-1)
	if !all {
		boundary = summaryBoundary(events, crew)
	}

	recorded := identity.RecordedAll(docs)
	var workers []worker
	for _, w := range dispatched(events, crew) {
		if !all && w.newest <= boundary {
			continue
		}
		id := identity.For(recorded, w.branch)
		w.name = id.Name
		w.codename = codenames[w.branch]
		if w.codename == "" {
			w.codename = id.Name
		}
		w.issue = issueOf(w.branch)
		w.outcome = outcomeOf(events, w.branch)
		w.tags = workerTags(events, crew, w)
		workers = append(workers, w)
	}
	if len(workers) == 0 {
		return 0
	}
	sort.Slice(workers, func(i, j int) bool {
		if workers[i].newest != workers[j].newest {
			return workers[i].newest < workers[j].newest
		}
		return workers[i].branch < workers[j].branch
	})

	parts := make([]string, 0, len(workers)+1)
	for _, w := range workers {
		parts = append(parts, w.line())
	}
	if shared := sharedTags(workers); shared != "" {
		parts = append(parts, patternPrefix+shared)
	}

	// Never HTML-escaped: a model name comes back out of the note verbatim.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(note{Seam: summarySeam, Tag: summaryTag, Detail: strings.Join(parts, "; ")}); err != nil {
		say(stderr, "crew: retro: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", strings.TrimSuffix(buf.String(), "\n"))
	return 0
}

// summaryBoundary is the crew's newest session_summary note written by its own
// dispatcher: everything before it is already written up, so the draft starts
// after it.
func summaryBoundary(events []bus.Event, crew string) float64 {
	newest := math.Inf(-1)
	for _, ev := range events {
		if ev.Kind != bus.KindMsg || ev.From != "dispatcher:"+crew || ev.To != "retro:"+crew {
			continue
		}
		if tag, _ := noteOf(ev)["tag"].(string); tag != summaryTag {
			continue
		}
		if ts := tsOf(ev); ts > newest {
			newest = ts
		}
	}
	return newest
}

// dispatched is every branch with a dispatch row in the crew, carrying the
// {tier,engine,model} of its newest one.
func dispatched(events []bus.Event, crew string) []worker {
	by := map[string]*worker{}
	var order []string
	for _, ev := range events {
		if ev.Kind != bus.KindDispatch || ev.CrewID != crew || ev.Branch == "" {
			continue
		}
		w := by[ev.Branch]
		if w == nil {
			w = &worker{branch: ev.Branch, newest: math.Inf(-1), tier: missing, engine: missing, model: missing}
			by[ev.Branch] = w
			order = append(order, ev.Branch)
		}
		if ts := tsOf(ev); ts >= w.newest {
			w.newest = ts
			row := rowOf(ev)
			w.tier, w.engine, w.model = field(row, "tier"), field(row, "engine"), field(row, "model")
		}
	}
	out := make([]worker, 0, len(order))
	for _, b := range order {
		out = append(out, *by[b])
	}
	return out
}

// outcomeOf is the branch's own newest status, skipping the rows `crew reap` and
// the watchdog post: they rewrite a state the worker never chose. `PR #N` rides
// that same row's pr_url, and when the newest row carries none — a worker that
// posts pr_open and then a bare done — it rides that same session's latest
// pr_open row. A session id is worker id equality, so a resume never inherits
// the PR its predecessor opened.
func outcomeOf(events []bus.Event, branch string) string {
	newest, found := math.Inf(-1), false
	state, pr, ses := missing, "", ""
	// prOfSession is each session's latest pr_open row's PR; prTS is the ts
	// those latests were read at.
	prOfSession := map[string]string{}
	prTS := map[string]float64{}
	for _, ev := range events {
		if ev.Kind != bus.KindStatus || workerBranch(ev.From) != branch {
			continue
		}
		body := objOf(ev)
		if s, _ := body["state"].(string); s != "pr_open" {
			continue
		}
		if s, _ := body["source"].(string); s == "watchdog" {
			continue
		}
		p := prOf(body)
		if p == "" {
			continue
		}
		s := sessionOf(ev.From)
		if ts := tsOf(ev); ts >= prTS[s] {
			prTS[s], prOfSession[s] = ts, p
		}
	}
	for _, ev := range events {
		if ev.Kind != bus.KindStatus || workerBranch(ev.From) != branch {
			continue
		}
		body := objOf(ev)
		if s, _ := body["state"].(string); s == "reaped" {
			continue
		}
		if s, _ := body["source"].(string); s == "watchdog" {
			continue
		}
		ts := tsOf(ev)
		if found && ts < newest {
			continue
		}
		newest, found = ts, true
		state = field(body, "state")
		pr = prOf(body)
		ses = sessionOf(ev.From)
	}
	if !found {
		return missing
	}
	if pr == "" {
		pr = prOfSession[ses]
	}
	if pr == "" {
		return state
	}
	return state + " " + pr
}

// sessionOf is the session id of a worker id — `worker:<branch>#<session>` —
// which a resume mints fresh. An id with no session suffix reads as the empty
// session.
func sessionOf(from string) string {
	if i := strings.LastIndex(from, "#"); i >= 0 {
		return from[i+1:]
	}
	return ""
}

// workerTags are the distinct tags the branch's own retro notes carried, plus the
// tag of a dispatcher note in the crew that names the branch — by codename or by
// issue number — which is how a note the dispatcher wrote about a worker reaches
// that worker's line. session_summary is the summary itself and other is the
// catch-all, so neither names a pattern.
func workerTags(events []bus.Event, crew string, w worker) []string {
	var tags []string
	seen := map[string]bool{}
	for _, ev := range events {
		// A note belongs to the crew whose retro sink it was addressed to, and a
		// branch name can be shared between crews.
		if ev.Kind != bus.KindMsg || ev.To != "retro:"+crew {
			continue
		}
		body := noteOf(ev)
		tag, _ := body["tag"].(string)
		if tag == "" || tag == summaryTag || tag == "other" || seen[tag] {
			continue
		}
		detail, _ := body["detail"].(string)
		mine := workerBranch(ev.From) == w.branch
		named := strings.HasPrefix(ev.From, "dispatcher:") &&
			(mentions(detail, w.name) || mentions(detail, w.issue))
		if mine || named {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags
}

// sharedTags names the tags at least two of the workers carried, in
// first-appearance order: the pattern only the whole crew can show.
func sharedTags(workers []worker) string {
	count := map[string]int{}
	var order []string
	for _, w := range workers {
		for _, tag := range w.tags {
			if count[tag] == 0 {
				order = append(order, tag)
			}
			count[tag]++
		}
	}
	var shared []string
	for _, tag := range order {
		if count[tag] >= 2 {
			shared = append(shared, tag)
		}
	}
	return strings.Join(shared, ",")
}

// field is one row's text field, or the em-dash the fold prints for an absent one.
func field(fields map[string]any, key string) string {
	if s, ok := fields[key].(string); ok && s != "" {
		return s
	}
	return missing
}

// prOf is "PR #N" for the pull a status row carried, "" when it carried none.
func prOf(body map[string]any) string {
	url, _ := body["pr_url"].(string)
	if m := pullRe.FindStringSubmatch(url); m != nil {
		return "PR #" + m[1]
	}
	return ""
}

var pullRe = regexp.MustCompile(`/pull/([0-9]+)`)

// issueOf is the issue number a branch is named for — the same capture roster
// takes for a colliding codename's suffix.
func issueOf(branch string) string {
	if m := issueRe.FindStringSubmatch(branch); m != nil {
		return m[1]
	}
	return ""
}

var issueRe = regexp.MustCompile(`^(?:[a-z]+/)?([0-9]+)`)

// workerBranch is the branch a worker-posted row came from, `worker:<branch>#<session>`.
// A role's rows, and every other sender, read as no branch at all.
func workerBranch(from string) string {
	s, ok := strings.CutPrefix(from, "worker:")
	if !ok {
		return ""
	}
	if i := strings.LastIndex(s, "#"); i >= 0 {
		return s[:i]
	}
	return s
}

// mentions is a whole-token read: the "1186" inside "#1186" names branch 1186 and
// does not name 11860.
func mentions(detail, token string) bool {
	if token == "" {
		return false
	}
	for at := 0; at+len(token) <= len(detail); {
		i := strings.Index(detail[at:], token)
		if i < 0 {
			return false
		}
		i += at
		end := i + len(token)
		left := i == 0 || !isWordByte(detail[i-1])
		right := end == len(detail) || !isWordByte(detail[end])
		if left && right {
			return true
		}
		at = i + 1
	}
	return false
}

// isWordByte is an ASCII letter or digit; a codename's "·" is not one.
func isWordByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// tsOf is an event's millisecond stamp. A row without a numeric one is older than
// every row: it clears no boundary and never wins a newest.
func tsOf(ev bus.Event) float64 {
	v, ok := docField(ev, "ts")
	if !ok {
		return math.Inf(-1)
	}
	num, ok := v.(json.Number)
	if !ok {
		return math.Inf(-1)
	}
	f, err := num.Float64()
	if err != nil {
		return math.Inf(-1)
	}
	return f
}

// objOf is an event's body when it is an object, as a status row's is.
func objOf(ev bus.Event) map[string]any {
	body, _ := docField(ev, "body")
	obj, _ := body.(map[string]any)
	return obj
}

// rowOf is an event's whole row: a dispatch row carries tier, engine and model
// beside its kind, not in a body.
func rowOf(ev bus.Event) map[string]any {
	row, _ := ev.Doc.(map[string]any)
	return row
}

// docField is one top-level field of an event's row.
func docField(ev bus.Event, key string) (any, bool) {
	obj, ok := ev.Doc.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := obj[key]
	return v, ok
}

// noteOf is a msg row's body: the JSON string `crew msg` appends, read back. A
// row whose body is not an object, or not JSON at all, is no note.
func noteOf(ev bus.Event) map[string]any {
	s, ok := docField(ev, "body")
	if !ok {
		return nil
	}
	str, ok := s.(string)
	if !ok || str == "" {
		return nil
	}
	var body map[string]any
	if json.Unmarshal([]byte(str), &body) != nil {
		return nil
	}
	return body
}
