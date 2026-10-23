package status

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/ledger"
)

// gateCase is one `crew status` call against a seeded bus. A non-empty ledger is
// a substring of the ledger.Check refusal the case must produce (the want is
// computed from the same inputs); otherwise stderr is the exact refusal, or ""
// for a pass.
type gateCase struct {
	name, doc string
	rows      []string
	state     string
	detail    string
	code      int
	stderr    string
	ledger    string
}

const (
	reviewDoc = standardDoc
	acceptDoc = "tier: trivial\nkind: implement\n\n## Acceptance\n- AC1 do it\n"
	ciDoc     = "tier: trivial\nkind: implement\n\n## Acceptance\n- AC1 CI green\n"
)

// seamPass is a done call the gate lets through.
func seamPass(name, doc string, rows ...string) gateCase {
	return gateCase{name: name, doc: doc, rows: rows, state: "done"}
}

// seamRefuse is a done call the gate refuses with the exact stderr want.
func seamRefuse(name, doc, want string, rows ...string) gateCase {
	return gateCase{name: name, doc: doc, rows: rows, state: "done", code: 1, stderr: want}
}

func otherCrewMsg(from, to, body string) string {
	return `{"ts":1,"crew_id":"c2","from":"` + from + `","to":"` + to + `","kind":"msg","body":` + jsonStr(body) + `}`
}

func piVerdict(to, v string) string {
	return msgRow(rev+"#s1-1", to, `{"seam":"review","verdict":"`+v+`"}`)
}

func reviewCases() []gateCase {
	refused := noReview("done", "standard")
	reviewBody := `{"seam":"review","review_mode":"full"}`
	return []gateCase{
		// review seam, engine claude
		seamRefuse("claude: no log", reviewDoc, refused),
		seamPass("claude: seam full", reviewDoc, reviewSeam, deslopSeam),
		seamPass("claude: seam downgraded", reviewDoc,
			msgRow(worker, "review:c1", `{"seam":"review","review_mode":"downgraded"}`), deslopSeam),
		seamPass("claude: seam without review_mode", reviewDoc,
			msgRow(worker, "review:c1", `{"seam":"review"}`), deslopSeam),
		seamRefuse("claude: review_mode weird", reviewDoc, refused,
			msgRow(worker, "review:c1", `{"seam":"review","review_mode":"weird"}`), deslopSeam),
		seamRefuse("claude: review_mode null", reviewDoc, refused,
			msgRow(worker, "review:c1", `{"seam":"review","review_mode":null}`), deslopSeam),
		seamRefuse("claude: tag", reviewDoc, refused,
			msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full","tag":"note"}`), deslopSeam),
		seamRefuse("claude: to dispatcher", reviewDoc, refused,
			msgRow(worker, "dispatcher:c1", reviewBody), deslopSeam),
		seamRefuse("claude: other crew", reviewDoc, refused,
			otherCrewMsg(worker, "review:c2", reviewBody), deslopSeam),
		seamRefuse("claude: other branch", reviewDoc, refused,
			msgRow("worker:feat/y#s1-1", "review:c1", reviewBody), deslopSeam),
		seamRefuse("claude: from worker:feat/x#sY#Z is not branch feat/x", reviewDoc, refused,
			msgRow(otherSession, "review:c1", reviewBody), deslopSeam),
		seamPass("claude: earlier session counts", reviewDoc,
			msgRow("worker:feat/x#s0-9", "review:c1", reviewBody), deslopSeam),
		seamRefuse("claude: body not JSON", reviewDoc, refused,
			msgRow(worker, "review:c1", `plain text`), deslopSeam),
		seamRefuse("claude: body array", reviewDoc, refused,
			msgRow(worker, "review:c1", `[1,2]`), deslopSeam),
		seamRefuse("claude: reviewer accept ignored", reviewDoc, refused,
			piVerdict(branch, "accept"), deslopSeam),
		seamPass("claude: non-JSON reviewer reply does not revoke", reviewDoc,
			reviewSeam, msgRow(rev, branch, `looks fine`), deslopSeam),

		// review seam, engine pi: verdicts not pinned by TestPiReviewerVerdicts
		seamRefuse("pi: revise to another branch voids an accept", piDoc(), refused,
			piVerdict(branch, "accept"), piVerdict("worker:feat/y", "revise"), deslopSeam),
		seamRefuse("pi: accept to another branch does not grant", piDoc(), refused,
			piVerdict("worker:feat/y", "accept"), deslopSeam),
		seamRefuse("pi: accept to worker:feat/x#sY#Z does not grant", piDoc(), refused,
			piVerdict(otherSession, "accept"), deslopSeam),
		seamRefuse("pi: accept from reviewer#sY#Z does not grant", piDoc(), refused,
			msgRow(rev+"#sY#Z", branch, `{"seam":"review","verdict":"accept"}`), deslopSeam),
		seamRefuse("pi: reviewer verdict accept without seam", piDoc(), refused,
			msgRow(rev, branch, `{"verdict":"accept"}`), deslopSeam),
		seamPass("pi: tag with verdict accept still grants", piDoc(),
			msgRow(rev, branch, `{"seam":"review","tag":"note","verdict":"accept"}`), deslopSeam),
		seamRefuse("pi: re-request with extra final key voids", piDoc(), refused,
			piVerdict(branch, "accept"), msgRow(branch, rev, `{"final":true,"x":1}`), deslopSeam),
		seamRefuse("pi: re-request with final false voids", piDoc(), refused,
			piVerdict(branch, "accept"), msgRow(branch, rev, `{"final":false}`), deslopSeam),
		seamRefuse("pi: BOM re-request after accept", piDoc(), refused,
			piVerdict(branch, "accept"), bomRequest, deslopSeam),
	}
}

func deslopCases() []gateCase {
	refused := noDeslop("done", "standard")
	deslopBody := `{"seam":"deslop"}`
	return []gateCase{
		// deslop seam
		seamPass("deslop: seam", standardDoc, reviewSeam, deslopSeam),
		seamPass("deslop: earlier session counts", standardDoc,
			reviewSeam, msgRow("worker:feat/x#s0-9", "review:c1", deslopBody)),
		seamPass("deslop: found after unrelated and torn lines", standardDoc,
			reviewSeam, `{"crew_id":"c1","from":"`+worker, statusRow(worker, `{"state":"working"}`), deslopSeam),
		seamRefuse("deslop: no row", standardDoc, refused, reviewSeam),
		seamRefuse("deslop: tag", standardDoc, refused,
			reviewSeam, msgRow(worker, "review:c1", `{"seam":"deslop","tag":"note"}`)),
		seamRefuse("deslop: to review:c2", standardDoc, refused,
			reviewSeam, msgRow(worker, "review:c2", deslopBody)),
		seamRefuse("deslop: other crew", standardDoc, refused,
			reviewSeam, otherCrewMsg(worker, "review:c2", deslopBody)),
		seamRefuse("deslop: other branch", standardDoc, refused,
			reviewSeam, msgRow("worker:feat/y#s1-1", "review:c1", deslopBody)),
		seamRefuse("deslop: from worker:feat/x#sY#Z is not branch feat/x", standardDoc, refused,
			reviewSeam, msgRow(otherSession, "review:c1", deslopBody)),
		seamRefuse("deslop: body not JSON", standardDoc, refused,
			reviewSeam, msgRow(worker, "review:c1", `deslop`)),
		seamRefuse("deslop: other seam", standardDoc, refused,
			reviewSeam, msgRow(worker, "review:c1", `{"seam":"review"}`)),
		seamPass("deslop: body with a lone low surrogate", standardDoc,
			reviewSeam, msgRow(worker, "review:c1", `{"seam":"deslop","note":"\udc00"}`)),
	}
}

func planCases() []gateCase {
	refused := noPlan("done", "standard")
	base := []string{reviewSeam, deslopSeam}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	planBody := func(v string) string { return `{"seam":"plan","plan_critic_first_pass":"` + v + `"}` }
	dispatchOther := `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/y"}`
	dispatchOtherCrew := `{"ts":2,"crew_id":"c2","kind":"dispatch","branch":"feat/x"}`
	return []gateCase{
		// plan seam (plan: required)
		seamPass("plan: seam accept", planDoc, with(msgRow(worker, "review:c1", planBody("accept")))...),
		seamPass("plan: seam revise", planDoc, with(msgRow(worker, "review:c1", planBody("revise")))...),
		seamPass("plan: seam reject", planDoc, with(msgRow(worker, "review:c1", planBody("reject")))...),
		seamPass("plan: seam from an earlier session", planDoc, with(msgRow("worker:feat/x#s0-9", "review:c1", planBody("accept")))...),
		seamRefuse("plan: no seam", planDoc, refused, base...),
		seamRefuse("plan: missing first pass", planDoc, refused,
			with(msgRow(worker, "review:c1", `{"seam":"plan"}`))...),
		seamRefuse("plan: first pass maybe", planDoc, refused,
			with(msgRow(worker, "review:c1", planBody("maybe")))...),
		seamRefuse("plan: tag", planDoc, refused,
			with(msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept","tag":"note"}`))...),
		seamRefuse("plan: other branch", planDoc, refused,
			with(msgRow("worker:feat/y#s1-1", "review:c1", planBody("accept")))...),
		seamRefuse("plan: from worker:feat/x#sY#Z is not branch feat/x", planDoc, refused,
			with(msgRow(otherSession, "review:c1", planBody("accept")))...),
		seamRefuse("plan: other crew", planDoc, refused,
			with(otherCrewMsg(worker, "review:c2", planBody("accept")))...),
		seamRefuse("plan: to dispatcher", planDoc, refused,
			with(msgRow(worker, "dispatcher:c1", planBody("accept")))...),
		seamRefuse("plan: body not JSON", planDoc, refused,
			with(msgRow(worker, "review:c1", `plan accepted`))...),

		// plan seam against dispatch and resume rows
		seamPass("plan: seam after dispatch", planDoc, with(dispatchRow, planSeam)...),
		seamRefuse("plan: dispatch after seam", planDoc, refused, with(planSeam, dispatchRow)...),
		seamPass("plan: resume after dispatch", planDoc, with(dispatchRow, resumeRow)...),
		seamRefuse("plan: resume then dispatch", planDoc, refused, with(resumeRow, dispatchRow)...),
		seamPass("plan: dispatch for another branch does not reset", planDoc, with(planSeam, dispatchOther)...),
		seamPass("plan: dispatch for another crew does not reset", planDoc, with(planSeam, dispatchOtherCrew)...),
		seamRefuse("plan: resume for another branch", planDoc, refused,
			with(`{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/y"}`)...),
		seamRefuse("plan: resume for another crew", planDoc, refused,
			with(`{"ts":1,"crew_id":"c2","kind":"resume","branch":"feat/x"}`)...),
		seamPass("plan: torn dispatcher msg naming crew, dispatch and branch does not reset", planDoc,
			with(planSeam, tornDispatchr)...),

		// plan exemptions
		seamPass("plan: provided is exempt", standardDoc+"plan: provided\n", base...),
		seamPass("plan: absent is exempt", standardDoc, base...),
		seamPass("plan: resume true in the doc is exempt", planDoc+"resume: true\n", base...),
		seamPass("plan: trivial tier is exempt", "tier: trivial\nkind: implement\nengine: claude\nplan: required\n"),
		seamPass("plan: review kind is exempt", "tier: standard\nkind: review\nengine: claude\nplan: required\n"),

		// seam order: review, then deslop, then plan
		seamRefuse("order: nothing posted", planDoc, noReview("done", "standard")),
		seamRefuse("order: deslop only", planDoc, noReview("done", "standard"), deslopSeam),
		seamRefuse("order: review only", planDoc, noDeslop("done", "standard"), reviewSeam),
		seamRefuse("order: review and deslop", planDoc, refused, base...),
	}
}

// ledgerCases are pr_open handoffs: the ledger gate runs first, then the seams.
func ledgerCases() []gateCase {
	waiverDoc := "tier: trivial\nkind: implement\n\n## Acceptance\n- AC1 x\n"
	const waived = "AC1 waived(dispatcher)"
	const hint = "every acceptance ledger item must read"
	const waiverHint = "needs a dispatcher waiver on the bus"
	const ciHint = "is a CI item"
	refuse := func(name, doc, detail, wantIn string, rows ...string) gateCase {
		return gateCase{name: name, doc: doc, rows: rows, state: "pr_open", detail: detail, code: 1, ledger: wantIn}
	}
	pass := func(name, doc, detail string, rows ...string) gateCase {
		return gateCase{name: name, doc: doc, rows: rows, state: "pr_open", detail: detail}
	}
	return []gateCase{
		// ledger handoff
		refuse("pending item", acceptDoc, "AC1 pending", hint),
		refuse("empty detail with an acceptance list", acceptDoc, "", "the task doc has an acceptance list"),
		pass("conforming pass", acceptDoc, "AC1 pass(unit 3 of 3)"),
		{name: "done skips the ledger", doc: acceptDoc, state: "done", detail: "AC1 pending"},

		// waiver from the dispatcher
		pass("waiver reply matches", waiverDoc, waived, msgRow("dispatcher:c1", worker, "waive AC1")),
		refuse("no waiver on the bus", waiverDoc, waived, waiverHint),
		refuse("negated waiver does not count", waiverDoc, waived, waiverHint,
			msgRow("dispatcher:c1", worker, "will not waive AC1")),
		refuse("waiver from a non-dispatcher sender", waiverDoc, waived, waiverHint,
			msgRow("lead:c1", worker, "waive AC1")),
		refuse("waiver to an earlier session", waiverDoc, waived, waiverHint,
			msgRow("dispatcher:c1", "worker:feat/x#s0-9", "waive AC1")),
		refuse("waiver naming another item", waiverDoc, waived, waiverHint,
			msgRow("dispatcher:c1", worker, "waive AC2")),

		// CI item: needs a run id
		refuse("CI evidence without a run id", acceptDoc, "AC1 pass(CI green)", ciHint),
		refuse("CI entry in the doc without a run id", ciDoc, "AC1 pass(green)", ciHint),
		refuse("CI run id with fewer than six digits", acceptDoc, "AC1 pass(CI run id 12345)", ciHint),
		pass("CI evidence with an actions/runs URL", acceptDoc, "AC1 pass(CI actions/runs/123456)"),
		pass("CI evidence with a run id", acceptDoc, "AC1 pass(CI run id 123456)"),
		pass("CI entry in the doc with a run id", ciDoc, "AC1 pass(actions/runs/123456)"),
		pass("CIA is not a CI item", acceptDoc, "AC1 pass(CIA ok)"),

		// ledger before the seams
		refuse("ledger fires before the seams", standardDoc+"\n## Acceptance\n- AC1 do it\n", "AC1 pending", hint),
		{name: "ledger passes, then the review seam", doc: standardDoc + "\n## Acceptance\n- AC1 do it\n",
			state: "pr_open", detail: "AC1 pass(unit 3 of 3)", code: 1, stderr: noReview("pr_open", "standard")},
		{name: "ledger passes, then the deslop seam", doc: standardDoc + "\n## Acceptance\n- AC1 do it\n",
			rows: []string{reviewSeam}, state: "pr_open", detail: "AC1 pass(unit 3 of 3)", code: 1, stderr: noDeslop("pr_open", "standard")},
		pass("ledger and seams pass", standardDoc+"\n## Acceptance\n- AC1 do it\n", "AC1 pass(unit 3 of 3)", reviewSeam, deslopSeam),
	}
}

// ledgerTruthCases pins the ledger gate's decisions: every ledger the grammar
// refuses exits 1 with the ledger hint and writes no status row, and every
// ledger it accepts is written with an empty stderr. The doc has no acceptance
// list, so no seam gates apply and only the ledger decides.
func ledgerTruthCases() []gateCase {
	const noListDoc = "tier: trivial\nkind: implement\nengine: claude\n"
	const deepDoc = "tier: deep\nkind: implement\nengine: claude\n"
	const hint = "every acceptance ledger item"
	waived := msgRow("dispatcher:c1", worker, "waive AC1 AC2 AC3")
	refuse := func(name, detail string) gateCase {
		return gateCase{name: name, doc: noListDoc, state: "pr_open", detail: detail, code: 1, ledger: hint}
	}
	pass := func(name, detail string, rows ...string) gateCase {
		return gateCase{name: name, doc: noListDoc, rows: rows, state: "pr_open", detail: detail}
	}
	cases := []gateCase{
		// the refused truth table
		refuse("truth: skipped with a note", "AC1 pass(bats); AC2 skipped (not run: no simulator)"),
		refuse("truth: parenthesized state word", "AC2 (pending on device)"),
		refuse("truth: deferred", "AC3 deferred (not done yet)"),
		refuse("truth: a whole ledger in parens", "(AC2 pending)"),
		refuse("truth: pending with evidence", "AC2 pending(sim)"),
		refuse("truth: pass then pending", "AC1 pass(bats); AC2 pending(sim)"),
		refuse("truth: NOT DONE", "AC3 NOT DONE"),
		refuse("truth: not run", "AC4 not run"),
		refuse("truth: waived after pending", "AC1 pending(sim); AC2 waived(dispatcher)"),
		refuse("truth: items glued by a space", "AC1 pass(x) AC2 pending"),
		refuse("truth: an unbalanced paren", "AC1 pass(x AC2 pending(sim)"),
		refuse("truth: a note after waived", "AC2 waived(dispatcher) not run on CI"),
		refuse("truth: pending between two passes", "AC1 pass(x) AC2 pending AC3 pass(y)"),
		refuse("truth: a semicolon-led pending", "AC1 pass(x); AC2 pending; AC3 pass(y)"),
		refuse("truth: blank parens", "AC2 pass()"),
		refuse("truth: waived(self)", "AC2 waived(self)"),
		refuse("truth: waived(low risk)", "AC2 waived(low risk)"),
		refuse("truth: n/a", "AC2 n/a(no ui)"),
		refuse("truth: partial", "A7b partial(flag yes, viewer unit-only)"),
		refuse("truth: free text pointing at the PR", "acceptance ledger: all pass, see PR body/comment"),
		refuse("truth: an extra closing paren", "AC1 pass(x)) AC2 pending"),
		refuse("truth: a trailing id with no item", "AC1 pass(x); AC2"),
		refuse("truth: passed is not pass", "AC1 passed (no pending migrations)"),
		refuse("truth: a parenthesized note after waived", "AC2 waived(dispatcher) (not run on CI)"),

		// the accepted truth table
		pass("truth accepted: pass and waived", "AC1 pass(bats) AC2 waived(dispatcher)", waived),
		pass("truth accepted: the word pending inside evidence", "AC5 pass(bats: pending ledger refused)"),
		pass("truth accepted: PASS is the keyword", "PASS(ci pending)"),
		pass("truth accepted: run twice is no run id", "AC1 pass(nix build (x86) not run twice)"),
		pass("truth accepted: pending deep inside nested evidence", "AC1 pass(bats: 3 pending sub-tests skipped (see log (details))); AC2 pass(nix build)"),
		pass("truth accepted: a waived item's note", "AC2 waived(dispatcher: not run on CI)", waived),
		pass("truth accepted: empty detail without a list", ""),
		pass("truth accepted: whitespace-only detail", "   "),
		pass("truth accepted: the protocol-shaped ledger", "AC1 pass(bats); AC2 pass(nix build); AC3 waived(dispatcher)", waived),
		pass("truth accepted: a trailing semicolon", "AC1 pass(bats); AC2 pass(nix build); AC3 waived(dispatcher);", waived),
		pass("truth accepted: a run id and a comma", "AC1-3 pass(vitest), AC4 pass(test+emu log)"),
		pass("truth accepted: a colon after the id", "AC1: pass(bats)"),
		pass("truth accepted: WAIVED(Dispatcher)", "AC2 WAIVED(Dispatcher)", waived),
	}
	// the ledger is checked before the review seam, on every gated tier
	return append(cases,
		gateCase{name: "known gap: a lone state word reads as the next id and writes", doc: noListDoc,
			state: "pr_open", detail: "AC1 pass(x) pending pass(y)"},
		gateCase{name: "mid-sentence mentions leave the empty detail written", doc: "tier: trivial\nkind: implement\nengine: claude\n\nSee the acceptance list in the issue for the details.\nacceptance is mentioned here in passing.\n",
			state: "pr_open", detail: ""},
		gateCase{name: "a heading that only resembles Acceptance leaves the empty detail written", doc: "tier: trivial\nkind: implement\nengine: claude\n\n### **Accepted**\n- one\n",
			state: "pr_open", detail: ""},
		gateCase{name: "deep: the ledger fires before the seams", doc: deepDoc, state: "pr_open",
			detail: "AC2 pending(sim)", code: 1, ledger: hint},
		gateCase{name: "deep: ledger and seams pass", doc: deepDoc, rows: []string{reviewSeam, deslopSeam},
			state: "pr_open", detail: "AC1 pass(bats)"},
	)
}

// TestLedgerSkippedForDoneReviewKindAndRole: done calls, a kind: review task
// doc and role callers are never ledger-checked, and each of the five calls
// writes its row.
func TestLedgerSkippedForDoneReviewKindAndRole(t *testing.T) {
	f := newFixture(t)
	f.doc("tier: trivial\nkind: implement\nengine: claude\n")
	for _, args := range [][]string{
		{"worker:feat/x#s1-1", "done", "AC2 pending(sim)", "https://example.com/pr/1"},
		{"worker:feat/x#s2-2", "done", "follow-ups: #312 (upstream fix pending)"},
		{"role:feat/x:reviewer", "pr_open", "AC2 pending", "https://example.com/pr/1"},
	} {
		if code, stderr := f.status(args...); code != 0 || stderr != "" {
			t.Fatalf("status %v: %d %q", args, code, stderr)
		}
	}
	f.doc("tier: trivial\nkind: review\nengine: claude\n")
	for _, args := range [][]string{
		{"worker:feat/x#s3-3", "pr_open", "2 findings pending", "https://example.com/pr/1"},
		{"worker:feat/x#s3-3", "done", "2 findings pending author reply", "https://example.com/pr/1"},
	} {
		if code, stderr := f.status(args...); code != 0 || stderr != "" {
			t.Fatalf("status %v: %d %q", args, code, stderr)
		}
	}
	if got := f.lines(); len(got) != 5 {
		t.Errorf("%d rows, want 5: %q", len(got), got)
	}
}

func TestGateCases(t *testing.T) {
	groups := []struct {
		name  string
		cases []gateCase
	}{
		{"review", reviewCases()},
		{"deslop", deslopCases()},
		{"plan", planCases()},
		{"ledger", ledgerCases()},
		{"truth", ledgerTruthCases()},
	}
	for _, g := range groups {
		for _, tc := range g.cases {
			t.Run(g.name+"/"+tc.name, func(t *testing.T) {
				f := newFixture(t)
				f.doc(tc.doc)
				f.log(tc.rows...)
				before := len(f.lines())

				want := tc.stderr
				if tc.ledger != "" {
					in := ledger.Input{From: worker, Crew: crewID, Detail: tc.detail, TaskDoc: tc.doc}
					if len(tc.rows) > 0 {
						in.Log = tc.rows
					}
					line := ledger.Check(in)
					if !strings.Contains(line, tc.ledger) {
						t.Fatalf("ledger.Check = %q, want it to contain %q", line, tc.ledger)
					}
					want = line + "\n"
				}

				code, stderr := f.status(worker, tc.state, tc.detail)
				if code != tc.code || stderr != want {
					t.Errorf("got %d %q\nwant %d %q", code, stderr, tc.code, want)
				}
				got := f.lines()
				switch {
				case tc.code == 0 && len(got) != before+1:
					t.Errorf("row not appended: %d rows, want %d", len(got), before+1)
				case tc.code == 0 && !strings.Contains(got[len(got)-1], `"state":"`+tc.state+`"`):
					t.Errorf("last row is not the %s status: %q", tc.state, got[len(got)-1])
				case tc.code != 0 && len(got) != before:
					t.Errorf("refused call appended a row: %q", got[before:])
				}
			})
		}
	}
}
