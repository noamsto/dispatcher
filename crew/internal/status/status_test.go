package status

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/ledger"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

const (
	crewID = "c1"
	worker = "worker:feat/x#s1-1"
	branch = "worker:feat/x"
	rev    = "role:feat/x:reviewer"
	tsText = `1700000000500`
)

type foldFunc = func(prog string, rows []jsonv.Value, vars map[string]jsonv.Value) (jsonv.Value, error)

// fixture is a checkout with a bus: Toplevel and the bus paths are plain temp
// dirs, so no git is involved.
type fixture struct {
	t      *testing.T
	top    string
	paths  bus.Paths
	env    map[string]string
	tmux   [][]string // argv of every tmux call
	role   string     // stdout of the @crew_role query
	noTmux bool
	noTop  bool
	fold   foldFunc
	crew   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		t:     t,
		top:   root,
		paths: bus.Paths{Common: root + "/.git", Dir: root + "/.git/crew", Log: root + "/.git/crew/events.jsonl"},
		env:   map[string]string{},
		crew:  crewID,
	}
}

func (f *fixture) opts() Options {
	o := Options{
		CrewID:   func() string { return f.crew },
		Toplevel: func() string { return f.top },
		NowMS:    func() int64 { return 1700000000500 },
		Getenv:   func(k string) string { return f.env[k] },
		Fold:     f.fold,
	}
	if f.noTop {
		o.Toplevel = func() string { return "" }
	}
	if !f.noTmux {
		o.Tmux = func(args ...string) (string, error) {
			f.tmux = append(f.tmux, args)
			if args[0] == "display-message" {
				return f.role, nil
			}
			return "", nil
		}
	}
	return o
}

func (f *fixture) status(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := RunStatus(args, f.paths, &stderr, f.opts())
	return code, stderr.String()
}

func (f *fixture) msg(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := RunMsg(args, f.paths, &stderr, f.opts())
	return code, stderr.String()
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

func jsonStr(s string) string { return string(jsonv.Append(nil, jsonv.Str(s), jsonv.Options{})) }

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
	const badState = "crew: status state must be one of working|blocked|pr_open|done|failed|exited (got '%s')\n"
	cases := []struct {
		name string
		args []string
		code int
		err  string
		body string
	}{
		{"restamp first", []string{"--restamp", worker, "blocked", "why"}, 0, "", `{"state":"blocked","detail":"why","restamp":true}`},
		{"restamp middle", []string{worker, "--restamp", "blocked", "why"}, 0, "", `{"state":"blocked","detail":"why","restamp":true}`},
		{"restamp last", []string{worker, "blocked", "why", "--restamp"}, 0, "", `{"state":"blocked","detail":"why","restamp":true}`},
		{"dashes after --", []string{worker, "working", "--", "--x"}, 0, "", `{"state":"working","detail":"--x"}`},
		{"everything after -- positional", []string{"--", worker, "working", "--restamp"}, 0, "", `{"state":"working","detail":"--restamp"}`},
		{"unknown arg", []string{worker, "working", "--x"}, 1, "crew: status: unknown arg '--x' (use -- before a detail that starts with dashes)\n", ""},
		{"unknown arg beats state", []string{worker, "bogus", "--force"}, 1, "crew: status: unknown arg '--force' (use -- before a detail that starts with dashes)\n", ""},
		{"single dash is positional", []string{worker, "working", "-x"}, 0, "", `{"state":"working","detail":"-x"}`},
		{"invalid state", []string{worker, "bogus"}, 1, strings.Replace(badState, "%s", "bogus", 1), ""},
		{"no state", []string{worker}, 1, strings.Replace(badState, "%s", "", 1), ""},
		{"no args", nil, 1, strings.Replace(badState, "%s", "", 1), ""},
		{"restamp needs blocked", []string{worker, "working", "--restamp"}, 1, "crew: --restamp is only valid with blocked\n", ""},
		{"state checked before restamp", []string{worker, "nope", "--restamp"}, 1, strings.Replace(badState, "%s", "nope", 1), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.status(tc.args...)
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

func TestLongDetailElided(t *testing.T) {
	f := newFixture(t)
	f.status(worker, "working", strings.Repeat("é", 6000))
	got := f.lines()
	if len(got) != 1 || len(got[0]) > bus.LineMax || !strings.Contains(got[0], "…[elided]") {
		t.Errorf("row not fitted: %d rows", len(got))
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
		// jq's fromjson refuses a lone high surrogate escape, so the reply is not an accept.
		{"accept body with a lone high surrogate rejects", []string{msgRow(rev, branch, `{"seam":"review","verdict":"accept","note":"\ud83d"}`), deslopSeam}, false},
		{"accept body with a lone low surrogate passes", []string{msgRow(rev, branch, `{"seam":"review","verdict":"accept","note":"\udc00"}`), deslopSeam}, true},
		{"reviewer line with a lone high surrogate rejects", []string{verdict("accept"), deslopSeam,
			`{"ts":1,"crew_id":"c1","from":"` + rev + `","to":"` + branch + `","kind":"msg","body":"{\"seam\":\"review\",\"verdict\":\"accept\"}","x":"\ud83d"}`}, false},
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

// TestSeamsRefuseLoneHighSurrogates pins jq's fromjson, which errors on a high
// surrogate escape not followed by a low one (in the line or in the msg body),
// so the row is no seam; a lone low surrogate decodes to U+FFFD and counts.
func TestSeamsRefuseLoneHighSurrogates(t *testing.T) {
	outer := func(body string) string {
		return `{"ts":1,"crew_id":"c1","from":"` + worker + `","to":"review:c1","kind":"msg","body":` + jsonStr(body) + `,"x":"\ud83d"}`
	}
	cases := []struct {
		name string
		rows []string
		want string
	}{
		{"deslop body", []string{reviewSeam, msgRow(worker, "review:c1", `{"seam":"deslop","note":"\ud83d"}`)}, noDeslop("done", "standard")},
		{"deslop body, high then non-low escape", []string{reviewSeam, msgRow(worker, "review:c1", `{"seam":"deslop","note":"\ud83d\u0041"}`)}, noDeslop("done", "standard")},
		{"deslop line", []string{reviewSeam, outer(`{"seam":"deslop"}`)}, noDeslop("done", "standard")},
		{"review body", []string{msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full","note":"\ud83d"}`), deslopSeam}, noReview("done", "standard")},
		{"review line", []string{outer(`{"seam":"review","review_mode":"full"}`), deslopSeam}, noReview("done", "standard")},
		{"lone low surrogates count", []string{
			msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full","note":"\udc00"}`),
			msgRow(worker, "review:c1", `{"seam":"deslop","note":"\udc00"}`),
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.doc(standardDoc)
			f.log(tc.rows...)
			code, stderr := f.status(worker, "done")
			if tc.want == "" && code != 0 {
				t.Errorf("refused: %q", stderr)
			}
			if tc.want != "" && (code != 1 || stderr != tc.want) {
				t.Errorf("got %d %q\nwant %q", code, stderr, tc.want)
			}
		})
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
			f.fold = func(prog string, rows []jsonv.Value, vars map[string]jsonv.Value) (jsonv.Value, error) {
				if prog == c.failing {
					return jsonv.Value{}, boom
				}
				return Options{}.withDefaults().Fold(prog, rows, vars)
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
	display := []string{"display-message", "-p", "-t", "%5", "#{@crew_role}"}
	set := func(opt, val string) []string { return []string{"set-option", "-p", "-t", "%5", opt, val} }

	t.Run("lead pane", func(t *testing.T) {
		f := newFixture(t)
		f.env["TMUX_PANE"] = "%5"
		f.role = "lead\n"
		if code, _ := f.status(worker, "blocked", long); code != 0 {
			t.Fatal(code)
		}
		want := [][]string{display, set("@crew_state", "blocked"), set("@crew_detail", strings.Repeat("é", 30)+strings.Repeat("✓", 10)), set("@crew_source", "")}
		if len(f.tmux) != len(want) {
			t.Fatalf("tmux calls %q", f.tmux)
		}
		for i := range want {
			if strings.Join(f.tmux[i], "\x1f") != strings.Join(want[i], "\x1f") {
				t.Errorf("call %d: %q, want %q", i, f.tmux[i], want[i])
			}
		}
	})
	t.Run("detail is the original, not the shrunk one", func(t *testing.T) {
		f := newFixture(t)
		f.env["TMUX_PANE"] = "%5"
		f.role = "lead"
		f.status(worker, "working", strings.Repeat("x", 9000))
		if got := f.tmux[2][5]; got != strings.Repeat("x", 40) {
			t.Errorf("detail %q", got)
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
			if code, _ := f.status(worker, "working", "d"); code != 0 {
				t.Fatal(code)
			}
			for _, c := range f.tmux {
				if c[0] == "set-option" {
					t.Errorf("published: %q", c)
				}
			}
		})
	}
	t.Run("not on a refusal or a dedupe", func(t *testing.T) {
		f := newFixture(t)
		f.env["TMUX_PANE"] = "%5"
		f.role = "lead"
		f.log(statusRow(worker, `{"state":"done"}`))
		f.status(worker, "done")
		f.status(worker, "bogus")
		if len(f.tmux) != 0 {
			t.Errorf("tmux touched: %q", f.tmux)
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
	top := bus.Toplevel(context.Background(), ".")
	if top == "" {
		t.Skip("not run inside a git checkout")
	}
	if got := o.Toplevel(); got != top {
		t.Errorf("Toplevel() = %q, want %q", got, top)
	}
	if got, want := o.CrewID(), bus.CrewID(context.Background(), "."); got != want || got == "" {
		t.Errorf("CrewID() = %q, want %q", got, want)
	}
}
