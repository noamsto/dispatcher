// Package merge is `crew merge <N>`: a squash-merge that first proves the PR's
// head is green, the base is not red, and the worker's acceptance ledger still
// validates — so a dispatcher never merges on a worker's report alone.
package merge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/ledger"
)

// Usage is the one-line signature.
const Usage = "usage: crew merge <N> [--dry-run] [--allow-no-ci] [--no-ledger] [--crew ID]"

const exitFailure = 1

// Options is everything Run reads beyond the bus.
type Options struct {
	GH  gh.Client
	Git git.Client
	// Dir is where git runs, so the worktree list resolves the same repo.
	Dir string
}

// Request is the parsed command line.
type Request struct {
	PR                          string
	DryRun, AllowNoCI, NoLedger bool
	Crew                        string
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

type baseRuns struct {
	CheckRuns []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
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
	refuse := func(format string, args ...any) int {
		say(stderr, "crew: merge: refusing PR %d — %s\n", n, fmt.Sprintf(format, args...))
		return exitFailure
	}
	var report []string
	pass := func(format string, args ...any) { report = append(report, fmt.Sprintf(format, args...)) }

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

	out, err = o.GH.Run(ctx, "api", "repos/{owner}/{repo}/commits/"+p.BaseRefName+"/check-runs?per_page=100")
	if err != nil {
		return refuse("could not read %s's check runs: %v", p.BaseRefName, err)
	}
	var base baseRuns
	if err := json.Unmarshal([]byte(out), &base); err != nil {
		return refuse("unreadable check runs of %s: %v", p.BaseRefName, err)
	}
	for _, r := range base.CheckRuns {
		if strings.EqualFold(r.Status, "completed") && redConclusions[strings.ToLower(r.Conclusion)] {
			return refuse("base %s is red (%s: %s); fix the base before merging", p.BaseRefName, r.Name, r.Conclusion)
		}
	}
	pass("base %s: not red", p.BaseRefName)

	if req.NoLedger {
		pass("ledger: skipped (--no-ledger)")
	} else if code := checkLedger(ctx, req, p.URL, paths, o, refuse, pass); code != 0 {
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
	return 0
}

// checkLedger re-runs the pr_open gate on the newest pr_open row for this PR.
func checkLedger(ctx context.Context, req Request, url string, paths bus.Paths, o Options, refuse func(string, ...any) int, pass func(string, ...any)) int {
	data, err := os.ReadFile(paths.Log)
	if err != nil && !os.IsNotExist(err) {
		return refuse("could not read the bus: %v", err)
	}
	var lines []string
	var found *statusRow
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		line := sc.Text()
		lines = append(lines, line)
		var r statusRow
		if json.Unmarshal([]byte(line), &r) != nil || r.Kind != "status" || r.Body.State != "pr_open" || r.Body.PRURL != url {
			continue
		}
		if req.Crew != "" && r.CrewID != req.Crew {
			continue
		}
		found = &r
	}
	if found == nil {
		return refuse("no pr_open ledger on this bus for %s; check --crew, or pass --no-ledger for a PR no worker opened", url)
	}

	in := ledger.Input{From: found.From, Crew: found.CrewID, Detail: found.Body.Detail, Log: lines}
	if doc, ok := taskDoc(ctx, found.From, paths, o); ok {
		in.TaskDoc = doc
	}
	if msg := ledger.Check(in); msg != "" {
		return refuse("the ledger of %s no longer validates: %s", found.From, msg)
	}
	pass("ledger: %s validates", found.From)
	return 0
}

// taskDoc is the WORKER_TASK.md of the worker's branch when its worktree still
// exists; without it only the ledger's form is checked.
func taskDoc(ctx context.Context, from string, paths bus.Paths, o Options) (string, bool) {
	branch := strings.TrimPrefix(from, "worker:")
	if i := strings.LastIndex(branch, "#s"); i >= 0 {
		branch = branch[:i]
	}
	list, err := git.Worktrees(ctx, o.Git, o.Dir, paths.Common)
	if err != nil {
		return "", false
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
		return "", false
	}
	b, err := os.ReadFile(dir + "/WORKER_TASK.md")
	if err != nil {
		return "", false
	}
	return string(b), true
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
