package followups_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/followups"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
)

const (
	marker     = "Follow-up of #5 (PR #7)"
	fileA      = "adapters/core/guard.sh"
	hits3      = `[{"number":1},{"number":2},{"number":3},{"number":2}]`
	hits2      = `[{"number":1},{"number":2}]`
	createdURL = "https://example.test/issues/42"
)

var ledgerBlock = "<details><summary>Agent ledger</summary>\n\n- AC1 pass(bats)\n\n</details>\n"

// world is the scripted gh state of one run.
type world struct {
	prBody    string
	noIssues  bool
	views     map[int]string
	assigned  string
	hits      map[string]string
	failOn    string
	candidate string
}

func openOrigin() map[int]string {
	return map[int]string{5: `{"state":"OPEN","title":"origin","comments":[]}`}
}

func (w *world) gh() *ghtest.Fake {
	return &ghtest.Fake{Func: func(args []string) (string, error) {
		argv := strings.Join(args, " ")
		if w.failOn != "" && strings.HasPrefix(argv, w.failOn) {
			return "", fmt.Errorf("boom")
		}
		switch {
		case args[0] == "pr" && args[1] == "view":
			return fmt.Sprintf(`{"number":7,"url":"https://example.test/pull/7","body":%q}`, w.prBody), nil
		case args[0] == "repo":
			return fmt.Sprintf(`{"hasIssuesEnabled":%v}`, !w.noIssues), nil
		case args[0] == "issue" && args[1] == "view":
			var n int
			_, _ = fmt.Sscan(args[2], &n)
			if v, ok := w.views[n]; ok {
				return v, nil
			}
			return "", fmt.Errorf("no issue %d", n)
		case args[0] == "issue" && args[1] == "list" && slices.Contains(args, "--assignee"):
			if w.assigned == "" {
				return "[]", nil
			}
			return w.assigned, nil
		case args[0] == "issue" && args[1] == "list" && slices.Contains(args, "all"):
			for file, out := range w.hits {
				if strings.Contains(argv, `"`+file+`"`) {
					return out, nil
				}
			}
			return "[]", nil
		case args[0] == "issue" && args[1] == "list":
			if w.candidate == "" {
				return "[]", nil
			}
			return w.candidate, nil
		case args[0] == "issue" && args[1] == "create":
			return createdURL, nil
		}
		return "", nil
	}}
}

type result struct {
	code           int
	stdout, stderr string
	calls, writes  []string
}

func isWrite(call string) bool {
	for _, p := range []string{"issue create", "issue comment", "issue reopen", "issue close", "pr edit"} {
		if strings.HasPrefix(call, p) {
			return true
		}
	}
	return false
}

func run(t *testing.T, w *world, entries string, apply bool) result {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "entries.json")
	if err := os.WriteFile(path, []byte(entries), 0o644); err != nil {
		t.Fatal(err)
	}
	if w.prBody == "" {
		w.prBody = defaultBody()
	}
	g := w.gh()
	clk := clock.Clock{Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }}
	var stdout, stderr bytes.Buffer
	paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
	code := followups.Run(context.Background(), followups.Request{PR: "7", Entries: path, Apply: apply}, paths, &stdout, &stderr, followups.Options{GH: g, Clock: clk})
	res := result{code: code, stdout: stdout.String(), stderr: stderr.String(), calls: g.Calls()}
	for _, c := range res.calls {
		if isWrite(c) {
			res.writes = append(res.writes, c)
		}
	}
	return res
}

func comment(n int, title, body string) string {
	return fmt.Sprintf("issue comment %d --body %s\n\n%s\n\n%s", n, marker, title, body)
}

func prEdit(prBody string, links ...followups.Link) string {
	return "pr edit 7 --body " + followups.Rewrite(prBody, links)
}

func checkWrites(t *testing.T, r result, want ...string) {
	t.Helper()
	if !slices.Equal(r.writes, want) {
		t.Errorf("writes =\n%q\nwant\n%q", r.writes, want)
	}
}

func defaultBody() string {
	return "Closes #5\n\n## Follow-ups (untracked)\n- raw\n\n" + ledgerBlock
}

const one = `[{"title":"tidy guard","body":"the body","file":"adapters/core/guard.sh","same_area":true}]`

func TestSameAreaOpenOriginComments(t *testing.T) {
	r := run(t, &world{views: openOrigin()}, one, true)
	checkWrites(t, r,
		comment(5, "tidy guard", "the body"),
		prEdit(defaultBody(), followups.Link{N: 5, Title: "tidy guard"}))
	if r.code != 0 || !strings.Contains(r.stdout, "follow-ups for PR #7: commented #5") {
		t.Errorf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestSameAreaClosedOriginReopens(t *testing.T) {
	w := &world{views: map[int]string{5: `{"state":"CLOSED","title":"origin","comments":[]}`}}
	r := run(t, w, one, true)
	checkWrites(t, r,
		fmt.Sprintf("issue reopen 5 --comment %s\n\ntidy guard\n\nthe body", marker),
		prEdit(defaultBody(), followups.Link{N: 5, Title: "tidy guard", Note: "reopened"}))
	if !strings.Contains(r.stdout, "reopened #5") {
		t.Errorf("stdout %q", r.stdout)
	}
	if !strings.Contains(r.writes[1], "- #5 — tidy guard (reopened)") {
		t.Errorf("pr body lacks the reopen note: %q", r.writes[1])
	}
}

func TestDupOfComments(t *testing.T) {
	w := &world{views: map[int]string{9: `{"state":"OPEN","title":"dup","comments":[]}`}}
	r := run(t, w, `[{"title":"again","body":"b","dup_of":9}]`, true)
	checkWrites(t, r, comment(9, "again", "b"), prEdit(defaultBody(), followups.Link{N: 9, Title: "again"}))
}

func TestNewIssue(t *testing.T) {
	r := run(t, &world{views: openOrigin()}, `[{"title":"t1","body":"b1"}]`, true)
	checkWrites(t, r,
		"issue create --title t1 --body "+marker+"\n\nb1 --assignee @me",
		prEdit(defaultBody(), followups.Link{N: 42, Title: "t1"}))
	if !strings.Contains(r.stdout, "filed #42") {
		t.Errorf("stdout %q", r.stdout)
	}
}

func TestThreshold(t *testing.T) {
	entry := `[{"title":"tidy","body":"b","file":"` + fileA + `","same_area":true}]`
	t.Run("three trips it", func(t *testing.T) {
		w := &world{views: openOrigin(), hits: map[string]string{fileA: hits3}}
		r := run(t, w, entry, true)
		want := "issue create --title redesign: " + fileA + " --body " + marker +
			"\n\nFollow-ups touching " + fileA + " in 30 days: #1, #2, #3\n\n### tidy\n\nb --assignee @me"
		checkWrites(t, r, want, prEdit(defaultBody(), followups.Link{N: 42, Title: "redesign: " + fileA}))
		search := `issue list --state all --limit 200 --search "Follow-up of #" in:body,comments "` + fileA + `" updated:>=2026-09-20 --json number`
		if !slices.Contains(r.calls, search) {
			t.Errorf("threshold search missing from %q", r.calls)
		}
	})
	t.Run("two does not", func(t *testing.T) {
		w := &world{views: openOrigin(), hits: map[string]string{fileA: hits2}}
		r := run(t, w, entry, true)
		if len(r.writes) != 2 || !strings.HasPrefix(r.writes[0], "issue comment 5") {
			t.Errorf("writes %q", r.writes)
		}
	})
}

func TestRerunWritesNothing(t *testing.T) {
	doneBody := "Closes #5\n\n## Follow-ups\n- #42 — t1\n- #5 — c1\n\n" + ledgerBlock
	w := &world{
		prBody:   doneBody,
		assigned: fmt.Sprintf(`[{"number":42,"title":"t1","body":%q}]`, marker+"\n\nb1"),
		views:    map[int]string{5: fmt.Sprintf(`{"state":"OPEN","title":"o","comments":[{"body":%q}]}`, marker+"\n\nc1\n\nbody")},
	}
	r := run(t, w, `[{"title":"t1","body":"b1"},{"title":"c1","body":"body","same_area":true}]`, true)
	checkWrites(t, r)
	if r.code != 0 || !strings.Contains(r.stdout, "nothing new") {
		t.Errorf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestRerunClosedOriginNotReopenedAgain(t *testing.T) {
	w := &world{views: map[int]string{5: fmt.Sprintf(`{"state":"CLOSED","title":"o","comments":[{"body":%q}]}`, marker+"\n\ntidy guard\n\nthe body")}}
	r := run(t, w, one, true)
	for _, c := range r.writes {
		if strings.HasPrefix(c, "issue reopen") || strings.HasPrefix(c, "issue comment") {
			t.Errorf("unexpected write %q", c)
		}
	}
}

func TestThresholdDriftRerun(t *testing.T) {
	entry := `[{"title":"t1","body":"b1","file":"` + fileA + `"}]`
	w := &world{views: openOrigin(), hits: map[string]string{fileA: hits2}}
	r1 := run(t, w, entry, true)
	if len(r1.writes) == 0 || !strings.HasPrefix(r1.writes[0], "issue create --title t1 ") {
		t.Fatalf("run 1 writes %q", r1.writes)
	}
	w.hits = map[string]string{fileA: `[{"number":1},{"number":2},{"number":42}]`}
	w.assigned = fmt.Sprintf(`[{"number":42,"title":"t1","body":%q}]`, marker+"\n\nb1")
	w.prBody = "Closes #5\n\n## Follow-ups\n- #42 — t1\n\n" + ledgerBlock
	r2 := run(t, w, entry, true)
	checkWrites(t, r2)
	if !strings.Contains(r2.stdout, "nothing new") {
		t.Errorf("stdout %q", r2.stdout)
	}
}

func TestSharedRedesignIssue(t *testing.T) {
	entries := `[{"title":"first","body":"b1","file":"` + fileA + `"},{"title":"second","body":"b2","file":"` + fileA + `"}]`
	w := &world{views: openOrigin(), hits: map[string]string{fileA: hits3}}
	r := run(t, w, entries, true)
	var creates []string
	for _, c := range r.writes {
		if strings.HasPrefix(c, "issue create") {
			creates = append(creates, c)
		}
	}
	if len(creates) != 1 || !strings.Contains(creates[0], "### first\n\nb1\n\n### second\n\nb2") {
		t.Fatalf("creates %q", creates)
	}
	searches := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, "issue list --state all --limit 200 --search") {
			searches++
		}
	}
	if searches != 1 {
		t.Errorf("threshold snapshots = %d, want 1", searches)
	}

	body := marker + "\n\nFollow-ups touching " + fileA + " in 30 days: #1, #2, #3\n\n### first\n\nb1\n\n### second\n\nb2"
	w.assigned = fmt.Sprintf(`[{"number":42,"title":"redesign: %s","body":%q}]`, fileA, body)
	w.prBody = "Closes #5\n\n## Follow-ups\n- #42 — redesign: " + fileA + "\n\n" + ledgerBlock
	r = run(t, w, entries, true)
	checkWrites(t, r)
	if !strings.Contains(r.stdout, "nothing new") {
		t.Errorf("stdout %q", r.stdout)
	}
}

func TestSupersedesCloses(t *testing.T) {
	w := &world{views: map[int]string{
		3: `{"state":"OPEN","title":"old","comments":[]}`,
		4: `{"state":"CLOSED","title":"older","comments":[]}`,
	}}
	r := run(t, w, `[{"title":"t1","body":"b1","supersedes":[3,4]}]`, true)
	checkWrites(t, r,
		"issue create --title t1 --body "+marker+"\n\nb1 --assignee @me",
		"issue close 3 --comment "+marker+"\n\nSuperseded by #42.",
		prEdit(defaultBody(), followups.Link{N: 42, Title: "t1"}))
	if !strings.Contains(r.stdout, "filed #42, closed #3") {
		t.Errorf("stdout %q", r.stdout)
	}
}

func TestPRBodySwapKeepsLedger(t *testing.T) {
	r := run(t, &world{views: openOrigin()}, `[{"title":"t1","body":"b1"}]`, true)
	want := "pr edit 7 --body Closes #5\n\n## Follow-ups\n- #42 — t1\n\n" + ledgerBlock
	if got := r.writes[len(r.writes)-1]; got != want {
		t.Errorf("pr edit =\n%q\nwant\n%q", got, want)
	}
}

func TestPRBodyCRLF(t *testing.T) {
	led := "<details><summary>Agent ledger</summary>\r\n\r\n- AC1 pass\r\n\r\n</details>\r\n"
	w := &world{views: openOrigin(), prBody: "Closes #5\r\n\r\n## Follow-ups (untracked)\r\n- raw\r\n\r\n" + led}
	r := run(t, w, `[{"title":"t1","body":"b1"}]`, true)
	want := "pr edit 7 --body Closes #5\r\n\r\n## Follow-ups\r\n- #42 — t1\r\n\r\n" + led
	if got := r.writes[len(r.writes)-1]; got != want {
		t.Errorf("pr edit =\n%q\nwant\n%q", got, want)
	}
}

func TestLinear(t *testing.T) {
	entries := `[{"title":"t1","body":"b1","same_area":true},{"title":"t2","body":"b2"}]`
	for name, w := range map[string]*world{
		"ticket in Closes": {prBody: "Closes ABC-12\n"},
		"issues disabled":  {prBody: "Closes #5\n", noIssues: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := run(t, w, entries, true)
			if r.code != 1 || !strings.Contains(r.stderr, "crew: followups: refusing --apply — Linear-tracked") || len(r.writes) != 0 {
				t.Errorf("apply: code %d stderr %q writes %q", r.code, r.stderr, r.writes)
			}
			r = run(t, w, entries, false)
			if r.code != 0 || len(r.writes) != 0 || !strings.Contains(r.stdout, "new ticket") {
				t.Errorf("plan: code %d stdout %q writes %q", r.code, r.stdout, r.writes)
			}
			for _, c := range r.calls {
				if strings.HasPrefix(c, "issue") {
					t.Errorf("Linear plan read GitHub issues: %q", c)
				}
			}
		})
	}
	r := run(t, &world{prBody: "Closes ABC-12\n"}, entries, false)
	if !strings.Contains(r.stdout, `entry 1 "t1": comment on parent ABC-12`) {
		t.Errorf("stdout %q", r.stdout)
	}
}

func TestPlanModeWritesNothing(t *testing.T) {
	w := &world{
		views:     map[int]string{5: `{"state":"CLOSED","title":"o","comments":[]}`, 3: `{"state":"OPEN","title":"x","comments":[]}`},
		hits:      map[string]string{"x/y.go": hits3},
		candidate: `[{"number":8,"title":"similar"}]`,
	}
	entries := `[{"title":"a","body":"b","same_area":true,"supersedes":[3]},{"title":"c","body":"d","file":"x/y.go"},{"title":"e","body":"f"}]`
	r := run(t, w, entries, false)
	if r.code != 0 || len(r.writes) != 0 {
		t.Fatalf("code %d writes %q stderr %q", r.code, r.writes, r.stderr)
	}
	for _, want := range []string{
		`entry 1 "a": reopen origin #5`,
		"  closes: #3",
		"  candidates: #8 similar",
		`entry 2 "c": redesign issue redesign: x/y.go`,
		`entry 3 "e": new issue`,
		"plan only; pass --apply to write",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestPlanMarksDoneEntries(t *testing.T) {
	w := &world{assigned: fmt.Sprintf(`[{"number":42,"title":"t1","body":%q}]`, marker+"\n\nb1")}
	r := run(t, w, `[{"title":"t1","body":"b1"}]`, false)
	if !strings.Contains(r.stdout, `entry 1 "t1": new issue (already done: #42)`) {
		t.Errorf("stdout %q", r.stdout)
	}
}

func TestWriteFailure(t *testing.T) {
	entries := `[{"title":"t1","body":"b1"},{"title":"t2","body":"b2"}]`
	w := &world{views: openOrigin(), failOn: "issue create --title t2"}
	r := run(t, w, entries, true)
	if r.code != 1 || !strings.Contains(r.stderr, "crew: followups: gh issue create \"t2\" failed") {
		t.Errorf("code %d stderr %q", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "follow-ups for PR #7: filed #42") {
		t.Errorf("partial summary missing: %q", r.stdout)
	}
	for _, c := range r.writes {
		if strings.HasPrefix(c, "pr edit") {
			t.Errorf("pr edit after a failed write: %q", c)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		r := run(t, &world{}, `[{"title":"t","body":"b","fil":"x"}]`, false)
		if r.code != 1 || !strings.Contains(r.stderr, "crew: followups: unreadable entries file") {
			t.Errorf("code %d stderr %q", r.code, r.stderr)
		}
	})
	t.Run("empty title", func(t *testing.T) {
		r := run(t, &world{}, `[{"title":" ","body":"b"}]`, false)
		if r.code != 1 || !strings.Contains(r.stderr, "has no title") {
			t.Errorf("code %d stderr %q", r.code, r.stderr)
		}
	})
}

func TestUsage(t *testing.T) {
	tests := []struct {
		name string
		req  followups.Request
	}{
		{"bad PR", followups.Request{PR: "x", Entries: "f"}},
		{"zero PR", followups.Request{PR: "0", Entries: "f"}},
		{"neither mode", followups.Request{PR: "7"}},
		{"both modes", followups.Request{PR: "7", Entries: "f", Draft: true}},
		{"draft with apply", followups.Request{PR: "7", Draft: true, Apply: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			g := &ghtest.Fake{}
			code := followups.Run(context.Background(), tt.req, bus.Paths{}, &stdout, &stderr, followups.Options{GH: g})
			if code != 1 || stderr.String() != "crew: followups: "+followups.Usage+"\n" || len(g.Calls()) != 0 {
				t.Errorf("code %d stderr %q calls %q", code, stderr.String(), g.Calls())
			}
		})
	}
}
