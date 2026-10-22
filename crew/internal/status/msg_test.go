package status

import (
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

func msgWant(from, to, body string) string {
	return `{"ts":` + tsText + `,"crew_id":"c1","from":"` + from + `","to":"` + to + `","kind":"msg","body":` + jsonStr(body) + `}`
}

func TestMsgCrewIDUnset(t *testing.T) {
	f := newFixture(t)
	f.crew = ""
	code, stderr := f.msg("a", "b", "c")
	if code != 1 || stderr != "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n" {
		t.Errorf("got %d %q", code, stderr)
	}
}

func TestMsgRow(t *testing.T) {
	f := newFixture(t)
	if code, stderr := f.msg(worker, "dispatcher:c1", "hello"); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(msgWant(worker, "dispatcher:c1", "hello"))
}

func TestMsgMissingArgs(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.msg(); code != 0 {
		t.Fatal(code)
	}
	f.wantRow(msgWant("", "", ""))
}

func TestMsgJSONBodyStaysText(t *testing.T) {
	f := newFixture(t)
	const body = `{"seam":"review","review_mode":"full"}`
	f.msg(worker, "review:c1", body)
	f.wantRow(msgWant(worker, "review:c1", body))
}

func TestMsgEmptyIDRecipient(t *testing.T) {
	for _, to := range []string{"dispatcher:", "role:", "worker:", "review:"} {
		t.Run(to, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.msg(worker, to, "x")
			if code != 1 || stderr != "crew: msg recipient '"+to+"' is missing an id after the colon\n" {
				t.Errorf("got %d %q", code, stderr)
			}
			f.wantNoRow()
		})
	}
}

func TestMsgRoleControlBytes(t *testing.T) {
	refusal := func(to string) string {
		return "crew: msg: refusing to send to '" + to + "': a role assignment body must be one line, but this one contains a control character (newline/tab/…); re-send it as compact JSON, e.g. jq -c\n"
	}
	cases := []struct {
		name, to, body string
		refused        bool
	}{
		{"newline", "role:a:impl", "a\nb", true},
		{"tab", "role:a:impl", "a\tb", true},
		{"escape", "role:a:impl", "a\x1bb", true},
		{"DEL", "role:a:impl", "a\x7fb", true},
		{"unit separator", "role:a:impl", "a\x1fb", true},
		{"e acute", "role:a:impl", "café", false},
		{"space and printable", "role:a:impl", `{"a": "b~"}`, false},
		{"newline to a non-role", "dispatcher:c1", "a\nb", false},
		{"newline to a worker", "worker:feat/x", "a\nb", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.msg(worker, tc.to, tc.body)
			if tc.refused {
				if code != 1 || stderr != refusal(tc.to) {
					t.Errorf("got %d %q", code, stderr)
				}
				f.wantNoRow()
				return
			}
			if code != 0 || stderr != "" {
				t.Fatalf("got %d %q", code, stderr)
			}
			f.wantRow(msgWant(worker, tc.to, tc.body))
		})
	}
}

func TestMsgLongBodyElided(t *testing.T) {
	for _, unit := range []string{"x", "é", "✓"} {
		t.Run(unit, func(t *testing.T) {
			f := newFixture(t)
			if code, _ := f.msg(worker, "dispatcher:c1", strings.Repeat(unit, 9000)); code != 0 {
				t.Fatal(code)
			}
			got := f.lines()
			if len(got) != 1 || len(got[0]) > bus.LineMax {
				t.Fatalf("rows %d, first len %d", len(got), len(got[0]))
			}
			if !strings.Contains(got[0], "…[elided]") {
				t.Errorf("no elision marker: %.80q", got[0])
			}
		})
	}
}

func TestMsgNeverPublishesPane(t *testing.T) {
	f := newFixture(t)
	f.env["TMUX_PANE"] = "%5"
	f.role = "lead"
	f.msg(worker, "dispatcher:c1", "hi")
	if got := f.tm.Calls(); len(got) != 0 {
		t.Errorf("tmux touched: %q", got)
	}
}

func TestMsgRefusesDispatcherFromWorkerOrRoleEnv(t *testing.T) {
	for key, val := range map[string]string{"CREW_WORKER_ID": worker, "CREW_ROLE_ID": rev} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			f.env[key] = val
			code, stderr := f.msg("dispatcher:c1", worker, "stop")
			want := "crew: msg: refusing sender 'dispatcher:c1' — " + sender.Check(t.Context(), "dispatcher:c1", crewID, f.opts().Sender) + "\n"
			if code != 1 || stderr != want {
				t.Errorf("got %d %q", code, stderr)
			}
			f.wantNoRow()
		})
	}
}

func TestMsgStampedLeadPanePostsOnlyAsLead(t *testing.T) {
	f := newFixture(t)
	f.anchored(leadPane)
	code, stderr := f.msg(rev, "dispatcher:c1", "hi")
	if code != 1 || !strings.HasPrefix(stderr, "crew: msg: refusing sender '"+rev+"' — ") {
		t.Errorf("got %d %q", code, stderr)
	}
	f.wantNoRow()

	if code, stderr := f.msg(worker, "dispatcher:c1", "hi"); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(msgWant(worker, "dispatcher:c1", "hi"))
}

// noPlanMsg is the review-seam refusal `crew msg` posts under `plan: required`
// when the branch has no plan seam: the same hint the pr_open/done gate gives.
const noPlanMsg = "crew: msg: refusing the review seam for standard session " + worker +
	` — plan: required but no plan seam on the bus for this branch; run the plan critic (spec-plan-critic, or the plan-critic role pane), ingest its first verdict, then crew msg "$CREW_WORKER_ID" "review:c1" '{"seam":"plan","plan_critic_first_pass":"accept"}' (or revise/reject: the critic's first verdict) and retry. A plan phase you skipped or could not run is reported, not papered over: post crew status blocked naming why and await the dispatcher (it can re-dispatch with plan: provided); never post the seam without a critic verdict` + "\n"

func TestMsgReviewSeamPlanGate(t *testing.T) {
	const review = `{"seam":"review","review_mode":"full"}`
	const deslop = `{"seam":"deslop"}`
	const plan = `{"seam":"plan","plan_critic_first_pass":"accept"}`
	planRow := msgRow(worker, "review:c1", plan)

	cases := []struct {
		name, doc, to, body string
		rows                []string
		code                int
	}{
		// AC1: plan: required with no plan seam refuses the review seam.
		{"required: no seam", planDoc, "review:c1", review, nil, 1},
		{"required: rows, none a plan seam", planDoc, "review:c1", review,
			[]string{msgRow(worker, "review:c1", deslop), statusRow(worker, `{"state":"working"}`)}, 1},
		{"required: plan seam for another branch", planDoc, "review:c1", review,
			[]string{msgRow("worker:feat/y#s1-1", "review:c1", plan)}, 1},
		{"required: plan seam for another crew", planDoc, "review:c1", review,
			[]string{otherCrewMsg(worker, "review:c2", plan)}, 1},
		{"required: plan seam before the latest dispatch", planDoc, "review:c1", review,
			[]string{planRow, dispatchRow}, 1},
		{"required: seam-shaped body that is not a plan seam", planDoc, "review:c1", review,
			[]string{msgRow(worker, "review:c1", `{"seam":"plan"}`)}, 1},
		{"required: tagged plan seam", planDoc, "review:c1", review,
			[]string{msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept","tag":"note"}`)}, 1},
		{"required: plan seam to dispatcher", planDoc, "review:c1", review,
			[]string{msgRow(worker, "dispatcher:c1", plan)}, 1},
		// seam.jq reads a body through `_jqfromjson`, which skips a leading byte
		// order mark: the same body is a review seam to both gates.
		{"required: BOM-prefixed review seam", planDoc, "review:c1", "\ufeff" + review, nil, 1},
		{"required: BOM-prefixed review seam, plan seam posted", planDoc, "review:c1", "\ufeff" + review,
			[]string{planRow}, 0},
		// And one parser: `_jqfromjson` keeps a number outside float64's range as
		// a json.Number, where encoding/json would have rejected the body.
		{"required: out-of-range number in the body", planDoc, "review:c1", `{"seam":"review","n":1e400}`, nil, 1},
		{"required: out-of-range number, plan seam posted", planDoc, "review:c1", `{"seam":"review","n":1e400}`,
			[]string{planRow}, 0},

		// AC2: a plan seam on an earlier session of the branch counts.
		{"required: plan seam this session", planDoc, "review:c1", review, []string{planRow}, 0},
		{"required: plan seam earlier session", planDoc, "review:c1", review,
			[]string{msgRow("worker:feat/x#s0-9", "review:c1", plan)}, 0},
		{"required: plan seam after this dispatch", planDoc, "review:c1", review,
			[]string{dispatchRow, planRow}, 0},
		{"required: resume of the branch", planDoc, "review:c1", review,
			[]string{dispatchRow, resumeRow}, 0},
		{"required: resume true in the doc", planDoc + "resume: true\n", "review:c1", review, nil, 0},

		// AC3: everything else posts as before.
		{"provided", standardDoc + "plan: provided\n", "review:c1", review, nil, 0},
		{"plan absent", standardDoc, "review:c1", review, nil, 0},
		{"required: deslop seam unaffected", planDoc, "review:c1", deslop, nil, 0},
		{"required: plan seam unaffected", planDoc, "review:c1", plan, nil, 0},
		{"required: plain msg unaffected", planDoc, "dispatcher:c1", "plan critic said accept", nil, 0},
		{"required: body not JSON unaffected", planDoc, "review:c1", "review done", nil, 0},
		{"required: body is an array unaffected", planDoc, "review:c1", `[1,2]`, nil, 0},
		{"required: another seam unaffected", planDoc, "review:c1", `{"seam":"other"}`, nil, 0},
		{"required: review seam to another recipient unaffected", planDoc, "review:c2", review, nil, 0},
		{"required: trivial tier exempt", "tier: trivial\nkind: implement\nengine: claude\nplan: required\n", "review:c1", review, nil, 0},
		{"required: review kind exempt", "tier: standard\nkind: review\nengine: claude\nplan: required\n", "review:c1", review, nil, 0},
		{"required: no task doc", "", "review:c1", review, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.doc != "" {
				f.doc(tc.doc)
			}
			f.log(tc.rows...)
			before := len(f.lines())

			code, stderr := f.msg(worker, tc.to, tc.body)
			want := ""
			if tc.code != 0 {
				want = noPlanMsg
			}
			if code != tc.code || stderr != want {
				t.Errorf("got %d %q\nwant %d %q", code, stderr, tc.code, want)
			}
			if tc.code == 0 {
				if tc.doc == "" {
					f.wantRow(msgWant(worker, tc.to, tc.body))
					return
				}
				got := f.lines()
				if len(got) != before+1 {
					t.Errorf("row not appended: %d rows, want %d", len(got), before+1)
				}
				return
			}
			if got := f.lines(); len(got) != before {
				t.Errorf("refused call appended a row: %q", got[before:])
			}
		})
	}
}

// TestMsgReviewSeamUnreadableLogFailsClosed pins that a crew log the fold cannot
// read refuses the review seam rather than waving it through.
func TestMsgReviewSeamUnreadableLogFailsClosed(t *testing.T) {
	skipIfRoot(t)
	f := newFixture(t)
	f.doc(planDoc)
	f.log(msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`))
	if err := os.Chmod(f.paths.Log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.paths.Log, 0o644) })

	code, stderr := f.msg(worker, "review:c1", `{"seam":"review"}`)
	if code != 1 || !strings.HasPrefix(stderr, "crew: msg: refusing the review seam for standard session "+worker+" — could not read the crew log for the plan seam") {
		t.Fatalf("got %d %q", code, stderr)
	}
}
