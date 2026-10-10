package status

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/ledger"
)

// gate is the arm's pr_open/done gate for a worker session.
type gate struct {
	crew, from, state, detail string
	paths                     bus.Paths
	o                         Options
}

// taskDoc is the worker's WORKER_TASK.md as the gate reads it.
type taskDoc struct {
	text               string
	unreadable         bool
	tier, kind, engine string
}

// check returns the refusal line, or "" when the row may be posted. Seams from
// any earlier session on this branch in this crew count (the resume case), and
// a worker that cannot review stops on an ungated state (blocked/failed), so
// refusing here never strands one.
func (g gate) check() string {
	if g.state != "pr_open" && g.state != "done" || !strings.HasPrefix(g.from, "worker:") {
		return ""
	}
	top := g.o.Toplevel()
	if top == "" {
		return ""
	}
	path := top + "/WORKER_TASK.md"
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	doc := readTaskDoc(path)
	kind := doc.kind
	if kind == "" {
		kind = "implement"
	}
	if kind != "implement" {
		return ""
	}

	log := readLog(g.paths.Log)
	if g.state == "pr_open" {
		in := ledger.Input{From: g.from, Crew: g.crew, Detail: g.detail, TaskDoc: doc.text, TaskDocErr: doc.unreadable}
		if log.exists {
			in.Log = log.lines
			if in.Log == nil {
				in.Log = []string{}
			}
		}
		if line := ledger.Check(in); line != "" {
			return line
		}
	}
	if doc.tier != "standard" && doc.tier != "deep" {
		return ""
	}

	// The branch session id: the session suffix `#s…` cut at its last occurrence.
	b := g.from
	if i := strings.LastIndex(b, "#s"); i >= 0 {
		b = b[:i]
	}
	vars := map[string]jsonv.Value{
		"c": jsonv.Str(g.crew),
		"b": jsonv.Str(b),
		"r": jsonv.Str("role:" + strings.TrimPrefix(b, "worker:") + ":reviewer"),
		"e": jsonv.Str(doc.engine),
	}
	if line, ok := g.seam("review", seamProg, log, vars); !ok {
		if line != "" {
			return line
		}
		return fmt.Sprintf(`crew: refusing %s for %s session %s — no review seam on the bus for this branch; run the code review gate, ingest its verdict, then crew msg "$CREW_WORKER_ID" "review:%s" '{"seam":"review","review_mode":"full"}' (or downgraded) and retry; a review request or a pane that has not returned a verdict is not a review; a review that cannot run goes blocked/failed, never pr_open/done. On pi the reviewer pane's latest verdict decides: accept passes, revise needs your own review:%s seam after you fix it, a reject (or any reply that is not an exact accept/revise) blocks until the reviewer's next verdict, and a re-request (any msg from you to the reviewer except the {"final":true} release) cancels every earlier verdict and your own earlier seam until a new verdict arrives`,
			g.state, doc.tier, g.from, g.crew, g.crew)
	}
	if line, ok := g.seam("deslop", deslopProg, log, vars); !ok {
		if line != "" {
			return line
		}
		return fmt.Sprintf(`crew: refusing %s for %s session %s — no deslop seam on the bus for this branch; run the harness deslop skill (dispatcher:deslop on claude, $deslop on codex, deslop on cursor and pi) over the diff you are about to push, commit its cleanup, then crew msg "$CREW_WORKER_ID" "review:%s" '{"seam":"deslop"}' and retry. The seam records that the skill ran — never post it just to get past this gate`,
			g.state, doc.tier, g.from, g.crew)
	}
	return ""
}

// seam runs one fold over the log's lines. ok is true when the fold found the
// seam; otherwise line is the "could not read the crew log" refusal, or "" for a
// plain miss (no log file counts as one).
func (g gate) seam(name, prog string, log logFile, vars map[string]jsonv.Value) (line string, ok bool) {
	if !log.exists {
		return "", false
	}
	unreadable := func(code int) string {
		return fmt.Sprintf("crew: refusing %s for %s — could not read the crew log for the %s seam (jq exit %d)", g.state, g.from, name, code)
	}
	if log.err != nil {
		return unreadable(jqUnreadable), false
	}
	rows := make([]jsonv.Value, len(log.lines))
	for i, l := range log.lines {
		rows[i] = jsonv.Str(l)
	}
	v, err := g.o.Fold(prog, rows, vars)
	if err != nil {
		return unreadable(jqRuntime), false
	}
	f, isNum := v.AsFloat()
	return "", isNum && f == 1
}

// logFile is the bus log as `jq -R` reads it: lines, a final unterminated one
// included. exists is `[ -f "$log" ]`; err is a regular file that will not open.
type logFile struct {
	exists bool
	lines  []string
	err    error
}

func readLog(path string) logFile {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return logFile{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return logFile{exists: true, err: err}
	}
	if len(data) == 0 {
		return logFile{exists: true}
	}
	return logFile{exists: true, lines: strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")}
}

var (
	tierRe   = regexp.MustCompile(`^tier:(.*)`)
	kindRe   = regexp.MustCompile(`^kind:(.*)`)
	engineRe = regexp.MustCompile(`^engine:(.*)`)
	spaceRe  = regexp.MustCompile(`[ \t\n\v\f\r]+`)
)

// readTaskDoc is the doc plus its three header fields, each the first line
// anywhere that starts `field:`, with all whitespace removed
// (`sed -n 's/^f:[[:space:]]*//p' | head -1 | tr -d '[:space:]'`). An
// unreadable doc leaves every field empty.
func readTaskDoc(path string) taskDoc {
	data, err := os.ReadFile(path)
	if err != nil {
		return taskDoc{unreadable: true}
	}
	d := taskDoc{text: string(data)}
	field := func(re *regexp.Regexp) string {
		for line := range strings.SplitSeq(d.text, "\n") {
			if m := re.FindStringSubmatch(line); m != nil {
				return spaceRe.ReplaceAllString(m[1], "")
			}
		}
		return ""
	}
	d.tier, d.kind, d.engine = field(tierRe), field(kindRe), field(engineRe)
	return d
}
