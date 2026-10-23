package status

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
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
	from      string // the status caller; empty is the fixture's worker
}

// caller is the session id the case posts its status as.
func (c gateCase) caller() string {
	if c.from != "" {
		return c.from
	}
	return worker
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

// piCases ports the pi reviewer-verdict cases of tests/crew.bats' terminal
// status gate section (#241, #306, #391, #392): each row is one `crew status
// <worker> pr_open ""` over a seeded bus, asserted with its exact exit code and
// refusal, and with the status row written or absent. The engine line decides
// whether a pane verdict counts as a seam, so a row seeds the tier and engine
// it needs.
func piCases() []gateCase {
	// verdict is a reviewer-pane verdict msg as the pi grid posts it.
	verdict := func(to, v string) string {
		return msgRow(rev, to, `{"role":"reviewer","seam":"review","verdict":"`+v+`"}`)
	}
	// request is the lead's review re-request: any msg to the reviewer that is
	// not the bare {"final":true} release, whether or not it carries seam keys.
	request := msgRow(worker, rev, `{"seam":"review","artifact":"/a/review.diff","question":"Review this diff."}`)
	const (
		deepPiDoc = "tier: deep\nkind: implement\nengine: pi\n"
		resumed   = "worker:feat/x#s2-2"
		escaped   = `worker:feat/a"b#s1-1`
	)
	refuse := func(name string, rows ...string) gateCase {
		return gateCase{name: name, doc: piDoc(), rows: rows, state: "pr_open", code: 1, stderr: noReview("pr_open", "standard")}
	}
	allow := func(name string, rows ...string) gateCase {
		return gateCase{name: name, doc: piDoc(), rows: rows, state: "pr_open"}
	}
	// deepPi gates the resumed session, so its refusal names that id.
	deepPi := func(name string, code int, rows ...string) gateCase {
		want := ""
		if code != 0 {
			want = noReviewFor("pr_open", "deep", resumed)
		}
		return gateCase{name: name, doc: deepPiDoc, rows: rows, state: "pr_open", from: resumed, code: code, stderr: want}
	}
	refuseClaude := func(name string, rows ...string) gateCase {
		return gateCase{name: name, doc: standardDoc, rows: rows, state: "pr_open", code: 1, stderr: noReview("pr_open", "standard")}
	}
	allowClaude := func(name string, rows ...string) gateCase {
		return gateCase{name: name, doc: standardDoc, rows: rows, state: "pr_open"}
	}
	// The crash-splice fixtures, byte-exact: a hard kill mid-append leaves a line
	// with no newline, so the next record splices onto it and one unparsable line
	// carries a whole valid record (#391). The reader's fail-closed content
	// heuristic has to unescape both ids to see the reject inside it.
	const (
		splicedReject = `torn{"ts":1700000000500,"crew_id":"c1","from":"role:feat/x:reviewer",` +
			`"to":"worker:feat/x#s1-1","kind":"msg","body":"{\"role\":\"reviewer\",\"seam\":\"review\",\"verdict\":\"reject\"}"}`
		splicedRequest = `torn{"ts":1700000000500,"crew_id":"c1","from":"worker:feat/x#s1-1",` +
			`"to":"role:feat/x:reviewer","kind":"msg","body":"{\"seam\":\"review\",\"artifact\":\"/a/review.diff\",\"question\":\"Review this diff.\"}"}`
		escapedAccept = `{"ts":1,"crew_id":"c1","from":"role:feat/a\"b:reviewer","to":"worker:feat/a\"b#s1-1",` +
			`"kind":"msg","body":"{\"seam\":\"review\",\"verdict\":\"accept\"}"}`
		escapedSplicedReject = `torn{"ts":1700000000500,"crew_id":"c1","from":"role:feat/a\"b:reviewer",` +
			`"to":"worker:feat/a\"b#s1-1","kind":"msg","body":"{\"seam\":\"review\",\"verdict\":\"reject\"}"}`
		// An unrelated record spliced onto a torn line: the spliced row names this
		// crew but never the reviewer, and the second torn line names the reviewer
		// but another crew, so neither reads as a verdict.
		splicedUnrelated = `torn{"ts":1,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"msg","body":"{\"note\":\"x\"}"}`
		tornOtherCrew    = `torn{{"ts":1,"crew_id":"other","from":"role:feat/x:reviewer","to":"worker:feat/x","kind":"msg","body":"{}"}`
	)

	// The rows are named after the bats case each replaces. A comment over two or
	// more rows marks one folded bats family.
	return []gateCase{
		allow("accept alone counts as a review seam", verdict(worker, "accept"), deslopSeam),

		// a pi lone verdict that is not an accept to this worker is refused
		refuse("lone revise to this worker is refused", verdict(worker, "revise")),
		refuse("lone reject to this worker is refused", verdict(worker, "reject")),
		refuse("lone accept addressed to the dispatcher is refused", verdict("dispatcher:c1", "accept")),
		refuse("lone accept addressed to another branch's worker is refused", verdict("worker:feat/other#s1-1", "accept")),

		refuse("reject is not cleared by the lead's own review seam", verdict(worker, "reject"), reviewSeam),
		allow("reject is cleared by a later accept", verdict(worker, "reject"), verdict(worker, "accept"), deslopSeam),
		allow("reject then revise then the lead's own review seam is allowed",
			verdict(worker, "reject"), verdict(worker, "revise"), reviewSeam, deslopSeam),
		allow("revise then the lead's own review seam is allowed", verdict(worker, "revise"), reviewSeam, deslopSeam),
		allow("revise then a later accept is allowed", verdict(worker, "revise"), verdict(worker, "accept"), deslopSeam),

		// a pi accept then a non-accept verdict is refused
		refuse("accept followed by a later reject is refused", verdict(worker, "accept"), verdict(worker, "reject")),
		refuse("accept then a reject addressed to the dispatcher is refused", verdict(worker, "accept"), verdict("dispatcher:c1", "reject")),
		refuse("accept then a wrong-case Reject verdict is refused", verdict(worker, "accept"), verdict(worker, "Reject")),

		refuse("accept is voided when the review is re-requested", verdict(worker, "accept"), request),
		allow("accept after the review was re-requested is allowed",
			verdict(worker, "accept"), request, verdict(worker, "accept"), deslopSeam),
		refuse("re-requested review is not satisfied by the lead's own review seam",
			verdict(worker, "accept"), request, reviewSeam),
		refuse("re-request answered by an elided or invalid verdict is refused",
			verdict(worker, "accept"), request, msgRow(rev, worker, `{"role":"reviewer","seam":"review"}`), verdict(worker, "maybe")),

		// a resumed session inherits the latest pi verdict, accept or reject
		deepPi("resumed session inherits an accept verdict", 0, verdict(worker, "accept"), deslopSeam),
		deepPi("resumed session inherits a later reject verdict", 1, verdict(worker, "accept"), verdict(worker, "reject")),

		// a torn trailing line does not change the pi verdict outcome
		allow("torn trailing line after an accept changes nothing", verdict(worker, "accept"), deslopSeam, `torn{`),
		refuse("torn trailing line after a reject changes nothing", verdict(worker, "reject"), `torn{`),

		refuse("reject then a revise addressed to the dispatcher stays refused",
			verdict(worker, "reject"), verdict("dispatcher:c1", "revise"), reviewSeam),

		// a pi accept then one non-verdict message is refused
		refuse("accept then an unparseable lead assignment to the reviewer is refused",
			verdict(worker, "accept"), msgRow(worker, rev, `not json`)),
		refuse("accept then a markdown-fenced reviewer reject is refused",
			verdict(worker, "accept"), msgRow(rev, worker, "```json {\"seam\":\"review\",\"verdict\":\"reject\"} ```")),
		refuse("accept then a reviewer msg whose body is a JSON array is refused",
			verdict(worker, "accept"), msgRow(rev, worker, `[{"seam":"review","verdict":"reject"}]`)),
		refuse("accept then a reject carrying a tag key is refused",
			verdict(worker, "accept"), msgRow(rev, worker, `{"role":"reviewer","seam":"review","verdict":"reject","tag":"x"}`)),
		refuse("accept then a final release that also carries a question is refused",
			verdict(worker, "accept"), msgRow(worker, rev, `{"final":true,"question":"re-review please"}`)),

		// a pi accept survives two non-cancelling bus objects
		allow("accept survives role_exited and final objects on the bus",
			verdict(worker, "accept"), msgRow(rev, worker, `{"event":"role_exited"}`),
			msgRow(worker, "dispatcher:c1", `{"final":true}`), deslopSeam),
		allow("accept survives a lead release that carries only final",
			verdict(worker, "accept"), msgRow(worker, rev, `{"final":true}`),
			msgRow(worker, "role:feat/other:reviewer", `{"question":"another branch"}`), deslopSeam),

		// a cancelling message after a pi accept is refused until a fresh accept
		refuse("accept then a reviewer object with neither seam nor verdict is refused",
			verdict(worker, "accept"), msgRow(rev, worker, `{"decision":"reject"}`)),
		allow("a reviewer object with neither seam nor verdict is cleared by a fresh accept",
			verdict(worker, "accept"), msgRow(rev, worker, `{"decision":"reject"}`), verdict(worker, "accept"), deslopSeam),
		refuse("accept then a lead question to the reviewer is refused",
			verdict(worker, "accept"), msgRow(worker, rev, `{"question":"look again please"}`)),
		allow("a lead question to the reviewer is cleared by a fresh accept",
			verdict(worker, "accept"), msgRow(worker, rev, `{"question":"look again please"}`), verdict(worker, "accept"), deslopSeam),

		// one message that is not a pi review seam is refused
		refuse("a reviewer object with neither seam nor verdict alone is refused", msgRow(rev, worker, `{"decision":"accept"}`)),
		refuse("a reviewer retro-style note is not a review seam", msgRow(rev, worker, `{"seam":"review","tag":"other","detail":"x"}`)),

		// a pi accept survives one non-cancelling message
		allow("accept survives a reviewer role_exited event with no seam or verdict",
			verdict(worker, "accept"), msgRow(rev, worker, `{"role":"reviewer","event":"role_exited","pane":"%3","detail":"engine exited before a verdict"}`), deslopSeam),
		allow("accept survives a reviewer tag-only note with no seam",
			verdict(worker, "accept"), msgRow(rev, worker, `{"tag":"other","detail":"x"}`), deslopSeam),
		allow("a lead final release to the reviewer does not cancel an accept",
			verdict(worker, "accept"), msgRow(worker, rev, `{"final":true}`), deslopSeam),
		allow("accept survives a reviewer retro-style note",
			verdict(worker, "accept"), msgRow(rev, worker, `{"seam":"review","tag":"other","detail":"x"}`), deslopSeam),

		allow("accept then a mis-addressed accept still stands",
			verdict(worker, "accept"), verdict("dispatcher:c1", "accept"), deslopSeam),
		allow("accept survives a numeric-from object line from another crew",
			verdict(worker, "accept"), `{"ts":1,"crew_id":"other","from":7,"to":8,"kind":"msg","body":"{}"}`, deslopSeam),
		refuse("reject then a new assignment then the lead's own seam is refused",
			verdict(worker, "reject"), request, reviewSeam),

		// a pi lead seam with review_mode false or null is refused
		refuse("lead seam with review_mode false is refused", msgRow(worker, "review:c1", `{"seam":"review","review_mode":false}`)),
		refuse("lead seam with review_mode null is refused", msgRow(worker, "review:c1", `{"seam":"review","review_mode":null}`)),

		allowClaude("off pi a re-assignment does not cancel the lead's own seam", request, reviewSeam, deslopSeam),

		// a later lead-to-reviewer message cancels the lead seam
		refuse("assignment with an elided artifact still cancels the lead's seam",
			reviewSeam, msgRow(worker, rev, `{"artifact":"…[elided]"}`)),
		refuse("lead question to the reviewer cancels the lead's own review seam",
			reviewSeam, msgRow(worker, rev, `{"question":"look again please"}`)),

		allow("scalar JSON lines in the log are tolerated", verdict(worker, "accept"), `42`, `"x"`, `null`, deslopSeam),

		// a pi accept then a record spliced onto a torn line: only a torn line that
		// still reads as this crew's reviewer cancels the accept.
		refuse("accept then a reject spliced onto a torn line is refused", verdict(worker, "accept"), splicedReject),
		allow("a fresh accept after a spliced reject is allowed",
			verdict(worker, "accept"), splicedReject, verdict(worker, "accept"), deslopSeam),
		refuse("accept then a torn line cut inside a reviewer reply is refused",
			verdict(worker, "accept"), `{"ts":1,"crew_id":"c1","from":"role:feat/x:reviewer","to":"wor`),
		refuse("accept then a lead assignment spliced onto a torn line is refused",
			verdict(worker, "accept"), splicedRequest),
		allow("accept survives an unrelated record spliced onto a torn line",
			verdict(worker, "accept"), splicedUnrelated, tornOtherCrew, deslopSeam),

		// the branch name carries a quote, so the heuristic compares escaped ids
		gateCase{name: "torn-line reject is detected for a JSON-escaped reviewer id",
			doc: piDoc(), rows: []string{escapedAccept, escapedSplicedReject}, state: "pr_open",
			from: escaped, code: 1, stderr: noReviewFor("pr_open", "standard", escaped)},

		// off pi a pane verdict is no seam at all, and the lead's own seam stands
		refuseClaude("off pi a pane reject alone is refused", verdict(worker, "reject")),
		refuseClaude("off pi a pane reject then a pane accept is refused",
			verdict(worker, "reject"), verdict(worker, "accept")),
		allowClaude("off pi the lead's own review seam is allowed even after a pane reject",
			verdict(worker, "reject"), reviewSeam, deslopSeam),
	}
}

// TestGatePiOversizedReviewerReply pins that an elided reviewer reply is no
// verdict: the reply is long enough that the writer truncates the line, so the
// row on the bus carries no verdict, and a reviewer reply that is not a verdict
// cancels the earlier accept. The reply goes through RunMsg because the writer's
// elision is what the case is about.
func TestGatePiOversizedReviewerReply(t *testing.T) {
	findings := make([]string, 400)
	for i := range findings {
		findings[i] = `"finding number ` + fmt.Sprint(i) + ` with some text"`
	}
	big := `{"role":"reviewer","seam":"review","verdict":"accept","findings":[` + strings.Join(findings, ",") + `]}`

	f := newFixture(t)
	f.doc(piDoc())
	f.log(msgRow(rev, worker, `{"role":"reviewer","seam":"review","verdict":"accept","findings":[],"evidence":"x"}`))
	if code, stderr := f.msg(rev, worker, big); code != 0 || stderr != "" {
		t.Fatalf("msg: %d %q", code, stderr)
	}

	// The written row parses, but its body no longer reads as a verdict: the
	// elision cut the JSON mid-array.
	lines := f.lines()
	if len(lines) != 2 {
		t.Fatalf("%d rows, want 2: %q", len(lines), lines)
	}
	var row struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &row); err != nil {
		t.Fatalf("the elided row does not parse: %v\n%s", err, lines[1])
	}
	if v, err := jqrun.FromJSON(row.Body); err == nil {
		if o, ok := v.(map[string]any); ok && (o["verdict"] == "accept" || o["seam"] == "review") {
			t.Errorf("the elided body still reads as a verdict: %v", o)
		}
	}

	if code, stderr := f.status(worker, "pr_open"); code != 1 || stderr != noReview("pr_open", "standard") {
		t.Errorf("got %d %q, want the review-seam refusal", code, stderr)
	}
	if got := f.lines(); len(got) != 2 {
		t.Errorf("the refusal wrote a row: %q", got[2:])
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
		{"pi", piCases()},
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
					in := ledger.Input{From: tc.caller(), Crew: crewID, Detail: tc.detail, TaskDoc: tc.doc}
					if len(tc.rows) > 0 {
						in.Log = tc.rows
					}
					line := ledger.Check(in)
					if !strings.Contains(line, tc.ledger) {
						t.Fatalf("ledger.Check = %q, want it to contain %q", line, tc.ledger)
					}
					want = line + "\n"
				}

				code, stderr := f.status(tc.caller(), tc.state, tc.detail)
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
