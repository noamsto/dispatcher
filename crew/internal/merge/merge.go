// Package merge is `crew merge <N>`: a squash-merge that first proves the PR's
// head is green, the base is not red, and the worker's acceptance ledger still
// validates, and no directive of the dispatcher's is still undelivered to the
// worker — so a dispatcher never merges on a worker's report alone.
package merge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/ledger"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/policy"
	"github.com/noamsto/dispatcher/crew/internal/reply"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
	"github.com/noamsto/dispatcher/crew/internal/unread"
)

// Usage is the one-line signature.
const Usage = "usage: crew merge <N> [--dry-run] [--allow-no-ci] [--no-ledger] [--crew ID] [--policy NAME]"

const exitFailure = 1

// Options is everything Run reads beyond the bus.
type Options struct {
	GH  gh.Client
	Git git.Client
	// Dir is where git runs, so the worktree list resolves the same repo.
	Dir string
	// Reply posts the release to the merged PR's worker.
	Reply reply.Options
	// Policy loads the standing policy --policy names; nil refuses every name.
	Policy func(name string) (policy.Record, error)
}

// Request is the parsed command line.
type Request struct {
	PR                          string
	DryRun, AllowNoCI, NoLedger bool
	Crew                        string
	Policy                      string
}

type pr struct {
	State       string  `json:"state"`
	IsDraft     bool    `json:"isDraft"`
	Mergeable   string  `json:"mergeable"`
	HeadRefOid  string  `json:"headRefOid"`
	BaseRefName string  `json:"baseRefName"`
	URL         string  `json:"url"`
	Rollup      []check `json:"statusCheckRollup"`
}

// check is a CheckRun (name, status, conclusion) or a StatusContext (context,
// state) from the rollup.
type check struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func (c check) label() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// result is the check's outcome: pending, or the conclusion in upper case.
func (c check) result() (outcome string, pending bool) {
	if c.Typename == "StatusContext" || c.Context != "" {
		s := strings.ToUpper(c.State)
		return s, s == "PENDING" || s == "EXPECTED" || s == ""
	}
	if !strings.EqualFold(c.Status, "COMPLETED") {
		return "", true
	}
	return strings.ToUpper(c.Conclusion), false
}

// redConclusions are the completed conclusions that make a base red; a
// cancelled run is usually a superseded one.
var redConclusions = map[string]bool{"failure": true, "timed_out": true, "startup_failure": true, "action_required": true}

type statusRow struct {
	CrewID string `json:"crew_id"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Body   struct {
		State  string `json:"state"`
		Detail string `json:"detail"`
		PRURL  string `json:"pr_url"`
	} `json:"body"`
}

// Run is the command: the checks in order, the first failure refusing.
func Run(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	n, err := strconv.Atoi(req.PR)
	if err != nil || n <= 0 {
		say(stderr, "crew: merge: %s\n", Usage)
		return exitFailure
	}
	// First, so a refused session never reaches GitHub, --dry-run included.
	if reason := sender.Check(ctx, "dispatcher:"+req.Crew, req.Crew, o.Reply.Sender); reason != "" {
		say(stderr, "crew: merge: refusing sender 'dispatcher:%s' — %s\n", req.Crew, reason)
		return exitFailure
	}
	refuse := func(format string, args ...any) int {
		say(stderr, "crew: merge: refusing PR %d — %s\n", n, fmt.Sprintf(format, args...))
		return exitFailure
	}
	log := paths.Log
	if req.Crew != "" {
		c, err := paths.Crew(req.Crew)
		if err != nil {
			say(stderr, "crew: merge: invalid crew id %s\n", req.Crew)
			return exitFailure
		}
		log = c.Log
	}
	var provenance string
	if req.Policy != "" {
		if req.NoLedger || req.AllowNoCI {
			return refuse("--policy never waives a gate: drop --no-ledger/--allow-no-ci, or merge on the human's own approval")
		}
		rec, err := loadPolicy(req.Policy, o)
		if err != nil {
			return refuse("standing policy '%s' is not in force: %s", plain(req.Policy), plain(err.Error()))
		}
		provenance = fmt.Sprintf("'%s' set by %s@%s at %s: \"%s\"", plain(rec.Name), plain(rec.By), plain(rec.Host), plain(rec.At), plain(rec.Text))
	}
	var report []string
	var opened *statusRow
	pass := func(format string, args ...any) { report = append(report, fmt.Sprintf(format, args...)) }
	if provenance != "" {
		pass("policy: %s", provenance)
	}

	out, err := o.GH.Run(ctx, "pr", "view", req.PR, "--json", "state,isDraft,mergeable,headRefOid,baseRefName,url,statusCheckRollup")
	if err != nil {
		return refuse("gh pr view failed: %v", err)
	}
	var p pr
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return refuse("unreadable gh pr view output: %v", err)
	}
	switch {
	case p.State != "OPEN":
		return refuse("PR is %s, not OPEN; nothing to merge", strings.ToLower(p.State))
	case p.IsDraft:
		return refuse("PR is a draft; mark it ready for review first")
	case p.Mergeable == "UNKNOWN":
		return refuse("GitHub is still computing mergeability; retry")
	case p.Mergeable != "MERGEABLE":
		return refuse("PR is %s; have the worker rebase and resolve conflicts", strings.ToLower(p.Mergeable))
	}
	pass("pr: open, ready, mergeable")

	if len(p.Rollup) == 0 && !req.AllowNoCI {
		return refuse("head has no checks; pass --allow-no-ci if this repo has no CI")
	}
	for _, c := range p.Rollup {
		outcome, pending := c.result()
		switch {
		case pending:
			return refuse("checks still running (%s); retry when they finish", c.label())
		case outcome != "SUCCESS" && outcome != "NEUTRAL" && outcome != "SKIPPED":
			return refuse("head check %s is %s; fix it or wait for a green re-run", c.label(), strings.ToLower(outcome))
		}
		pass("head check %s: %s", c.label(), strings.ToLower(outcome))
	}
	if len(p.Rollup) == 0 {
		pass("head checks: none (--allow-no-ci)")
	}

	out, err = o.GH.Run(ctx, "api", "--paginate", "repos/{owner}/{repo}/commits/"+p.BaseRefName+"/check-runs?per_page=100",
		"--jq", `.check_runs[] | [.name, .status, .conclusion, .details_url] | @tsv`)
	if err != nil {
		return refuse("could not read %s's check runs: %v", p.BaseRefName, err)
	}
	var ignored int
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			return refuse("unreadable check runs of %s: %q", p.BaseRefName, line)
		}
		if !strings.EqualFold(f[1], "completed") || !redConclusions[strings.ToLower(f[2])] {
			continue
		}
		// Only `failure` is classified: a workflow file that fails to start never
		// gets a runner either, and that base really is broken.
		if strings.EqualFold(f[2], "failure") && neverStarted(ctx, o.GH, f[3]) {
			ignored++
			continue
		}
		return refuse("base %s is red (%s: %s); fix the base before merging", p.BaseRefName, f[0], f[2])
	}
	if ignored > 0 {
		suffix := "s"
		if ignored == 1 {
			suffix = ""
		}
		pass("base %s: not red (%d check run%s never started — ignored)", p.BaseRefName, ignored, suffix)
	} else {
		pass("base %s: not red", p.BaseRefName)
	}

	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		return refuse("could not read the bus: %v", err)
	}
	var lines []string
	if len(data) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	events := bus.EventsFromLines(lines)

	if req.NoLedger {
		pass("ledger: skipped (--no-ledger)")
	} else {
		var code int
		if opened, code = checkLedger(ctx, req, p.URL, lines, paths, o, refuse, pass); code != 0 {
			return code
		}
	}

	if code := checkUnread(lines, events, req, p.URL, paths, refuse, pass); code != 0 {
		return code
	}

	if req.DryRun {
		say(stdout, "ok: all checks passed\n")
		for _, l := range report {
			say(stdout, "  %s\n", l)
		}
		return 0
	}
	merged, err := o.GH.Combined(ctx, "pr", "merge", req.PR, "--squash", "--match-head-commit", p.HeadRefOid)
	if merged != "" {
		say(stdout, "%s\n", merged)
	}
	if err != nil {
		return refuse("gh pr merge failed: %v", err)
	}
	if provenance != "" {
		say(stdout, "merged under standing policy %s\n", provenance)
	}
	if opened != nil {
		release(ctx, n, req.Policy, *opened, paths, stdout, stderr, o)
	}
	return 0
}

// loadPolicy reads the named policy through the injected loader, so a missing
// or refused one is never taken for in force.
func loadPolicy(name string, o Options) (policy.Record, error) {
	if o.Policy == nil {
		return policy.Record{}, errors.New("no policy loader")
	}
	rec, err := o.Policy(name)
	if errors.Is(err, policy.ErrMissing) {
		return policy.Record{}, fmt.Errorf("no standing policy '%s' in this repo", name)
	}
	return rec, err
}

// neverStarted reports whether the Actions job behind a failed base check run
// never reached a runner, which makes its failure no evidence about the base.
// A check run that is not an Actions job, and any lookup that is not a clean
// answer, counts as started: the base is cleared only on a real answer.
func neverStarted(ctx context.Context, c gh.Client, detailsURL string) bool {
	id := actionsJobID(detailsURL)
	if id == "" {
		return false
	}
	out, err := c.Run(ctx, "api", "repos/{owner}/{repo}/actions/jobs/"+id,
		"--jq", `[.runner_name, (.steps | length)] | @tsv`)
	if err != nil {
		return false
	}
	// jq's @tsv leaves an empty first field for a null or empty runner_name.
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	return len(f) == 2 && f[0] == "" && f[1] == "0"
}

// actionsJobID is the Actions job a check run belongs to, from its details_url
// (…/actions/runs/<run>/job/<job>). A check run posted by another app points
// elsewhere and reports "".
func actionsJobID(detailsURL string) string {
	i := strings.LastIndex(detailsURL, "/job/")
	if i < 0 || !strings.Contains(detailsURL[:i], "/actions/runs/") {
		return ""
	}
	id := detailsURL[i+len("/job/"):]
	if j := strings.IndexByte(id, '/'); j >= 0 {
		id = id[:j]
	}
	if id == "" || strings.ContainsFunc(id, func(r rune) bool { return r < '0' || r > '9' }) {
		return ""
	}
	return id
}

// release tells a worker still holding at pr_open that its PR merged, so it
// posts done instead of holding its slot for the dispatcher. The merge already
// happened, so a failed reply only warns.
func release(ctx context.Context, n int, policyName string, opened statusRow, paths bus.Paths, stdout, stderr io.Writer, o Options) {
	log := paths.Log
	if c, err := paths.Crew(opened.CrewID); err == nil {
		log = c.Log
	}
	data, err := os.ReadFile(log)
	if err != nil {
		say(stderr, "crew: merge: could not release %s: %v\n", opened.From, err)
		return
	}
	var newest string
	for line := range strings.SplitSeq(string(data), "\n") {
		var r statusRow
		if json.Unmarshal([]byte(line), &r) == nil && r.Kind == "status" && r.From == opened.From {
			newest = r.Body.State
		}
	}
	if newest != "pr_open" {
		return
	}
	body := fmt.Sprintf("approved — PR #%d is merged; post done", n)
	if policyName != "" {
		body = fmt.Sprintf("approved — PR #%d is merged under standing policy '%s'; post done", n, policyName)
	}
	var rerr bytes.Buffer
	if reply.Run(ctx, reply.Request{Crew: opened.CrewID, To: opened.From, Body: body}, paths, &rerr, o.Reply) != 0 {
		say(stderr, "crew: merge: PR merged, but could not release %s: %s\n", opened.From, strings.TrimSpace(rerr.String()))
		return
	}
	say(stdout, "released %s\n", opened.From)
}

// newestPROpen is the newest pr_open status row for the PR at url, from this
// crew when crew is set; nil when there is none. Each raw line is decoded on its
// own, so a corrupt line is skipped and does not hide the rows after it.
func newestPROpen(lines []string, url, crew string) *statusRow {
	var found *statusRow
	for _, line := range lines {
		var r statusRow
		if json.Unmarshal([]byte(line), &r) != nil || r.Kind != "status" || r.Body.State != "pr_open" || r.Body.PRURL != url {
			continue
		}
		if crew != "" && r.CrewID != crew {
			continue
		}
		found = &r
	}
	return found
}

// checkUnread refuses while a dispatcher msg to the PR worker's newest session
// is undelivered: the worker would be merged out from under a follow-up it never
// read. Merge only reads the marks; delivery records them.
func checkUnread(lines []string, events []bus.Event, req Request, url string, paths bus.Paths, refuse func(string, ...any) int, pass func(string, ...any)) int {
	row := newestPROpen(lines, url, req.Crew)
	if row == nil {
		pass("unread: skipped (no worker pr_open for this PR)")
		return 0
	}
	branch := strings.TrimPrefix(row.From, "worker:")
	if i := strings.LastIndex(branch, "#s"); i >= 0 {
		branch = branch[:i]
	}
	docs := make([]any, len(events))
	for i, ev := range events {
		docs[i] = ev.Doc
	}
	// Every crew: a re-dispatch of the branch may carry another crew id.
	folded, err := sessions.Fold(docs, branch, "", float64(time.Now().Unix()))
	if err != nil {
		return refuse("could not fold the sessions of %s: %s", plain(branch), plain(err.Error()))
	}
	// The pr_open session may have been replaced since; its successor is who a
	// directive now reaches.
	rows, _ := folded.([]any)
	var newest map[string]any
	if len(rows) > 0 {
		newest, _ = rows[len(rows)-1].(map[string]any)
	}
	session, _ := newest["worker_id"].(string)
	if session == "" || !bus.IsSessionID(session) {
		return refuse("the newest session of %s (%s) has no session id, so no msg can be checked for it; re-dispatch the branch (dispatch --pr %s) to clear this", plain(branch), plain(session), req.PR)
	}
	var undelivered []map[string]any
	for _, crew := range sessionCrews(events, session, row.CrewID) {
		state := paths.Dir
		if bc, err := paths.Crew(crew); err == nil {
			state = bc.State
		}
		found, err := unread.Directives(events, crew, session, marks.Read(state, crew, session))
		if err != nil {
			return refuse("could not check %s for undelivered msgs: %s", plain(session), plain(err.Error()))
		}
		undelivered = append(undelivered, found...)
	}
	if len(undelivered) == 0 {
		pass("unread: none for %s", plain(session))
		return 0
	}
	list := make([]string, len(undelivered))
	for i, m := range undelivered {
		ts, _ := json.Marshal(m["ts"])
		list[i] = fmt.Sprintf("ts %s %s", plain(string(ts)), plain(fmt.Sprintf("%q", excerpt(m["body"]))))
	}
	if terminal, _ := newest["terminal"].(bool); terminal {
		state, _ := newest["state"].(string)
		return refuse("dispatcher msg(s) to %s not yet delivered: %s; the session ended (%s) and a stopped session never reads its inbox; re-dispatch the branch (dispatch --pr %s)", plain(session), strings.Join(list, "; "), plain(state), req.PR)
	}
	return refuse("dispatcher msg(s) to %s not yet delivered: %s; let the worker read it (crew nudge %s), or re-dispatch the branch if its session ended", plain(session), strings.Join(list, "; "), plain(branch))
}

// sessionCrews are the crews a directive to session may sit under: the crew of
// its pr_open row, and the crew of each dispatch/resume row that started the
// session (a re-dispatch can carry another crew id). A plain status row never
// names one: a worker can post a status under any crew id.
func sessionCrews(events []bus.Event, session, prOpenCrew string) []string {
	crews := []string{prOpenCrew}
	for _, ev := range events {
		if ev.Kind != bus.KindDispatch && ev.Kind != bus.KindResume {
			continue
		}
		doc, _ := ev.Doc.(map[string]any)
		if id, _ := doc["worker_id"].(string); id == session && !slices.Contains(crews, ev.CrewID) {
			crews = append(crews, ev.CrewID)
		}
	}
	return crews
}

// plain makes a bus-supplied string safe to print: invisible code points and
// control characters (C1 included, which json.Marshal leaves raw) become
// \u{XXXX}. json.Marshal has already escaped C0 as \u00XX, so that text is not
// escaped twice.
func plain(s string) string {
	var b strings.Builder
	for _, r := range invisible.Escape(s) {
		if unicode.IsControl(r) {
			fmt.Fprintf(&b, `\u{%04X}`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// excerpt is a msg body as one line of text, cut to 80 runes.
func excerpt(body any) string {
	text, ok := body.(string)
	if !ok {
		b, _ := json.Marshal(body)
		text = string(b)
	}
	text = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
	if r := []rune(text); len(r) > 80 {
		text = string(r[:80])
	}
	return text
}

// checkLedger re-runs the pr_open gate on the newest pr_open row for this PR
// and returns that row.
func checkLedger(ctx context.Context, req Request, url string, lines []string, paths bus.Paths, o Options, refuse func(string, ...any) int, pass func(string, ...any)) (*statusRow, int) {
	found := newestPROpen(lines, url, req.Crew)
	if found == nil {
		return nil, refuse("no pr_open ledger on this bus for %s; check --crew, or pass --no-ledger for a PR no worker opened", url)
	}

	in := ledger.Input{From: found.From, Crew: found.CrewID, Detail: found.Body.Detail, Log: lines}
	doc, err := taskDoc(ctx, found.From, paths, o)
	if err != nil {
		return nil, refuse("could not locate the task doc of %s: %v", found.From, err)
	}
	if doc.unreadable {
		in.TaskDocErr = true
	}
	in.TaskDoc = doc.text
	if doc.kind != "" && doc.kind != "implement" {
		pass("ledger: skipped (kind %s)", doc.kind)
		return found, 0
	}
	if msg := ledger.Check(in); msg != "" {
		return nil, refuse("the ledger of %s no longer validates: %s", found.From, msg)
	}
	pass("ledger: %s validates", found.From)
	return found, 0
}

type doc struct {
	text, kind string
	unreadable bool
}

// taskDoc is the WORKER_TASK.md of the worker's branch when its worktree still
// exists; without it only the ledger's form is checked. A git that cannot list
// worktrees is an error, so a weaker check is never taken silently.
func taskDoc(ctx context.Context, from string, paths bus.Paths, o Options) (doc, error) {
	branch := strings.TrimPrefix(from, "worker:")
	if i := strings.LastIndex(branch, "#s"); i >= 0 {
		branch = branch[:i]
	}
	list, err := git.Worktrees(ctx, o.Git, o.Dir, paths.Common)
	if err != nil {
		return doc{}, err
	}
	var dir string
	for _, rec := range strings.Split(list, "\n\n") {
		var path string
		var match bool
		for line := range strings.SplitSeq(rec, "\n") {
			if v, ok := strings.CutPrefix(line, "worktree "); ok {
				path = v
			}
			if line == "branch refs/heads/"+branch {
				match = true
			}
		}
		if match {
			dir = path
			break
		}
	}
	if dir == "" {
		return doc{}, nil
	}
	path := dir + "/WORKER_TASK.md"
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return doc{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return doc{unreadable: true}, nil
	}
	d := doc{text: string(b)}
	for line := range strings.SplitSeq(d.text, "\n") {
		if v, ok := strings.CutPrefix(line, "kind:"); ok {
			d.kind = strings.Join(strings.Fields(v), "")
			break
		}
	}
	return d, nil
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
