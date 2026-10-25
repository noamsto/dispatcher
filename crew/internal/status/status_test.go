package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/ledger"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	crewID = "c1"
	worker = "worker:feat/x#s1-1"
	branch = "worker:feat/x"
	rev    = "role:feat/x:reviewer"
	tsText = `1700000000500`
)

type foldFunc = func(prog string, lines []string, vars map[string]any) (any, error)

// fixture is a checkout with a bus: Toplevel and the bus paths are plain temp
// dirs, so no git is involved.
type fixture struct {
	t      *testing.T
	top    string
	paths  bus.Paths
	env    map[string]string
	tm     *tmuxtest.Fake // every tmux call; answers the @crew_role query with role
	role   string         // stdout of the @crew_role query
	noTmux bool
	noTop  bool
	fold   foldFunc
	crew   string

	panes string     // sender check: tmux list-panes output; "" leaves the anchor unreadable
	proc  proc.Table // sender check: the caller's ancestry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:     t,
		top:   root,
		paths: bus.Paths{Common: root + "/.git", Dir: root + "/.git/crew", Log: root + "/.git/crew/events.jsonl"},
		env:   map[string]string{},
		crew:  crewID,
	}
	f.tm = &tmuxtest.Fake{Func: func(args []string) (string, error) {
		if args[0] == "display-message" {
			return f.role, nil
		}
		return "", nil
	}}
	return f
}

func (f *fixture) opts() Options {
	o := Options{
		CrewID: func() string { return f.crew },
		Git:    &gittest.Fake{Out: map[string]string{"-C . rev-parse --show-toplevel": f.top}},
		Clock:  clock.Clock{Now: func() time.Time { return time.UnixMilli(1700000000500) }},
		Fold:   f.fold,
		Sender: sender.Probes{Getenv: func(k string) string { return f.env[k] }},
	}
	if f.noTop {
		o.Git = &gittest.Fake{Out: map[string]string{"-C . rev-parse --show-toplevel": ""}}
	}
	if f.panes != "" {
		o.Sender.Tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": f.panes}}
		o.Sender.Proc = f.proc
	}
	if !f.noTmux {
		o.Tmux = f.tm
	}
	return o
}

// status runs the positionals from, state, detail, pr; the words past the
// fourth belong to the grammar, which main_test.go covers.
func (f *fixture) status(args ...string) (int, string) {
	return f.statusReq(StatusRequest{
		From: at(args, 0), State: at(args, 1), Detail: at(args, 2), PR: at(args, 3),
	})
}

func (f *fixture) statusReq(req StatusRequest) (int, string) {
	var stderr bytes.Buffer
	code := RunStatus(f.t.Context(), req, f.paths, &stderr, f.opts())
	return code, stderr.String()
}

func (f *fixture) msg(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := RunMsg(f.t.Context(), MsgRequest{From: at(args, 0), To: at(args, 1), Body: at(args, 2)}, f.paths, &stderr, f.opts())
	return code, stderr.String()
}

func at(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// doc writes the checkout's WORKER_TASK.md.
func (f *fixture) doc(s string) {
	f.t.Helper()
	if err := os.WriteFile(f.top+"/WORKER_TASK.md", []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// log seeds the bus; no rows means no log file at all.
func (f *fixture) log(rows ...string) {
	f.t.Helper()
	if err := os.MkdirAll(f.paths.Dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if len(rows) == 0 {
		return
	}
	if err := os.WriteFile(f.paths.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) rawLog() string {
	data, _ := os.ReadFile(f.paths.Log)
	return string(data)
}

// lines is the log's rows.
func (f *fixture) lines() []string {
	raw := strings.TrimSuffix(f.rawLog(), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// wantRow asserts the log holds exactly one row, value-equal to want.
func (f *fixture) wantRow(want string) {
	f.t.Helper()
	got := f.lines()
	if len(got) != 1 {
		f.t.Fatalf("log has %d rows, want 1: %q", len(got), got)
	}
	if g, w := testjson.Compact(testjson.MustParse(f.t, got[0])), testjson.Compact(testjson.MustParse(f.t, want)); g != w {
		f.t.Errorf("row:\n got  %s\n want %s", g, w)
	}
}

func (f *fixture) wantNoRow() {
	f.t.Helper()
	if got := f.lines(); len(got) != 0 {
		f.t.Fatalf("log has rows, want none: %q", got)
	}
}

func jsonStr(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func statusRow(from, body string) string {
	return `{"ts":` + tsText + `,"crew_id":"c1","from":"` + from + `","to":"dispatcher:c1","kind":"status","body":` + body + `}`
}

// msgRow is a msg row whose body is the JSON text body, as `crew msg` writes it.
func msgRow(from, to, body string) string {
	return `{"ts":1,"crew_id":"c1","from":"` + from + `","to":"` + to + `","kind":"msg","body":` + jsonStr(body) + `}`
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a chmod-000 file")
	}
}

func TestCrewIDUnset(t *testing.T) {
	f := newFixture(t)
	f.crew = ""
	code, stderr := f.status(worker, "working")
	if code != 1 || stderr != "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n" {
		t.Errorf("got %d %q", code, stderr)
	}
	if _, err := os.Stat(f.paths.Dir); err == nil {
		t.Error("bus dir created before the crew id check")
	}
}

func TestMkdirBeforeParse(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.status(worker, "bogus"); code != 1 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(f.paths.Dir); err != nil {
		t.Errorf("bus dir not created ahead of the refusal: %v", err)
	}
}

func TestArgs(t *testing.T) {
	const badState = "crew: status state must be one of working|blocked|pr_open|done|failed|exited|reaped (got '%s')\n"
	cases := []struct {
		name string
		req  StatusRequest
		code int
		err  string
		body string
	}{
		{"restamp", StatusRequest{From: worker, State: "blocked", Detail: "why", Restamp: true}, 0, "", `{"state":"blocked","detail":"why","restamp":true}`},
		{"dashes in detail", StatusRequest{From: worker, State: "working", Detail: "--x"}, 0, "", `{"state":"working","detail":"--x"}`},
		{"restamp text in detail", StatusRequest{From: worker, State: "working", Detail: "--restamp"}, 0, "", `{"state":"working","detail":"--restamp"}`},
		{"single dash detail", StatusRequest{From: worker, State: "working", Detail: "-x"}, 0, "", `{"state":"working","detail":"-x"}`},
		{"invalid state", StatusRequest{From: worker, State: "bogus"}, 1, strings.Replace(badState, "%s", "bogus", 1), ""},
		{"no state", StatusRequest{From: worker}, 1, strings.Replace(badState, "%s", "", 1), ""},
		{"no args", StatusRequest{}, 1, strings.Replace(badState, "%s", "", 1), ""},
		{"restamp needs blocked", StatusRequest{From: worker, State: "working", Restamp: true}, 1, "crew: --restamp is only valid with blocked\n", ""},
		{"state checked before restamp", StatusRequest{From: worker, State: "nope", Restamp: true}, 1, strings.Replace(badState, "%s", "nope", 1), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.statusReq(tc.req)
			if code != tc.code || stderr != tc.err {
				t.Fatalf("got %d %q, want %d %q", code, stderr, tc.code, tc.err)
			}
			if tc.code != 0 {
				f.wantNoRow()
				return
			}
			f.wantRow(statusRow(worker, tc.body))
		})
	}
}

func TestRowValues(t *testing.T) {
	f := newFixture(t)
	if code, stderr := f.status(worker, "pr_open", "", "https://x/pull/1"); code != 0 {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(statusRow(worker, `{"state":"pr_open","pr_url":"https://x/pull/1"}`))
	if got := f.rawLog(); !strings.HasSuffix(got, "\n") {
		t.Errorf("row not newline-terminated: %q", got)
	}
}

func TestRowOmitsEmptyDetailAndPR(t *testing.T) {
	f := newFixture(t)
	f.status(worker, "working", "", "")
	f.wantRow(statusRow(worker, `{"state":"working"}`))
}

func TestRowDetailAndPR(t *testing.T) {
	f := newFixture(t)
	f.status(worker, "failed", "boom", "u")
	f.wantRow(statusRow(worker, `{"state":"failed","detail":"boom","pr_url":"u"}`))
}

// TestAcceptanceLedgerRidesInTheStatusDetail pins that the acceptance ledger a
// worker posts becomes the status row's detail byte for byte, with the
// dispatcher's waiver on the bus behind it.
func TestAcceptanceLedgerRidesInTheStatusDetail(t *testing.T) {
	const ledgerText = "AC1 pass(bats) AC2 waived(dispatcher)"
	f := newFixture(t)
	f.doc("tier: trivial\nkind: implement\nengine: claude\n")
	f.log(msgRow("dispatcher:c1", worker, "waive AC1 AC2 AC3"))
	if code, stderr := f.status(worker, "pr_open", ledgerText, "https://example.com/pr/1"); code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	got := f.lines()
	if len(got) != 2 {
		t.Fatalf("%d rows, want the waiver msg and the status row: %q", len(got), got)
	}
	if g, w := testjson.Compact(testjson.MustParse(t, got[1])),
		testjson.Compact(testjson.MustParse(t, statusRow(worker,
			`{"state":"pr_open","detail":"`+ledgerText+`","pr_url":"https://example.com/pr/1"}`))); g != w {
		t.Errorf("row:\n got  %s\n want %s", g, w)
	}
}

// The documented set is the accepted set: a worker posting any of these six
// states lands a row. `reaped` is the seventh state the refusal names, and a
// worker may not post it (TestReapedRefusedFromWorkerAndRole).
func TestEveryDocumentedStateIsAccepted(t *testing.T) {
	for _, state := range []string{"working", "blocked", "pr_open", "done", "failed", "exited"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			if code, stderr := f.status(worker, state); code != 0 || stderr != "" {
				t.Fatalf("code %d, stderr %q", code, stderr)
			}
			f.wantRow(statusRow(worker, `{"state":"`+state+`"}`))
		})
	}
}

// TestLongDetailElided posts a detail past the bus line cap: one line, at most
// bus.LineMax bytes, whose detail ends on the elision marker. The quote row is
// the nonlinear case — a `"` costs two bytes once the line is encoded, so a cut
// sized on the text's own length still overflows.
func TestLongDetailElided(t *testing.T) {
	for _, unit := range []string{"x", "é", "✓", `"`} {
		t.Run(unit, func(t *testing.T) {
			f := newFixture(t)
			if code, stderr := f.status(worker, "failed", strings.Repeat(unit, 6000)); code != 0 {
				t.Fatalf("code %d, stderr %q", code, stderr)
			}
			got := f.lines()
			if len(got) != 1 {
				t.Fatalf("%d rows, want 1", len(got))
			}
			if n := len(got[0]); n > bus.LineMax {
				t.Errorf("line is %d bytes, over the %d cap", n, bus.LineMax)
			}
			var row struct {
				Body struct {
					State  string `json:"state"`
					Detail string `json:"detail"`
				} `json:"body"`
			}
			if err := json.Unmarshal([]byte(got[0]), &row); err != nil {
				t.Fatalf("the clipped line is not one JSON value: %v", err)
			}
			if row.Body.State != "failed" || !strings.HasSuffix(row.Body.Detail, "[elided]") {
				t.Errorf("state %q, detail %d bytes, ends with elision: %v",
					row.Body.State, len(row.Body.Detail), strings.HasSuffix(row.Body.Detail, "[elided]"))
			}
		})
	}
}

// A refused --restamp appends nothing: the row already posted stays the log's
// last word, byte for byte.
func TestRefusedRestampWritesNothing(t *testing.T) {
	f := newFixture(t)
	if code, stderr := f.statusReq(StatusRequest{From: worker, State: "blocked", Detail: "why", Restamp: true}); code != 0 || stderr != "" {
		t.Fatalf("blocked --restamp: code %d, stderr %q", code, stderr)
	}
	before := f.rawLog()
	if code, _ := f.statusReq(StatusRequest{From: worker, State: "working", Restamp: true}); code != 1 {
		t.Fatalf("working --restamp: code %d, want 1", code)
	}
	if got := f.rawLog(); got != before {
		t.Errorf("the refused --restamp changed the log:\n got %s\nwant %s", got, before)
	}
}

func TestReapedRefusedFromWorkerAndRole(t *testing.T) {
	for _, from := range []string{worker, "role:feat/x:rev"} {
		f := newFixture(t)
		code, stderr := f.status(from, "reaped")
		if code == 0 || !strings.Contains(stderr, "posted only by crew reap") {
			t.Errorf("%s: code %d %q, want refusal", from, code, stderr)
		}
		if b, _ := os.ReadFile(f.paths.Log); len(b) != 0 {
			t.Errorf("%s: log not empty: %q", from, b)
		}
	}
}

func TestTerminalDedupe(t *testing.T) {
	prior := func(extra ...string) []string {
		return append([]string{statusRow(worker, `{"state":"pr_open","pr_url":"u1"}`)}, extra...)
	}
	cases := []struct {
		name    string
		rows    []string
		args    []string
		wantNew bool
	}{
		{"same state and pr", prior(), []string{worker, "pr_open", "", "u1"}, false},
		{"different pr", prior(), []string{worker, "pr_open", "", "u2"}, true},
		{"different from", prior(), []string{"worker:feat/y#s1-1", "pr_open", "", "u1"}, true},
		{"different state", prior(), []string{worker, "done", "", "u1"}, true},
		{"reaped deduped", []string{statusRow("dispatcher:c1", `{"state":"reaped"}`)}, []string{"dispatcher:c1", "reaped"}, false},
		{"reaped after done is new", []string{statusRow("dispatcher:c1", `{"state":"done"}`)}, []string{"dispatcher:c1", "reaped"}, true},
		{"working never deduped", []string{statusRow(worker, `{"state":"working"}`)}, []string{worker, "working"}, true},
		{"later row for the same from wins", prior(statusRow(worker, `{"state":"working"}`)), []string{worker, "pr_open", "", "u1"}, true},
		{"other kind ignored", prior(msgRow(worker, "x", "y")), []string{worker, "pr_open", "", "u1"}, false},
		{"no pr_url equals empty pr", []string{statusRow(worker, `{"state":"done"}`)}, []string{worker, "done"}, false},
		{"false pr_url is absent", []string{statusRow(worker, `{"state":"done","pr_url":false}`)}, []string{worker, "done"}, false},
		{"null pr_url is absent", []string{statusRow(worker, `{"state":"failed","pr_url":null}`)}, []string{worker, "failed"}, false},
		{"numeric pr_url renders as json", []string{statusRow(worker, `{"state":"exited","pr_url":12}`)}, []string{worker, "exited", "", "12"}, false},
		{"body without state renders null", []string{statusRow(worker, `{}`)}, []string{worker, "failed"}, true},
		{"torn tail keeps the prefix", prior(`{"ts":1,"crew_id":"c`), []string{worker, "pr_open", "", "u1"}, false},
		{"non-object rows skipped", prior(`5`, `"x"`, `[1]`, `null`), []string{worker, "pr_open", "", "u1"}, false},
		{"string body errors and is skipped", prior(statusRow(worker, `"x"`)), []string{worker, "pr_open", "", "u1"}, false},
		{"array body errors and is skipped", prior(statusRow(worker, `[1]`)), []string{worker, "pr_open", "", "u1"}, false},
		{"null body prints null and wins", prior(statusRow(worker, `null`)), []string{worker, "pr_open", "", "u1"}, true},
		{"row after a parse error is unread", []string{`garbage`, statusRow(worker, `{"state":"pr_open","pr_url":"u1"}`)}, []string{worker, "pr_open", "", "u1"}, true},
		{"newline in state: last line wins", []string{statusRow(worker, `{"state":"a\npr_open","pr_url":"u1"}`)}, []string{worker, "pr_open", "", "u1"}, false},
		{"other crew ignored", []string{`{"ts":1,"crew_id":"c2","from":"` + worker + `","kind":"status","body":{"state":"pr_open","pr_url":"u1"}}`}, []string{worker, "pr_open", "", "u1"}, true},
		{"missing from is not an empty from", []string{`{"ts":1,"crew_id":"c1","kind":"status","body":{"state":"done"}}`}, []string{"", "done"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.log(tc.rows...)
			if code, stderr := f.status(tc.args...); code != 0 {
				t.Fatalf("code %d %q", code, stderr)
			}
			want := 0
			if tc.wantNew {
				want = 1
			}
			if got := len(f.lines()) - len(tc.rows); got != want {
				t.Errorf("rows added = %d, want %d: %q", got, want, f.lines())
			}
		})
	}
}

func TestDedupeNoLog(t *testing.T) {
	f := newFixture(t)
	f.status(worker, "done")
	f.wantRow(statusRow(worker, `{"state":"done"}`))
}

// standardDoc is a standard-tier implement doc with no acceptance list, so the
// ledger passes on an empty detail and the seams are what decide.
const standardDoc = "tier: standard\nkind: implement\nengine: claude\n"

var (
	reviewSeam = msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full"}`)
	deslopSeam = msgRow(worker, "review:c1", `{"seam":"deslop"}`)
)

const planDoc = standardDoc + "plan: required\n"

var planSeam = msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`)

func noPlan(state, tier string) string {
	return "crew: refusing " + state + " for " + tier + ` session worker:feat/x#s1-1 — plan: required but no plan seam on the bus for this branch; run the plan critic (spec-plan-critic, or the plan-critic role pane), ingest its first verdict, then crew msg "$CREW_WORKER_ID" "review:c1" '{"seam":"plan","plan_critic_first_pass":"accept"}' (or revise/reject: the critic's first verdict) and retry. A plan phase you skipped or could not run is reported, not papered over: post crew status blocked naming why and await the dispatcher (it can re-dispatch with plan: provided); never post the seam without a critic verdict` + "\n"
}

const (
	noReviewTail = ` session worker:feat/x#s1-1 — no review seam on the bus for this branch; run the code review gate, ingest its verdict, then crew msg "$CREW_WORKER_ID" "review:c1" '{"seam":"review","review_mode":"full"}' (or downgraded) and retry; a review request or a pane that has not returned a verdict is not a review; a review that cannot run goes blocked/failed, never pr_open/done. On pi the reviewer pane's latest verdict decides: accept passes, revise needs your own review:c1 seam after you fix it, a reject (or any reply that is not an exact accept/revise) blocks until the reviewer's next verdict, and a re-request (any msg from you to the reviewer except the {"final":true} release) cancels every earlier verdict and your own earlier seam until a new verdict arrives` + "\n"
	noDeslopTail = ` session worker:feat/x#s1-1 — no deslop seam on the bus for this branch; run the harness deslop skill (dispatcher:deslop on claude, $deslop on codex, deslop on cursor and pi) over the diff you are about to push, commit its cleanup, then crew msg "$CREW_WORKER_ID" "review:c1" '{"seam":"deslop"}' and retry. The seam records that the skill ran — never post it just to get past this gate` + "\n"
)

func noReview(state, tier string) string {
	return "crew: refusing " + state + " for " + tier + noReviewTail
}
func noDeslop(state, tier string) string {
	return "crew: refusing " + state + " for " + tier + noDeslopTail
}

// noReviewFor is the review-seam refusal for a caller other than the fixture's
// worker: the line names the session, and the resumed and JSON-escaped branch
// cases post as another id.
func noReviewFor(state, tier, from string) string {
	return strings.ReplaceAll(noReview(state, tier), "session worker:feat/x#s1-1 ", "session "+from+" ")
}

func TestGatesSkipped(t *testing.T) {
	cases := []struct {
		name  string
		from  string
		doc   string
		noTop bool
	}{
		{"non-worker from", "lead:c1", standardDoc, false},
		{"no task doc", worker, "", false},
		{"no toplevel", worker, standardDoc, true},
	}
	for _, tc := range cases {
		for _, state := range []string{"pr_open", "done"} {
			t.Run(tc.name+" "+state, func(t *testing.T) {
				f := newFixture(t)
				f.noTop = tc.noTop
				if tc.doc != "" {
					f.doc(tc.doc)
				}
				if code, stderr := f.status(tc.from, state, "not a ledger"); code != 0 {
					t.Fatalf("%d %q", code, stderr)
				}
				f.wantRow(statusRow(tc.from, `{"state":"`+state+`","detail":"not a ledger"}`))
			})
		}
	}
}

func TestTaskDocSymlinkAndDirectory(t *testing.T) {
	t.Run("directory is not a regular file", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Mkdir(f.top+"/WORKER_TASK.md", 0o755); err != nil {
			t.Fatal(err)
		}
		if code, _ := f.status(worker, "pr_open", "x"); code != 0 {
			t.Errorf("code %d", code)
		}
	})
	t.Run("symlink is followed", func(t *testing.T) {
		f := newFixture(t)
		target := f.top + "/real.md"
		if err := os.WriteFile(target, []byte(standardDoc), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, f.top+"/WORKER_TASK.md"); err != nil {
			t.Fatal(err)
		}
		if code, stderr := f.status(worker, "done"); code != 1 || stderr != noReview("done", "standard") {
			t.Errorf("got %d %q", code, stderr)
		}
	})
}

func TestStandardImplementSeams(t *testing.T) {
	for _, state := range []string{"pr_open", "done"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			f.doc(standardDoc)

			if code, stderr := f.status(worker, state); code != 1 || stderr != noReview(state, "standard") {
				t.Errorf("no log: %d %q", code, stderr)
			}
			f.log(deslopSeam)
			if code, stderr := f.status(worker, state); code != 1 || stderr != noReview(state, "standard") {
				t.Errorf("deslop only: %d %q", code, stderr)
			}
			f.log(reviewSeam)
			if code, stderr := f.status(worker, state); code != 1 || stderr != noDeslop(state, "standard") {
				t.Errorf("review only: %d %q", code, stderr)
			}
			f.log(reviewSeam, deslopSeam)
			if code, stderr := f.status(worker, state); code != 0 {
				t.Fatalf("both: %d %q", code, stderr)
			}
			if got := f.lines(); len(got) != 3 {
				t.Errorf("row not appended: %q", got)
			}
		})
	}
}

func TestDeepTierIsGated(t *testing.T) {
	f := newFixture(t)
	f.doc("tier: deep\nkind: implement\n")
	if code, stderr := f.status(worker, "done"); code != 1 || stderr != noReview("done", "deep") {
		t.Errorf("%d %q", code, stderr)
	}
}

func TestSeamsKeyedOnBranchNotSession(t *testing.T) {
	f := newFixture(t)
	f.doc(standardDoc)
	f.log(
		msgRow("worker:feat/x#s0-9", "review:c1", `{"seam":"review","review_mode":"full"}`),
		msgRow("worker:feat/x#s0-9", "review:c1", `{"seam":"deslop"}`),
	)
	if code, stderr := f.status(worker, "done"); code != 0 {
		t.Errorf("earlier session's seams not honoured: %d %q", code, stderr)
	}
}

func TestDocWithoutKindLineIsImplement(t *testing.T) {
	f := newFixture(t)
	f.doc("tier: standard\nengine: claude\n")
	if code, stderr := f.status(worker, "pr_open"); code != 1 || stderr != noReview("pr_open", "standard") {
		t.Errorf("%d %q", code, stderr)
	}
}

func TestFieldParsing(t *testing.T) {
	// first matching line anywhere, whitespace removed, later lines ignored.
	f := newFixture(t)
	f.doc("# task\n\nbody\ntier:\t standard  \r\nkind:  imple ment\ntier: trivial\n")
	if code, stderr := f.status(worker, "done"); code != 1 || stderr != noReview("done", "standard") {
		t.Errorf("%d %q", code, stderr)
	}
	// an indented field line does not count.
	f = newFixture(t)
	f.doc(" tier: standard\nkind: implement\n")
	if code, stderr := f.status(worker, "done"); code != 0 {
		t.Errorf("indented tier gated: %d %q", code, stderr)
	}
}

// TestFieldLeadingUnicodeSpace pins `sed 's/^f:[[:space:]]*//'` under C.UTF-8:
// glibc's multibyte space class strips a leading U+3000 or U+2003, then
// `tr -d '[:space:]'` deletes ASCII whitespace only. NBSP is not a glibc space.
func TestFieldLeadingUnicodeSpace(t *testing.T) {
	t.Run("tier", func(t *testing.T) {
		for _, sp := range []string{"\u3000", "\u2003", " \u3000\t"} {
			f := newFixture(t)
			f.doc("tier:" + sp + "standard\nkind: implement\n")
			if code, stderr := f.status(worker, "done"); code != 1 || stderr != noReview("done", "standard") {
				t.Errorf("tier:%q: got %d %q", sp, code, stderr)
			}
		}
	})
	t.Run("tier NBSP is kept", func(t *testing.T) {
		f := newFixture(t)
		f.doc("tier:\u00a0standard\nkind: implement\n")
		if code, stderr := f.status(worker, "done"); code != 0 {
			t.Errorf("got %d %q", code, stderr)
		}
	})
	t.Run("tier trailing U+3000 is kept", func(t *testing.T) {
		f := newFixture(t)
		f.doc("tier: standard\u3000\nkind: implement\n")
		if code, stderr := f.status(worker, "done"); code != 0 {
			t.Errorf("got %d %q", code, stderr)
		}
	})
	t.Run("kind", func(t *testing.T) {
		f := newFixture(t)
		f.doc("tier: trivial\nkind:\u3000implement\n\n## Acceptance\n- AC1 x\n")
		if code, stderr := f.status(worker, "pr_open", "AC1 pending"); code != 1 || !strings.Contains(stderr, "acceptance") {
			t.Errorf("got %d %q", code, stderr)
		}
	})
	t.Run("engine", func(t *testing.T) {
		f := newFixture(t)
		f.doc("tier: standard\nkind: implement\nengine:\u3000pi\n")
		f.log(msgRow(rev, branch, `{"seam":"review","verdict":"accept"}`), deslopSeam)
		if code, stderr := f.status(worker, "done"); code != 0 {
			t.Errorf("pi accept not honoured: %d %q", code, stderr)
		}
	})
}

func TestTrivialTierNoSeamGate(t *testing.T) {
	f := newFixture(t)
	f.doc("tier: trivial\nkind: implement\n")
	if code, stderr := f.status(worker, "pr_open"); code != 0 {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(statusRow(worker, `{"state":"pr_open"}`))
}

func TestNoTierNoSeamGate(t *testing.T) {
	f := newFixture(t)
	f.doc("kind: implement\n")
	if code, stderr := f.status(worker, "done"); code != 0 {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestKindReviewSkipsLedgerAndSeams(t *testing.T) {
	f := newFixture(t)
	f.doc("tier: standard\nkind: review\n")
	if code, stderr := f.status(worker, "pr_open", "pending"); code != 0 {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(statusRow(worker, `{"state":"pr_open","detail":"pending"}`))
}

func TestLedgerRefusalSurfaced(t *testing.T) {
	const doc = "tier: trivial\nkind: implement\n\n## Acceptance\n- AC1 do it\n"
	cases := []struct{ name, detail string }{
		{"pending item", "AC1 pending"},
		{"empty with a list", ""},
		{"ci item without run id", "AC1 pass(CI green)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.doc(doc)
			want := ledger.Check(ledger.Input{From: worker, Crew: crewID, Detail: tc.detail, TaskDoc: doc})
			if want == "" {
				t.Fatal("fixture does not refuse")
			}
			if code, stderr := f.status(worker, "pr_open", tc.detail); code != 1 || stderr != want+"\n" {
				t.Errorf("got %d %q\nwant %q", code, stderr, want)
			}
			f.wantNoRow()
		})
	}
	t.Run("done skips the ledger", func(t *testing.T) {
		f := newFixture(t)
		f.doc(doc)
		if code, stderr := f.status(worker, "done", "AC1 pending"); code != 0 {
			t.Errorf("%d %q", code, stderr)
		}
	})
	t.Run("conforming ledger passes", func(t *testing.T) {
		f := newFixture(t)
		f.doc(doc)
		if code, stderr := f.status(worker, "pr_open", "AC1 pass(unit 3 of 3)", "u"); code != 0 {
			t.Errorf("%d %q", code, stderr)
		}
	})
	t.Run("ledger fires before the seam gate", func(t *testing.T) {
		f := newFixture(t)
		f.doc("tier: standard\nkind: implement\n")
		if code, stderr := f.status(worker, "pr_open", "junk"); code != 1 || !strings.Contains(stderr, "acceptance") {
			t.Errorf("%d %q", code, stderr)
		}
	})
	t.Run("waiver read from the log", func(t *testing.T) {
		f := newFixture(t)
		f.doc("tier: trivial\nkind: implement\n\n## Acceptance\n- AC1 x\n")
		f.log(msgRow("dispatcher:c1", worker, "waive AC1"))
		if code, stderr := f.status(worker, "pr_open", "AC1 waived(dispatcher)"); code != 0 {
			t.Errorf("%d %q", code, stderr)
		}
	})
}

func piDoc() string { return "tier: standard\nkind: implement\nengine: pi\n" }

func TestPiReviewerVerdicts(t *testing.T) {
	verdict := func(v string) string {
		return msgRow(rev+"#s1-1", branch, `{"seam":"review","verdict":"`+v+`"}`)
	}
	request := msgRow(branch, rev, `{"seam":"review","request":true}`)
	cases := []struct {
		name string
		rows []string
		pass bool
	}{
		{"accept passes without a lead seam", []string{verdict("accept"), deslopSeam}, true},
		{"revise without a lead seam is no seam", []string{verdict("revise"), deslopSeam}, false},
		{"revise then lead seam passes", []string{verdict("revise"), reviewSeam, deslopSeam}, true},
		{"reject blocks the lead seam", []string{verdict("reject"), reviewSeam, deslopSeam}, false},
		{"unknown verdict blocks", []string{verdict("maybe"), reviewSeam, deslopSeam}, false},
		{"reject then accept passes", []string{verdict("reject"), verdict("accept"), deslopSeam}, true},
		{"re-request voids an accept", []string{verdict("accept"), request, deslopSeam}, false},
		{"accept after the re-request passes", []string{request, verdict("accept"), deslopSeam}, true},
		{"final release does not void", []string{verdict("accept"), msgRow(branch, rev, `{"final":true}`), deslopSeam}, true},
		{"re-request voids the lead seam", []string{reviewSeam, request, deslopSeam}, false},
		{"reviewer note with a tag is ignored", []string{verdict("accept"), msgRow(rev, branch, `{"seam":"review","tag":"note"}`), deslopSeam}, true},
		{"role_exited is ignored", []string{verdict("accept"), msgRow(rev, branch, `{"event":"role_exited"}`), deslopSeam}, true},
		{"non-json reviewer reply rejects", []string{verdict("accept"), msgRow(rev, branch, `looks fine`), deslopSeam}, false},
		{"torn reviewer line rejects", []string{verdict("accept"), `{"crew_id":"c1","from":"` + rev}, false},
		{"accept body with a lone low surrogate passes", []string{msgRow(rev, branch, `{"seam":"review","verdict":"accept","note":"\udc00"}`), deslopSeam}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.doc(piDoc())
			f.log(tc.rows...)
			code, stderr := f.status(worker, "done")
			if tc.pass && code != 0 {
				t.Errorf("refused: %q", stderr)
			}
			if !tc.pass && (code != 1 || stderr != noReview("done", "standard")) {
				t.Errorf("got %d %q, want the review-seam refusal", code, stderr)
			}
		})
	}
	t.Run("claude ignores reviewer verdicts", func(t *testing.T) {
		f := newFixture(t)
		f.doc(standardDoc)
		f.log(verdict("accept"), deslopSeam)
		if code, stderr := f.status(worker, "done"); code != 1 || stderr != noReview("done", "standard") {
			t.Errorf("%d %q", code, stderr)
		}
	})
}

// TestSeamsCountLoneLowSurrogates pins that a lone low surrogate escape
// decodes to U+FFFD and the row still counts as a seam.
func TestSeamsCountLoneLowSurrogates(t *testing.T) {
	f := newFixture(t)
	f.doc(standardDoc)
	f.log(
		msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full","note":"\udc00"}`),
		msgRow(worker, "review:c1", `{"seam":"deslop","note":"\udc00"}`),
	)
	if code, stderr := f.status(worker, "done"); code != 0 {
		t.Errorf("refused: %q", stderr)
	}
}

func TestUnreadableLog(t *testing.T) {
	skipIfRoot(t)
	for _, state := range []string{"pr_open", "done"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			f.doc(standardDoc)
			f.log(reviewSeam, deslopSeam)
			if err := os.Chmod(f.paths.Log, 0); err != nil {
				t.Fatal(err)
			}
			want := "crew: refusing " + state + " for " + worker + " — could not read the crew log for the review seam (jq exit 2)\n"
			if code, stderr := f.status(worker, state); code != 1 || stderr != want {
				t.Errorf("got %d %q", code, stderr)
			}
		})
	}
}

func TestFoldErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct{ which, doc, failing string }{
		{"review", standardDoc, seamProg},
		{"deslop", standardDoc, deslopProg},
		{"plan", planDoc, planProg},
	} {
		t.Run(c.which, func(t *testing.T) {
			f := newFixture(t)
			f.doc(c.doc)
			f.log(reviewSeam, deslopSeam, planSeam)
			f.fold = func(prog string, lines []string, vars map[string]any) (any, error) {
				if prog == c.failing {
					return nil, boom
				}
				return Options{}.withDefaults().Fold(prog, lines, vars)
			}
			want := "crew: refusing pr_open for " + worker + " — could not read the crew log for the " + c.which + " seam (jq exit 5)\n"
			if code, stderr := f.status(worker, "pr_open"); code != 1 || stderr != want {
				t.Errorf("got %d %q", code, stderr)
			}
			if got := len(f.lines()); got != 3 {
				t.Errorf("log has %d rows, want 3", got)
			}
		})
	}
}

func TestPlanRequiredRefusedWithoutPlanSeam(t *testing.T) {
	for _, tier := range []string{"standard", "deep"} {
		for _, state := range []string{"pr_open", "done"} {
			t.Run(tier+"/"+state, func(t *testing.T) {
				f := newFixture(t)
				f.doc("tier: " + tier + "\nkind: implement\nengine: claude\nplan: required\n")
				f.log(reviewSeam, deslopSeam)
				if code, stderr := f.status(worker, state); code != 1 || stderr != noPlan(state, tier) {
					t.Errorf("%d %q", code, stderr)
				}
				if got := len(f.lines()); got != 2 {
					t.Errorf("log has %d rows, want 2", got)
				}
			})
		}
	}
}

func TestPlanSeamAccepted(t *testing.T) {
	for _, v := range []string{"accept", "revise", "reject"} {
		t.Run(v, func(t *testing.T) {
			f := newFixture(t)
			f.doc(planDoc)
			f.log(reviewSeam, deslopSeam, msgRow("worker:feat/x#s0-9", "review:c1", `{"seam":"plan","plan_critic_first_pass":"`+v+`"}`))
			if code, stderr := f.status(worker, "pr_open"); code != 0 {
				t.Fatalf("%d %q", code, stderr)
			}
			if got := len(f.lines()); got != 4 {
				t.Errorf("row not appended: %d rows", got)
			}
		})
	}
}

func TestPlanSeamAfterDispatch(t *testing.T) {
	f := newFixture(t)
	f.doc(planDoc)
	f.log(reviewSeam, deslopSeam, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x"}`, planSeam)
	if code, stderr := f.status(worker, "pr_open"); code != 0 {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestPlanSeamExemptions(t *testing.T) {
	resume := `{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/x","worker_id":"worker:feat/x#s2"}`
	for _, c := range []struct{ name, doc, extra string }{
		{"provided", standardDoc + "plan: provided\n", ""},
		{"no plan field", standardDoc, ""},
		{"trivial", "tier: trivial\nkind: implement\nengine: claude\nplan: required\n", ""},
		{"review kind", "tier: standard\nkind: review\nengine: claude\nplan: required\n", ""},
		{"resume in doc", planDoc + "resume: true\n", ""},
		{"bus resume row", planDoc, resume},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.doc(c.doc)
			rows := []string{reviewSeam, deslopSeam}
			if c.extra != "" {
				rows = append(rows, c.extra)
			}
			f.log(rows...)
			if code, stderr := f.status(worker, "pr_open"); code != 0 {
				t.Fatalf("%d %q", code, stderr)
			}
			if got := len(f.lines()); got != len(rows)+1 {
				t.Errorf("row not appended: %d rows", got)
			}
		})
	}
}

func TestPlanSeamRejectsNonEvidence(t *testing.T) {
	for _, c := range []struct{ name, row string }{
		{"no first pass", msgRow(worker, "review:c1", `{"seam":"plan"}`)},
		{"bad value", msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"maybe"}`)},
		{"tag", msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept","tag":"note"}`)},
		{"other branch", msgRow("worker:feat/y#s1-1", "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`)},
		{"other crew", `{"ts":1,"crew_id":"c2","from":"` + worker + `","to":"review:c2","kind":"msg","body":"{\"seam\":\"plan\",\"plan_critic_first_pass\":\"accept\"}"}`},
		{"to dispatcher", msgRow(worker, "dispatcher:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`)},
		{"resume other branch", `{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/y"}`},
		{"resume other crew", `{"ts":1,"crew_id":"c2","kind":"resume","branch":"feat/x"}`},
		{"resume then dispatch", `{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/x"}` + "\n" + `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x"}`},
		{"plan seam then dispatch", msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`) + "\n" + `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.doc(planDoc)
			f.log(reviewSeam, deslopSeam, c.row)
			if code, stderr := f.status(worker, "pr_open"); code != 1 || stderr != noPlan("pr_open", "standard") {
				t.Errorf("%d %q", code, stderr)
			}
		})
	}
}

func TestUnreadableTaskDoc(t *testing.T) {
	skipIfRoot(t)
	f := newFixture(t)
	f.doc(standardDoc)
	if err := os.Chmod(f.top+"/WORKER_TASK.md", 0); err != nil {
		t.Fatal(err)
	}
	want := "crew: refusing pr_open for " + worker + " — could not read the task doc's acceptance list\n"
	if code, stderr := f.status(worker, "pr_open", "AC1 pass(unit 3 of 3)"); code != 1 || stderr != want {
		t.Errorf("got %d %q", code, stderr)
	}
	// done has no ledger and, with every field unreadable, no seam gate.
	if code, stderr := f.status(worker, "done"); code != 0 {
		t.Errorf("done: %d %q", code, stderr)
	}
}

func TestPanePublish(t *testing.T) {
	long := strings.Repeat("é", 30) + strings.Repeat("✓", 20)
	display := "display-message -p -t %5 #{@crew_role}"
	set := func(opt, val string) string { return "set-option -p -t %5 " + opt + " " + val }

	t.Run("lead pane", func(t *testing.T) {
		f := newFixture(t)
		f.env["TMUX_PANE"] = "%5"
		f.role = "lead"
		if code, _ := f.status(worker, "blocked", long); code != 0 {
			t.Fatal(code)
		}
		want := []string{display, set("@crew_state", "blocked"), set("@crew_detail", strings.Repeat("é", 30)+strings.Repeat("✓", 10)), set("@crew_source", "")}
		if got := f.tm.Calls(); !slices.Equal(got, want) {
			t.Errorf("tmux calls %q, want %q", got, want)
		}
	})
	t.Run("detail is the original, not the shrunk one", func(t *testing.T) {
		f := newFixture(t)
		f.env["TMUX_PANE"] = "%5"
		f.role = "lead"
		f.status(worker, "working", strings.Repeat("x", 9000))
		want := []string{display, set("@crew_state", "working"), set("@crew_detail", strings.Repeat("x", 40)), set("@crew_source", "")}
		if got := f.tm.Calls(); !slices.Equal(got, want) {
			t.Errorf("tmux calls %q, want %q", got, want)
		}
	})
	skipped := []struct {
		name string
		prep func(f *fixture)
	}{
		{"CREW_ROLE_ID set", func(f *fixture) { f.env["CREW_ROLE_ID"] = "role:x"; f.env["TMUX_PANE"] = "%5"; f.role = "lead" }},
		{"no TMUX_PANE", func(f *fixture) { f.role = "lead" }},
		{"role pane", func(f *fixture) { f.env["TMUX_PANE"] = "%5"; f.role = "reviewer" }},
		{"no role option", func(f *fixture) { f.env["TMUX_PANE"] = "%5" }},
		{"no tmux", func(f *fixture) { f.env["TMUX_PANE"] = "%5"; f.role = "lead"; f.noTmux = true }},
	}
	for _, tc := range skipped {
		t.Run("skipped: "+tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.prep(f)
			if code, stderr := f.status(worker, "working", "d"); code != 0 || stderr != "" {
				t.Fatalf("code %d, stderr %q", code, stderr)
			}
			for _, c := range f.tm.Calls() {
				if strings.HasPrefix(c, "set-option") {
					t.Errorf("published: %q", c)
				}
			}
			// Skipping the pane mirror never skips the bus write it follows.
			f.wantRow(statusRow(worker, `{"state":"working","detail":"d"}`))
		})
	}
	t.Run("not on a refusal or a dedupe", func(t *testing.T) {
		f := newFixture(t)
		f.env["TMUX_PANE"] = "%5"
		f.role = "lead"
		f.log(statusRow(worker, `{"state":"done"}`))
		f.status(worker, "done")
		f.status(worker, "bogus")
		if got := f.tm.Calls(); len(got) != 0 {
			t.Errorf("tmux touched: %q", got)
		}
	})
}

// TestZeroOptionsGateTheCheckout pins that a zero Options reads the process's
// own checkout: a "" toplevel would skip every pr_open/done gate.
func TestZeroOptionsGateTheCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CREW_ID", "zero-opts")
	o := Options{}.withDefaults()
	top := git.Toplevel(context.Background(), git.Exec{}, ".")
	if top == "" {
		t.Skip("not run inside a git checkout")
	}
	if got := git.Toplevel(context.Background(), o.Git, "."); got != top {
		t.Errorf("Toplevel = %q, want %q", got, top)
	}
	if got, want := o.CrewID(), bus.CrewID(context.Background(), "."); got != want || got == "" {
		t.Errorf("CrewID() = %q, want %q", got, want)
	}
}

const (
	leadPane = "200\x1ffeat/x\x1flead\x1fc1\x1f@1\x1f%1\n"
	rolePane = "200\x1ffeat/x\x1freviewer\x1fc1\x1f@1\x1f%1\n"
)

// anchored places the caller under the pane with pid 200.
func (f *fixture) anchored(panes string) {
	f.panes = panes
	f.proc = proctest.New(nil, map[int]int{os.Getpid(): 200, 200: 1})
}

func TestStatusRefusesSenderAnchorMismatch(t *testing.T) {
	f := newFixture(t)
	f.anchored(rolePane)
	code, stderr := f.status(worker, "failed", "boom")
	if code != 1 || !strings.HasPrefix(stderr, "crew: status: refusing sender '"+worker+"' — ") {
		t.Errorf("got %d %q", code, stderr)
	}
	f.wantNoRow()
}

func TestStatusRefusesDispatcherFromWorkerEnv(t *testing.T) {
	f := newFixture(t)
	f.env["CREW_WORKER_ID"] = worker
	code, stderr := f.status("dispatcher:c1", "working")
	want := "crew: status: refusing sender 'dispatcher:c1' — " + sender.Check(t.Context(), "dispatcher:c1", crewID, f.opts().Sender) + "\n"
	if code != 1 || stderr != want {
		t.Errorf("got %d %q", code, stderr)
	}
	f.wantNoRow()
}

// TestGateReadsToplevelFromDir pins the exact argv the gate reads the
// toplevel with: `git -C <Dir> rev-parse --show-toplevel`.
func TestGateReadsToplevelFromDir(t *testing.T) {
	fake := &gittest.Fake{Out: map[string]string{}}
	g := gate{state: "pr_open", from: "worker:x", o: Options{Git: fake, Dir: "/some/dir"}.withDefaults()}
	g.check(context.Background())
	calls := fake.Calls()
	if len(calls) != 1 || calls[0] != "-C /some/dir rev-parse --show-toplevel" {
		t.Errorf("Calls() = %q, want [-C /some/dir rev-parse --show-toplevel]", calls)
	}
}

// phaseDoc is a task doc header naming the protocol dir and engine, and
// phaseFile creates a phase file in it.
func phaseDoc(dir, engine string) string {
	return "tier: standard\nkind: implement\nengine: " + engine + "\nprotocol_dir: " + dir + "\n"
}

func phaseFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("phase\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatusWorkingPrintsPhasePointer(t *testing.T) {
	stems := map[string]string{
		"spec": "WORKER_PLAN", "plan": "WORKER_PLAN",
		"execute": "WORKER_EXECUTE", "gate": "WORKER_EXECUTE",
		"review": "WORKER_REVIEW", "ship": "WORKER_SHIP",
	}
	suffixes := map[string]string{"claude": ".claude.md", "pi": ".pi.md", "codex": ".md"}
	for engine, suffix := range suffixes {
		for stage, stem := range stems {
			t.Run(engine+"/"+stage, func(t *testing.T) {
				f := newFixture(t)
				dir := t.TempDir()
				f.doc(phaseDoc(dir, engine))
				phaseFile(t, dir, stem+suffix)
				code, stderr := f.status(worker, "working", stage)
				if want := "crew: read " + dir + "/" + stem + suffix + " before this stage\n"; code != 0 || stderr != want {
					t.Errorf("got %d %q, want 0 %q", code, stderr, want)
				}
				f.wantRow(statusRow(worker, `{"state":"working","detail":`+jsonStr(stage)+`}`))
			})
		}
	}

	t.Run("first word decides", func(t *testing.T) {
		f := newFixture(t)
		dir := t.TempDir()
		f.doc(phaseDoc(dir, "claude"))
		phaseFile(t, dir, "WORKER_REVIEW.claude.md")
		code, stderr := f.status(worker, "working", "review fixes")
		if want := "crew: read " + dir + "/WORKER_REVIEW.claude.md before this stage\n"; code != 0 || stderr != want {
			t.Errorf("got %d %q, want 0 %q", code, stderr, want)
		}
		f.wantRow(statusRow(worker, `{"state":"working","detail":"review fixes"}`))
	})
}

func TestStatusNoPhasePointer(t *testing.T) {
	tests := []struct {
		name    string
		doc     func(dir string) string // nil: no task doc
		file    string                  // created in dir; "" none
		state   string
		detail  string
		restamp bool
		wantErr string // stderr when not empty (a refusal)
		wantRow bool
	}{
		{"file absent", func(d string) string { return phaseDoc(d, "claude") }, "", "working", "plan", false, "", true},
		{"no task doc", nil, "WORKER_PLAN.claude.md", "working", "plan", false, "", true},
		{"no protocol_dir", func(string) string { return "tier: standard\nkind: implement\nengine: claude\n" }, "WORKER_PLAN.claude.md", "working", "plan", false, "", true},
		{"no engine", func(d string) string { return "tier: standard\nprotocol_dir: " + d + "\n" }, "WORKER_PLAN.md", "working", "plan", false, "", true},
		{"blocked", func(d string) string { return phaseDoc(d, "claude") }, "WORKER_PLAN.claude.md", "blocked", "plan", false, "", true},
		{"unknown first word", func(d string) string { return phaseDoc(d, "claude") }, "WORKER_PLAN.claude.md", "working", "planning", false, "", true},
		{"case-sensitive", func(d string) string { return phaseDoc(d, "claude") }, "WORKER_PLAN.claude.md", "working", "Plan", false, "", true},
		{"empty detail", func(d string) string { return phaseDoc(d, "claude") }, "WORKER_PLAN.claude.md", "working", "", false, "", true},
		{"restamp", func(d string) string { return phaseDoc(d, "claude") }, "WORKER_PLAN.claude.md", "working", "plan", true, "crew: --restamp is only valid with blocked\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			if tt.doc != nil {
				f.doc(tt.doc(dir))
			}
			if tt.file != "" {
				phaseFile(t, dir, tt.file)
			}
			_, stderr := f.statusReq(StatusRequest{From: worker, State: tt.state, Detail: tt.detail, Restamp: tt.restamp})
			if stderr != tt.wantErr || strings.Contains(stderr, "crew: read") {
				t.Errorf("stderr %q, want %q", stderr, tt.wantErr)
			}
			if !tt.wantRow {
				f.wantNoRow()
			}
		})
	}

	t.Run("directory in place of the file", func(t *testing.T) {
		f := newFixture(t)
		dir := t.TempDir()
		f.doc(phaseDoc(dir, "claude"))
		if err := os.Mkdir(filepath.Join(dir, "WORKER_PLAN.claude.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		if code, stderr := f.status(worker, "working", "plan"); code != 0 || stderr != "" {
			t.Errorf("got %d %q", code, stderr)
		}
	})
}
