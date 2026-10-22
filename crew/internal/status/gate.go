package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/ledger"
)

// jq's statuses for a fold that cannot run: 2 unreadable input, 5 a runtime error.
const (
	jqUnreadable = 2
	jqRuntime    = 5
)

// errLogUnreadable is foldSeam's report that the bus log would not read; its
// callers refuse quoting jq's unreadable-input status, not its runtime one.
var errLogUnreadable = errors.New("crew log unreadable")

// planHint is the body of both plan-seam refusals: the pr_open/done gate's and
// `crew msg`'s review-seam refusal. %s is the crew id.
const planHint = `plan: required but no plan seam on the bus for this branch; run the plan critic (spec-plan-critic, or the plan-critic role pane), ingest its first verdict, then crew msg "$CREW_WORKER_ID" "review:%s" '{"seam":"plan","plan_critic_first_pass":"accept"}' (or revise/reject: the critic's first verdict) and retry. A plan phase you skipped or could not run is reported, not papered over: post crew status blocked naming why and await the dispatcher (it can re-dispatch with plan: provided); never post the seam without a critic verdict`

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
	plan, resume       string
}

// check returns the refusal line, or "" when the row may be posted. Seams are
// checked review, deslop, then (`plan: required`) plan. Seams from any earlier
// session on this branch in this crew count (the resume case; the plan seam
// only since the latest dispatch). A worker that cannot review stops on an
// ungated state (blocked/failed), so refusing here never strands one.
func (g gate) check(ctx context.Context) string {
	if g.state != "pr_open" && g.state != "done" || !strings.HasPrefix(g.from, "worker:") {
		return ""
	}
	doc, ok := g.o.taskDoc(ctx)
	if !ok {
		return ""
	}
	if doc.kind != "" && doc.kind != "implement" {
		return ""
	}

	log := readLog(g.paths.Log)
	if g.state == "pr_open" {
		in := ledger.Input{From: g.from, Crew: g.crew, Detail: g.detail, TaskDoc: doc.text, TaskDocErr: doc.unreadable, Log: log.lines}
		if line := ledger.Check(in); line != "" {
			return line
		}
	}
	if doc.tier != "standard" && doc.tier != "deep" {
		return ""
	}

	vars := seamVars(g.crew, g.from, doc)
	type seam struct {
		name, prog, refusal string
	}
	seams := []seam{
		{"review", seamProg, fmt.Sprintf(`crew: refusing %s for %s session %s — no review seam on the bus for this branch; run the code review gate, ingest its verdict, then crew msg "$CREW_WORKER_ID" "review:%s" '{"seam":"review","review_mode":"full"}' (or downgraded) and retry; a review request or a pane that has not returned a verdict is not a review; a review that cannot run goes blocked/failed, never pr_open/done. On pi the reviewer pane's latest verdict decides: accept passes, revise needs your own review:%s seam after you fix it, a reject (or any reply that is not an exact accept/revise) blocks until the reviewer's next verdict, and a re-request (any msg from you to the reviewer except the {"final":true} release) cancels every earlier verdict and your own earlier seam until a new verdict arrives`,
			g.state, doc.tier, g.from, g.crew, g.crew)},
		{"deslop", deslopProg, fmt.Sprintf(`crew: refusing %s for %s session %s — no deslop seam on the bus for this branch; run the harness deslop skill (dispatcher:deslop on claude, $deslop on codex, deslop on cursor and pi) over the diff you are about to push, commit its cleanup, then crew msg "$CREW_WORKER_ID" "review:%s" '{"seam":"deslop"}' and retry. The seam records that the skill ran — never post it just to get past this gate`,
			g.state, doc.tier, g.from, g.crew)},
	}
	if planGated(doc) {
		seams = append(seams, seam{"plan", planProg, fmt.Sprintf("crew: refusing %s for %s session %s — %s",
			g.state, doc.tier, g.from, fmt.Sprintf(planHint, g.crew))})
	}
	unreadable := func(name string, code int) string {
		return fmt.Sprintf("crew: refusing %s for %s — could not read the crew log for the %s seam (jq exit %d)", g.state, g.from, name, code)
	}
	for _, s := range seams {
		posted, err := foldSeam(g.o, s.prog, log, vars)
		switch {
		case errors.Is(err, errLogUnreadable):
			return unreadable(s.name, jqUnreadable)
		case err != nil:
			return unreadable(s.name, jqRuntime)
		case !posted:
			return s.refusal
		}
	}
	return ""
}

// foldSeam runs one seam fold over the bus log: true when the log holds the
// seam. A log that will not read reports errLogUnreadable, and no log file at
// all holds no seam; both fail closed.
func foldSeam(o Options, prog string, log logFile, vars map[string]any) (bool, error) {
	if log.err != nil {
		return false, errLogUnreadable
	}
	if log.lines == nil {
		return false, nil
	}
	v, err := o.Fold(prog, log.lines, vars)
	if err != nil {
		return false, err
	}
	// RunAny hands an integer output back as json.Number.
	return v == json.Number("1"), nil
}

// planGated reports whether a session reading this task doc is gated on a plan
// seam: a standard or deep implement session whose doc says `plan: required`
// and that is not a resume of the branch.
func planGated(doc taskDoc) bool {
	if doc.kind != "" && doc.kind != "implement" {
		return false
	}
	if doc.tier != "standard" && doc.tier != "deep" {
		return false
	}
	return doc.plan == "required" && doc.resume != "true"
}

// seamVars are the seam folds' vars: $c crew, $b the branch session id, $r the
// reviewer role id, $e the task doc's engine.
func seamVars(crew, from string, doc taskDoc) map[string]any {
	// The branch session id: the session suffix `#s…` cut at its last occurrence.
	b := branchSession(from)
	return map[string]any{"c": crew, "b": b, "r": "role:" + strings.TrimPrefix(b, "worker:") + ":reviewer", "e": doc.engine}
}

func branchSession(from string) string {
	if i := strings.LastIndex(from, "#s"); i >= 0 {
		return from[:i]
	}
	return from
}

// taskDoc is the checkout's WORKER_TASK.md as both gate arms read it: through
// the git toplevel, or ok=false when there is no checkout or no regular doc
// there. An unreadable regular file is still read, its fields empty.
func (o Options) taskDoc(ctx context.Context) (taskDoc, bool) {
	top := git.Output(ctx, o.Git, "", "-C", o.Dir, "rev-parse", "--show-toplevel")
	if top == "" {
		return taskDoc{}, false
	}
	path := top + "/WORKER_TASK.md"
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return taskDoc{}, false
	}
	return readTaskDoc(path), true
}

// logFile is the bus log as `jq -R` reads it: lines, a final unterminated one
// included; nil lines = no log file; err = a regular file that will not read.
type logFile struct {
	lines []string
	err   error
}

func readLog(path string) logFile {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return logFile{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return logFile{lines: []string{}, err: err}
	}
	if len(data) == 0 {
		return logFile{lines: []string{}}
	}
	return logFile{lines: strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")}
}

var spaceRe = regexp.MustCompile(`[ \t\n\v\f\r]+`)

// readTaskDoc is the doc plus its five header fields, each the first line
// anywhere that starts `field:` (`sed -n 's/^f:[[:space:]]*//p' | head -1 |
// tr -d '[:space:]'`): sed strips leading glibc spaces, multibyte ones
// included, then tr deletes every ASCII one. An unreadable doc leaves every
// field empty.
func readTaskDoc(path string) taskDoc {
	data, err := os.ReadFile(path)
	if err != nil {
		return taskDoc{unreadable: true}
	}
	d := taskDoc{text: string(data)}
	field := func(name string) string {
		for line := range strings.SplitSeq(d.text, "\n") {
			if v, ok := strings.CutPrefix(line, name+":"); ok {
				return spaceRe.ReplaceAllString(strings.TrimLeftFunc(v, ledger.GlibcSpace), "")
			}
		}
		return ""
	}
	d.tier, d.kind, d.engine = field("tier"), field("kind"), field("engine")
	d.plan, d.resume = field("plan"), field("resume")
	return d
}
