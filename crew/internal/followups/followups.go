// Package followups is `crew followups <PR>`: the mechanical half of the
// dispatcher's follow-up procedure — dedupe, same-area, the redesign
// threshold, filing and the PR-body swap — as a plan the dispatcher reviews
// and then applies.
package followups

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/gh"
)

// Usage is the one-line signature.
const Usage = "usage: crew followups <PR> (--entries FILE [--apply] [--tracker github|linear] | --draft [--crew ID])"

const (
	exitFailure = 1
	// thresholdCount distinct follow-ups on one file in thresholdDays days
	// mean the file needs a redesign, not another patch.
	thresholdCount = 3
	thresholdDays  = 30
)

// Request is the parsed command line.
type Request struct {
	PR      string
	Entries string
	Apply   bool
	Draft   bool
	// Crew only filters --draft's bus rows.
	Crew string
	// Tracker "linear" forces the Linear path; "github" or empty detects it.
	Tracker string
}

// Options is everything Run reads beyond the bus.
type Options struct {
	GH    gh.Client
	Clock clock.Clock
}

type entry struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	File       string `json:"file"`
	SameArea   bool   `json:"same_area"`
	DupOf      int    `json:"dup_of"`
	Supersedes []int  `json:"supersedes"`
}

type issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

type issueView struct {
	State    string `json:"state"`
	Title    string `json:"title"`
	Comments []struct {
		Body string `json:"body"`
	} `json:"comments"`
}

type action int

const (
	newIssue action = iota
	redesignIssue
	commentOn
	reopen
)

// step is one entry's resolved action. done is the issue that already carries
// the entry, 0 when it still has to be written.
type step struct {
	n      int
	e      entry
	act    action
	target int
	done   int
	filed  bool
}

// redesign is one issue shared by every batch entry on a file.
type redesign struct {
	file    string
	touched []int
	steps   []*step
	number  int
}

func (r *redesign) title() string { return "redesign: " + r.file }

type run struct {
	ctx    context.Context
	gh     gh.Client
	stdout io.Writer
	stderr io.Writer

	pr     int
	marker string
	origin int
	// closes is whether the PR body names the origin; otherwise origin is the PR.
	closes   bool
	assigned []issue
	views    map[int]*issueView
}

// ref is the first `Closes` target of a PR body.
type ref struct {
	num    int
	ticket string
}

var closesRe = regexp.MustCompile(`(?m)^\s*(?i:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+(?:#(\d+)|([A-Z][A-Z0-9]*-\d+))`)

func closesRef(body string) ref {
	m := closesRe.FindStringSubmatch(body)
	if m == nil {
		return ref{}
	}
	if m[1] != "" {
		n, _ := strconv.Atoi(m[1])
		return ref{num: n}
	}
	return ref{ticket: m[2]}
}

// Run is the command: read, plan, and with --apply write.
func Run(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	n, err := strconv.Atoi(req.PR)
	if err != nil || n <= 0 || (req.Entries != "") == req.Draft || (req.Draft && (req.Apply || req.Tracker != "")) ||
		(!req.Draft && req.Crew != "") || !slices.Contains([]string{"", "github", "linear"}, req.Tracker) {
		say(stderr, "crew: followups: %s\n", Usage)
		return exitFailure
	}
	if req.Draft {
		return draft(n, req.Crew, paths, stdout, stderr)
	}
	fail := func(format string, args ...any) int {
		say(stderr, "crew: followups: "+format+"\n", args...)
		return exitFailure
	}

	entries, err := readEntries(req.Entries)
	if err != nil {
		return fail("%v", err)
	}
	r := &run{ctx: ctx, gh: o.GH, stdout: stdout, stderr: stderr, pr: n, views: map[int]*issueView{}}

	var p struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Body   string `json:"body"`
	}
	if err := r.read(&p, "pr", "view", req.PR, "--json", "number,url,body"); err != nil {
		return fail("%v", err)
	}
	var repo struct {
		HasIssuesEnabled bool `json:"hasIssuesEnabled"`
	}
	if err := r.read(&repo, "repo", "view", "--json", "hasIssuesEnabled"); err != nil {
		return fail("%v", err)
	}

	c := closesRef(p.Body)
	if req.Tracker == "linear" || !repo.HasIssuesEnabled || c.ticket != "" {
		if req.Apply {
			return fail("refusing --apply — Linear-tracked; the batch needs one human approval, write it with the Linear tools")
		}
		r.planLinear(entries, c.ticket)
		return 0
	}

	r.origin, r.closes = n, false
	if c.num > 0 {
		r.origin, r.closes = c.num, true
	}
	r.marker = fmt.Sprintf("Follow-up of #%d (PR #%d)", r.origin, n)
	if err := r.read(&r.assigned, "issue", "list", "--assignee", "@me", "--state", "all", "--limit", "200", "--json", "number,title,body"); err != nil {
		return fail("%v", err)
	}

	steps, groups, err := r.resolve(entries, o.Clock.Now())
	if err != nil {
		return fail("%v", err)
	}
	if !req.Apply {
		if err := r.printPlan(steps); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	return r.apply(steps, groups, p.Body)
}

func readEntries(path string) ([]entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read entries: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var entries []entry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("unreadable entries file %s: %v", path, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("entries file %s holds no entries", path)
	}
	for i, e := range entries {
		switch {
		case strings.TrimSpace(e.Title) == "":
			return nil, fmt.Errorf("entry %d has no title", i+1)
		case strings.TrimSpace(e.Body) == "":
			return nil, fmt.Errorf("entry %d (%q) has no body", i+1, e.Title)
		case e.DupOf < 0:
			return nil, fmt.Errorf("entry %d (%q) has a negative dup_of", i+1, e.Title)
		}
	}
	return entries, nil
}

func (r *run) read(dst any, args ...string) error {
	out, err := r.gh.Run(r.ctx, args...)
	if err != nil {
		return fmt.Errorf("gh %s failed: %v", strings.Join(args[:2], " "), err)
	}
	if err := json.Unmarshal([]byte(out), dst); err != nil {
		return fmt.Errorf("unreadable gh %s output: %v", strings.Join(args[:2], " "), err)
	}
	return nil
}

// view is `gh issue view`, cached for the run.
func (r *run) view(n int) (*issueView, error) {
	if v, ok := r.views[n]; ok {
		return v, nil
	}
	v := &issueView{}
	if err := r.read(v, "issue", "view", strconv.Itoa(n), "--json", "state,title,comments"); err != nil {
		return nil, err
	}
	r.views[n] = v
	return v, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSuffix(l, "\r")
}

// commented is whether issue n carries this run's comment for the entry.
func (r *run) commented(n int, title string) (bool, error) {
	v, err := r.view(n)
	if err != nil {
		return false, err
	}
	for _, c := range v.Comments {
		if firstLine(c.Body) == r.marker && strings.Contains(c.Body, title) {
			return true, nil
		}
	}
	return false, nil
}

// filed is the assigned issue that already carries the entry, 0 when none.
func (r *run) filed(e entry) (n int, redesign bool) {
	for _, is := range r.assigned {
		if firstLine(is.Body) != r.marker {
			continue
		}
		switch {
		case is.Title == e.Title:
			return is.Number, false
		case e.File != "" && is.Title == "redesign: "+e.File && strings.Contains(is.Body, e.Title):
			return is.Number, true
		}
	}
	return 0, false
}

// detectDone looks for the entry's earlier write, so a re-run never derives a
// different action from the state its own first run changed.
func (r *run) detectDone(s *step) error {
	if n, red := r.filed(s.e); n != 0 {
		s.done, s.target, s.filed = n, n, true
		if red {
			s.act = redesignIssue
		}
		return nil
	}
	var targets []int
	if s.e.DupOf > 0 {
		targets = append(targets, s.e.DupOf)
	}
	if s.e.SameArea && r.closes {
		targets = append(targets, r.origin)
	}
	for _, t := range targets {
		ok, err := r.commented(t, s.e.Title)
		if err != nil {
			return err
		}
		if ok {
			s.act, s.done, s.target = commentOn, t, t
			return nil
		}
	}
	return nil
}

// resolve picks every entry's action. The threshold counts are one snapshot
// per file, taken before any write, so an entry's own write cannot push a
// sibling over mid-apply. The batch's own filed issues are not prior hits;
// comment targets are, so a re-run after a partial apply counts the same
// prior hits as the first run.
func (r *run) resolve(entries []entry, now time.Time) ([]*step, []*redesign, error) {
	steps := make([]*step, len(entries))
	for i, e := range entries {
		steps[i] = &step{n: i + 1, e: e}
		if err := r.detectDone(steps[i]); err != nil {
			return nil, nil, err
		}
	}
	var own []int
	for _, s := range steps {
		if s.filed {
			own = append(own, s.done)
		}
	}
	since := now.UTC().AddDate(0, 0, -thresholdDays).Format("2006-01-02")
	touched := map[string][]int{}
	for _, s := range steps {
		f := s.e.File
		if s.done != 0 || f == "" {
			continue
		}
		if _, ok := touched[f]; ok {
			continue
		}
		var hits []struct {
			Number int `json:"number"`
		}
		search := fmt.Sprintf(`"Follow-up of #" in:body,comments %q updated:>=%s`, f, since)
		if err := r.read(&hits, "issue", "list", "--state", "all", "--limit", "200", "--search", search, "--json", "number"); err != nil {
			return nil, nil, err
		}
		var nums []int
		for _, h := range hits {
			if !slices.Contains(nums, h.Number) && !slices.Contains(own, h.Number) {
				nums = append(nums, h.Number)
			}
		}
		slices.Sort(nums)
		touched[f] = nums
	}

	var groups []*redesign
	byFile := map[string]*redesign{}
	for _, s := range steps {
		if s.done != 0 {
			continue
		}
		e := s.e
		switch {
		case e.File != "" && len(touched[e.File]) >= thresholdCount:
			g := byFile[e.File]
			if g == nil {
				g = &redesign{file: e.File, touched: touched[e.File]}
				byFile[e.File] = g
				groups = append(groups, g)
			}
			g.steps = append(g.steps, s)
			s.act = redesignIssue
		case e.DupOf > 0:
			s.act, s.target = commentOn, e.DupOf
		case e.SameArea && r.closes:
			v, err := r.view(r.origin)
			if err != nil {
				return nil, nil, err
			}
			s.act, s.target = commentOn, r.origin
			if v.State == "CLOSED" {
				s.act = reopen
			}
		default:
			if e.SameArea {
				say(r.stderr, "crew: followups: entry %d (%q): PR body has no Closes line, so same_area is treated as a normal entry\n", s.n, e.Title)
			}
		}
	}
	return steps, groups, nil
}

func (r *run) describe(s *step) string {
	var a string
	switch s.act {
	case newIssue:
		a = "new issue"
	case redesignIssue:
		a = "redesign issue redesign: " + s.e.File
	case commentOn:
		a = fmt.Sprintf("comment on #%d", s.target)
		if s.target == r.origin && s.e.DupOf != s.target {
			a = fmt.Sprintf("comment on origin #%d", s.target)
		}
	case reopen:
		a = fmt.Sprintf("reopen origin #%d", s.target)
	}
	if s.done != 0 {
		a += fmt.Sprintf(" (already done: #%d)", s.done)
	}
	return a
}

func (r *run) printPlan(steps []*step) error {
	for _, s := range steps {
		say(r.stdout, "entry %d %q: %s\n", s.n, s.e.Title, r.describe(s))
		if len(s.e.Supersedes) > 0 {
			say(r.stdout, "  closes: %s\n", refs(s.e.Supersedes))
		}
		if s.e.DupOf > 0 || s.done != 0 {
			continue
		}
		var cands []issue
		if err := r.read(&cands, "issue", "list", "--state", "open", "--search", s.e.Title, "--limit", "5", "--json", "number,title"); err != nil {
			return err
		}
		if len(cands) == 0 {
			continue
		}
		parts := make([]string, len(cands))
		for i, c := range cands {
			parts[i] = fmt.Sprintf("#%d %s", c.Number, c.Title)
		}
		say(r.stdout, "  candidates: %s\n", strings.Join(parts, ", "))
	}
	say(r.stdout, "plan only; pass --apply to write\n")
	return nil
}

func (r *run) planLinear(entries []entry, parent string) {
	for i, e := range entries {
		a := "new ticket"
		if e.SameArea && parent != "" {
			a = "comment on parent " + parent
		}
		say(r.stdout, "entry %d %q: %s\n", i+1, e.Title, a)
	}
	say(r.stdout, "plan only; pass --apply to write\n")
}

func refs(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

type event struct {
	verb string
	n    int
}

func (r *run) commentBody(s *step) string {
	return r.marker + "\n\n" + s.e.Title + "\n\n" + s.e.Body
}

func (g *redesign) body(marker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nFollow-ups touching %s in %d days: %s", marker, g.file, thresholdDays, refs(g.touched))
	for _, s := range g.steps {
		fmt.Fprintf(&b, "\n\n### %s\n\n%s", s.e.Title, s.e.Body)
	}
	return b.String()
}

var issueNumberRe = regexp.MustCompile(`/(\d+)$`)

func (r *run) create(title, body string) (int, error) {
	out, err := r.gh.Run(r.ctx, "issue", "create", "--title", title, "--body", body, "--assignee", "@me")
	if err != nil {
		return 0, fmt.Errorf("gh issue create %q failed: %v", title, err)
	}
	m := issueNumberRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0, fmt.Errorf("gh issue create %q printed no issue URL: %q", title, out)
	}
	return strconv.Atoi(m[1])
}

func (r *run) apply(steps []*step, groups []*redesign, prBody string) int {
	var events []event
	record := func(verb string, n int) {
		if ev := (event{verb, n}); !slices.Contains(events, ev) {
			events = append(events, ev)
		}
	}
	summary := func() {
		if len(events) == 0 {
			say(r.stdout, "follow-ups for PR #%d: nothing new\n", r.pr)
			return
		}
		parts := make([]string, len(events))
		for i, ev := range events {
			parts[i] = fmt.Sprintf("%s #%d", ev.verb, ev.n)
		}
		say(r.stdout, "follow-ups for PR #%d: %s\n", r.pr, strings.Join(parts, ", "))
	}
	failed := func(err error) int {
		if len(events) > 0 {
			summary()
		}
		say(r.stderr, "crew: followups: %v\n", err)
		return exitFailure
	}

	groupOf := map[*step]*redesign{}
	for _, g := range groups {
		for _, s := range g.steps {
			groupOf[s] = g
		}
	}
	reopened := false
	var links []Link
	for _, s := range steps {
		note := ""
		if s.done == 0 {
			verb, err := r.write(s, groupOf[s], &reopened)
			if err != nil {
				return failed(err)
			}
			record(verb, s.target)
			if verb == "reopened" {
				note = "reopened"
			}
		}
		title := s.e.Title
		if s.act == redesignIssue {
			title = "redesign: " + s.e.File
		}
		if l := (Link{N: s.target, Title: title, Note: note}); !slices.Contains(links, l) {
			links = append(links, l)
		}
		for _, n := range s.e.Supersedes {
			closed, err := r.supersede(n, s.target)
			if err != nil {
				return failed(err)
			}
			if closed {
				record("closed", n)
			}
		}
	}

	if body := Rewrite(prBody, links); body != prBody {
		if _, err := r.gh.Run(r.ctx, "pr", "edit", strconv.Itoa(r.pr), "--body", body); err != nil {
			return failed(fmt.Errorf("gh pr edit failed: %v", err))
		}
	}
	summary()
	return 0
}

// write performs the step's one gh write and names what it did.
func (r *run) write(s *step, g *redesign, reopened *bool) (string, error) {
	var err error
	switch {
	case s.act == newIssue:
		s.target, err = r.create(s.e.Title, r.marker+"\n\n"+s.e.Body)
		return "filed", err
	case s.act == redesignIssue:
		if g.number == 0 {
			if g.number, err = r.create(g.title(), g.body(r.marker)); err != nil {
				return "", err
			}
		}
		s.target = g.number
		return "filed", nil
	case s.act == reopen && !*reopened:
		_, err = r.gh.Run(r.ctx, "issue", "reopen", strconv.Itoa(s.target), "--comment", r.commentBody(s))
		*reopened = err == nil
		return "reopened", err
	}
	_, err = r.gh.Run(r.ctx, "issue", "comment", strconv.Itoa(s.target), "--body", r.commentBody(s))
	return "commented", err
}

// supersede closes n in favour of by unless it is already closed.
func (r *run) supersede(n, by int) (bool, error) {
	v, err := r.view(n)
	if err != nil {
		return false, err
	}
	if v.State == "CLOSED" {
		return false, nil
	}
	comment := fmt.Sprintf("%s\n\nSuperseded by #%d.", r.marker, by)
	if _, err := r.gh.Run(r.ctx, "issue", "close", strconv.Itoa(n), "--comment", comment); err != nil {
		return false, fmt.Errorf("gh issue close #%d failed: %v", n, err)
	}
	v.State = "CLOSED"
	return true, nil
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
