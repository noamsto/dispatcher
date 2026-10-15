package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode"
	"unicode/utf8"
)

const (
	from = "worker:feat/x#s2"
	crew = "c1"

	wantHint = `every acceptance ledger item must read <id> pass(<evidence>) or <id> waived(dispatcher), e.g. "AC1 pass(bats 12/12); AC2 waived(dispatcher)", with any note inside those parentheses; pending, not run, skipped, n/a, partial, or a note after the parentheses is refused. An item you cannot run is not a pass: post blocked "acceptance: <item> — <why>" and await the dispatcher, who alone waives it.`

	prefix   = "crew: refusing pr_open for " + from + " — "
	malform  = prefix + wantHint
	noList   = prefix + `this task doc has no acceptance list (no heading, bold, or line-start spelling of Acceptance), so an empty detail is the correct pr_open: crew status "$CREW_WORKER_ID" pr_open "" <url>. Do not invent a pass(...) item. (` + wantHint + ")"
	mustLed  = prefix + "the task doc has an acceptance list, so the pr_open detail must carry its ledger: " + wantHint
	readFail = prefix + "could not read the task doc's acceptance list"
	noID     = prefix + "a waived(dispatcher) item needs its acceptance id (e.g. AC3 waived(dispatcher)) so the dispatcher's waiver can name it."

	docList = "tier: standard\nkind: implement\n\n## Acceptance\n\n- AC1 bats green\n- AC2 CI green on the PR head\n- AC3 docs updated\n\n## Out of scope\n- AC4 CI everything\n"
	docNone = "tier: standard\nkind: implement\n\nDo the thing.\n"
)

func ciLine(id string) string {
	return prefix + "acceptance item '" + id + "' is a CI item, so its pass(...) must carry a CI run id or an actions/runs/<id> URL (for the PR's current head); a local gate (pre-push, bats-affected, shellcheck, nix flake check) is never CI evidence. If CI has not finished, wait for it, or block and ask the dispatcher to waive the item."
}

func waiverLine(id string) string {
	return prefix + "waived(dispatcher) needs a dispatcher waiver on the bus: a crew reply to this session (" + from + ") that names the item ('" + id + `') in a waive phrase, e.g. "waive ` + id + `"; a negation ("will not waive") does not count (a reply sent to an earlier session does not carry over). Block and ask the dispatcher to waive the item; do not write the waiver yourself.`
}

// row is one bus line as `crew reply` writes it, with a string body.
func row(c, sender, to, body string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		TS     int    `json:"ts"`
		CrewID string `json:"crew_id"`
		Kind   string `json:"kind"`
		From   string `json:"from"`
		To     string `json:"to"`
		Body   string `json:"body"`
	}{1, c, "msg", sender, to, body})
	return strings.TrimSuffix(b.String(), "\n")
}

func reply(body string) string { return row(crew, "dispatcher:"+crew, from, body) }

// jsonStr is s as a JSON string literal, DEL and non-ASCII raw.
func jsonStr(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// inner is jsonStr without the quotes, for splicing raw text into a literal.
func inner(s string) string { j := jsonStr(s); return j[1 : len(j)-1] }

// rawRow is a bus line whose body is already JSON text.
func rawRow(c, sender, to, bodyJSON string) string {
	return `{"crew_id":` + jsonStr(c) + `,"kind":"msg","from":` + jsonStr(sender) + `,"to":` + jsonStr(to) + `,"body":` + bodyJSON + `}`
}

type checkCase struct {
	name   string
	detail string
	doc    string
	docErr bool
	log    []string
	want   string
}

var checkCases = []checkCase{
	// form and the empty detail
	{name: "malformed with a list", detail: "AC1 pending", doc: docList, want: malform},
	{name: "malformed without a list", detail: "AC1 pending", doc: docNone, want: noList},
	{name: "whitespace-only detail with a list", detail: " \t\n", doc: docList, want: mustLed},
	{name: "empty detail without a list", detail: "", doc: docNone, want: ""},
	{name: "NBSP is \\s to the grammar but not [[:space:]] to the blank test", detail: " ", doc: docList, want: ""},
	{name: "bold acceptance spelling is a list", detail: "", doc: "**Acceptance criteria**\n- AC1 x\n", want: mustLed},
	{name: "line-start Acceptance: is a list", detail: "", doc: "  ACCEPTANCE: below\n", want: mustLed},
	{name: "a mid-sentence mention is not a list", detail: "", doc: "see the acceptance notes\n", want: ""},
	{name: "a single-# heading is not a list", detail: "", doc: "# Acceptance\n", want: ""},

	// an unreadable task doc
	{name: "unreadable doc, malformed detail", detail: "AC1 pending", docErr: true, want: noList},
	{name: "unreadable doc, empty detail", detail: "", docErr: true, want: readFail},
	{name: "unreadable doc, conforming detail", detail: "AC1 pass(x)", docErr: true, want: readFail},

	// accepted forms
	{name: "plain pass", detail: "AC1 pass(bats 12/12)", doc: docList},
	{name: "nested evidence and a CI run id", detail: "AC1 pass(a (b) c); AC2 pass(run id 1234567)", doc: docList},
	{name: "trailing colon on the id", detail: "AC1: pass(x)", doc: docList},
	{name: "CI item with an actions/runs URL", detail: "AC2 pass(CI https://github.com/o/r/actions/runs/123)", doc: docList},
	{name: "CI item with a run id", detail: "AC2 pass(CI run id 1234567)", doc: docList},
	{name: "multi-char fold", detail: "AC1 paß(x)", doc: docList},
	{name: "upper case keyword", detail: "AC1 PASS(x)", doc: docList},

	// refused forms
	{name: "pending", detail: "AC1 pending", doc: docList, want: malform},
	{name: "blank evidence", detail: "AC1 pass( )", doc: docList, want: malform},
	{name: "note after the parens", detail: "AC1 pass(x) note", doc: docList, want: malform},
	{name: "glued item", detail: "pass(x)AC2", doc: docList, want: malform},

	// CI detection
	{name: "evidence names CI", detail: "AC1 pass(CI green)", doc: docList, want: ciLine("AC1")},
	{name: "entry names CI", detail: "AC2 pass(bats 3/3)", doc: docList, want: ciLine("AC2")},
	{name: "entry by number", detail: "2 pass(local)", doc: docList, want: ciLine("2")},
	{name: "entry by ac-number", detail: "ac-02 pass(local)", doc: docList, want: ciLine("ac-02")},
	{name: "entry by position", detail: "pass(a); pass(b)", doc: docList, want: ciLine("?")},
	{name: "unknown non-numeric id has no entry", detail: "foo pass(a)", doc: "## Acceptance\n- CI\n", want: ""},
	{name: "Non-CI is not CI", detail: "AC1 pass(Non-CI check)", doc: docList},
	{name: "fenced code is not the list", detail: "1 pass(x)", doc: "## Acceptance\n```\n- CI fenced\n```\n- AC1 docs\n"},
	{name: "list ends at the next heading", detail: "AC4 pass(x)", doc: docList},
	{name: "list ends at bold out of scope", detail: "AC2 pass(x)", doc: "**Acceptance:**\n- AC1 docs\n**Out of scope**\n- AC2 CI\n"},
	{name: "indented items are not entries", detail: "1 pass(x)", doc: "## Acceptance\n  - CI\n"},
	{name: "numbered entries", detail: "AC2 pass(x)", doc: "### Acceptance\n1. AC1 docs\n2) AC2 CI run\n", want: ciLine("AC2")},
	{name: "bold id prefix", detail: "AC1 pass(x)", doc: "## Acceptance\n- **AC1** CI green\n", want: ciLine("AC1")},
	{name: "id lookup needs a token boundary", detail: "AC1 pass(x)", doc: "## Acceptance\n- AC10 CI\n- AC1 docs\n"},
	{name: "id lookup is case-insensitive", detail: "ac1 pass(x)", doc: "## Acceptance\n- AC2 docs\n- AC1 CI\n", want: ciLine("ac1")},
	{name: "awk -v unescapes the id", detail: `AC\061 pass(local)`, doc: "## Acceptance\n- AC2 docs\n- AC1 CI\n", want: ciLine(`AC\061`)},
	// glibc's [:alnum:] (bash's =~, gawk) takes Other_Alphabetic marks: U+093E, U+0345.
	{name: "Other_Alphabetic mark is no boundary before run", detail: "AC1 pass(CI \u093erun 1234567)", doc: docList, want: ciLine("AC1")},
	{name: "Other_Alphabetic mark U+0345 is no boundary before run", detail: "AC1 pass(CI \u0345run 1234567)", doc: docList, want: ciLine("AC1")},
	{name: "a mark glibc 2.42 calls alnum is no boundary before run", detail: "AC1 pass(CI \u0363run 1234567)", doc: docList, want: ciLine("AC1")},
	{name: "letter is no boundary before run", detail: "AC1 pass(CI xrun 1234567)", doc: docList, want: ciLine("AC1")},
	{name: "accented letter is no boundary before run", detail: "AC1 pass(CI \u00e9run 1234567)", doc: docList, want: ciLine("AC1")},
	{name: "Other_Alphabetic mark is no boundary before CI", detail: "AC1 pass(\u093eCI green)", doc: docList},
	{name: "Other_Alphabetic mark is no boundary before CI, U+0345", detail: "AC1 pass(\u0345CI green)", doc: docList},
	{name: "Other_Alphabetic mark after the id blocks the id lookup", detail: "AC1 pass(local)", doc: "## Acceptance\n- CI gate green\n- AC1\u093e local\n", want: ciLine("AC1")},
	{name: "Other_Alphabetic mark U+0345 after the id blocks the id lookup", detail: "AC1 pass(local)", doc: "## Acceptance\n- CI gate green\n- AC1\u0345 local\n", want: ciLine("AC1")},
	{name: "letter after the id blocks the id lookup", detail: "AC1 pass(local)", doc: "## Acceptance\n- CI gate green\n- AC1\u00e9 local\n", want: ciLine("AC1")},
	// gawk's [:alnum:] lags Go's tables (U+0C04, U+11F04, U+0F82 are alnum only
	// to Go), so an entry with such a character after the id may be the arm's
	// match or not: it is CI if either reading's entry names CI.
	{name: "U+0C04 after the id may be the arm's match", detail: "AC1 pass(bats ok); lint pass(ok)", doc: "## Acceptance\n- unit tests pass\n- lint\u0c04 CI green\n", want: ciLine("lint")},
	{name: "U+11F04 after the id may be the arm's match", detail: "AC1 pass(bats ok); lint pass(ok)", doc: "## Acceptance\n- unit tests pass\n- lint\U00011f04 CI green\n", want: ciLine("lint")},
	{name: "U+0F82 after the id may be the arm's match", detail: "AC1 pass(bats ok); lint pass(ok)", doc: "## Acceptance\n- unit tests pass\n- lint\u0f82 CI green\n", want: ciLine("lint")},
	{name: "an ambiguous entry does not hide the fallback's CI", detail: "AC2 pass(local)", doc: "## Acceptance\n- AC1 docs\n- CI gate green\n- AC2\u0c04 local\n", want: ciLine("AC2")},
	{name: "an ambiguous entry and its fallback both without CI", detail: "AC2 pass(local)", doc: "## Acceptance\n- AC1 CI green\n- docs\n- AC2\u0c04 local\n"},
	{name: "first failing item wins", detail: "AC2 pass(local); AC3 waived(dispatcher)", doc: docList, want: ciLine("AC2")},

	// waivers
	{name: "waiver to this session", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("waive AC3")}},
	{name: "negated waiver", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("I will not waive AC3")}, want: waiverLine("AC3")},
	{name: "curly negation", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("won’t waive AC3")}, want: waiverLine("AC3")},
	{name: "another item's waiver", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("waive AC4")}, want: waiverLine("AC3")},
	{name: "earlier session's reply", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{row(crew, "dispatcher:"+crew, "worker:feat/x#s1", "waive AC3")}, want: waiverLine("AC3")},
	{name: "another sender", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{row(crew, "worker:x", from, "waive AC3")}, want: waiverLine("AC3")},
	{name: "another crew", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{row("c2", "dispatcher:c2", from, "waive AC3")}, want: waiverLine("AC3")},
	{name: "but splits clauses", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("will not waive AC2 but waive AC3")}},
	{name: "unicode letter is no word boundary", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("éwaive AC3")}, want: waiverLine("AC3")},
	{name: "a sentence end splits off the period", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("Waived AC3.")}},
	{name: "id needs a token boundary", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("waive AC3x")}, want: waiverLine("AC3")},
	{name: "object body is its JSON text", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":{"note":"waive AC3"}}`}},
	{name: "duplicate nested key takes the last value", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":{"x":"waive AC3","x":""}}`}, want: waiverLine("AC3")},
	{name: "duplicate nested key last value waives", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":{"x":"not","x":"waive AC3"}}`}},
	{name: "number literal prints as jq does", detail: "AC1E2 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":["waive",1e2]}`}, want: waiverLine("AC1E2")},
	{name: "lone high surrogate refuses the row", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":"\ud800 waive AC3"}`}, want: waiverLine("AC3")},
	{name: "paired surrogate is fine", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":"\ud83d\ude00 waive AC3"}`}},
	{name: "nesting past jq limit refuses the row", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{`{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":` + strings.Repeat("[", 9999) + `"waive AC3"` + strings.Repeat("]", 9999) + `}`}, want: waiverLine("AC3")},
	{name: "a leading BOM is skipped", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{"\xef\xbb\xbf" + reply("waive AC3")}},
	{name: "invalid UTF-8 in a bus line never waives", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{strings.Replace(reply("waive AC3"), "waive AC3", "waive\xff AC3", 1)}, want: waiverLine("AC3")},
	{name: "invalid UTF-8 in the detail's id is malformed", detail: "AC\xe2\x82b waived(dispatcher)", doc: docList, log: []string{reply("waive AC��b")}, want: malform},
	// jq folds the truncated lead byte and the ")" into one U+FFFD, so it calls this malformed.
	{name: "invalid UTF-8 swallowing a paren is malformed", detail: "AC1 pass(ok\xf0)", doc: docList, want: malform},
	{name: "invalid UTF-8 swallowing a paren, no list", detail: "AC1 pass(ok\xf0)", doc: docNone, want: noList},
	{name: "the same line repaired waives", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{reply("waive\ufffd AC3")}},
	{name: "waived without an id", detail: "waived(dispatcher)", doc: docList, want: noID},
	{name: "colon-only id strips to empty", detail: ": waived(dispatcher)", doc: docList, want: noID},
	{name: "trailing colon id", detail: "AC3: waived(dispatcher)", doc: docList, log: []string{reply("waive ac3")}},
	{name: "no log", detail: "AC3 waived(dispatcher)", doc: docList, want: waiverLine("AC3")},
	{name: "unparsable lines skipped", detail: "AC3 waived(dispatcher)", doc: docList, log: []string{"not json", "{", "1 2", reply("waive AC3"), ""}},
	{name: "waiver for a later item", detail: "AC1 pass(x); AC3 waived(dispatcher)", doc: docList, want: waiverLine("AC3")},
}

func TestCheck(t *testing.T) {
	for _, tc := range checkCases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(Input{From: from, Crew: crew, Detail: tc.detail, TaskDoc: tc.doc, TaskDocErr: tc.docErr, Log: tc.log})
			if got != tc.want {
				t.Errorf("Check(%q)\n got %q\nwant %q", tc.detail, got, tc.want)
			}
		})
	}
}

// oversizeRows are matching-looking rows over the bound that Go cannot read or
// stringify as jq does, while jq runs its regexes on the 12M-char clause.
func oversizeRows() map[string]string {
	long := "waive ac3 not " + strings.Repeat("x", 12_000_000)
	head := strings.TrimSuffix(reply(""), `"body":""}`)
	return map[string]string{
		"tostring error": head + `"body":[` + jsonStr(long) + `,1e-1000000001]}`,
		"nan field":      head + `"body":` + jsonStr(long) + `,"n":nan}`,
		"leading zero":   head + `"body":` + jsonStr(long) + `,"n":01}`,
		"invalid UTF-8":  strings.Replace(reply(long), "not ", "not \xff", 1),
	}
}

// TestWaivedBodyBound pins the 1 MiB bound: an oversize row stops every waiver
// from counting, jq aborting on it, and a body exactly at the bound still counts.
func TestWaivedBodyBound(t *testing.T) {
	pad := func(lead string, n int) string { return lead + strings.Repeat("x", n-len(lead)) }
	type boundCase struct {
		name string
		log  []string
		want bool
	}
	tests := []boundCase{
		{"long negated clause beside a plain waiver", []string{reply("waive AC3 not " + strings.Repeat("x", 12_000_000)), reply("waive AC3")}, false},
		{"oversize row after a plain waiver", []string{reply("waive AC3"), reply(pad("waive AC3 ", maxBody+1))}, false},
		{"body at the bound", []string{reply(pad("waive AC3 ", maxBody))}, true},
		{"body one byte over", []string{reply(pad("waive AC3 ", maxBody+1))}, false},
		{"oversize row for another session is ignored", []string{row(crew, "dispatcher:"+crew, "worker:feat/x#s1", pad("x", maxBody+1)), reply("waive AC3")}, true},
	}
	for name, line := range oversizeRows() {
		tests = append(tests, boundCase{"oversize " + name, []string{line, reply("waive AC3")}, false})
	}
	tests = append(tests, boundCase{"undecodable line under the bound", []string{"not json", reply("waive AC3")}, true})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := waived(tc.log, crew, from, "AC3"); got != tc.want {
				t.Errorf("waived = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWaivedRefusesInvalidUTF8 pins that an argument or a bus line holding
// invalid UTF-8 never waives, though jq would repair it; each control is the
// same waiver with the repaired spelling, which does.
func TestWaivedRefusesInvalidUTF8(t *testing.T) {
	waiver := func(c, to, body string) string { return row(c, "dispatcher:"+c, to, body) }
	tests := []struct {
		name                    string
		log                     []string
		crew, from, id          string
		ctlCrew, ctlFrom, ctlID string
		ctlLog                  []string
	}{
		{name: "from truncated sequence", log: []string{waiver(crew, "w\ufffd\ufffd", "waive AC3")}, crew: crew, from: "w\xe2\x82", id: "AC3", ctlCrew: crew, ctlFrom: "w\ufffd\ufffd", ctlID: "AC3"},
		{name: "from invalid bytes", log: []string{waiver(crew, "w\ufffd\ufffd", "waive AC3")}, crew: crew, from: "w\xff\xff", id: "AC3", ctlCrew: crew, ctlFrom: "w\ufffd\ufffd", ctlID: "AC3"},
		{name: "crew", log: []string{waiver("c\ufffd\ufffd", from, "waive AC3")}, crew: "c\xe2\x82", from: from, id: "AC3", ctlCrew: "c\ufffd\ufffd", ctlFrom: from, ctlID: "AC3"},
		{name: "id truncated sequence", log: []string{reply("waive ac\ufffd\ufffd")}, crew: crew, from: from, id: "AC\xe2\x82", ctlCrew: crew, ctlFrom: from, ctlID: "AC\ufffd\ufffd"},
		{name: "id invalid lead byte", log: []string{reply("waive x\ufffdA")}, crew: crew, from: from, id: "x\xf0A", ctlCrew: crew, ctlFrom: from, ctlID: "x\ufffdA"},
		{name: "bus line", log: []string{strings.Replace(reply("waive AC3"), "waive AC3", "waive\xff AC3", 1)}, crew: crew, from: from, id: "AC3", ctlCrew: crew, ctlFrom: from, ctlID: "AC3", ctlLog: []string{reply("waive\ufffd AC3")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctl := tc.log
			if tc.ctlLog != nil {
				ctl = tc.ctlLog
			}
			if !waived(ctl, tc.ctlCrew, tc.ctlFrom, tc.ctlID) {
				t.Fatal("control does not waive")
			}
			if waived(tc.log, tc.crew, tc.from, tc.id) {
				t.Error("waived invalid UTF-8")
			}
		})
	}
}

// accepted and refused are the jq 1.8.2 probes of the form regex.
var (
	accepted = []string{
		"AC1 pass(x)", "AC1 pass(x)\n", "AC1 PASS(x)", "AC1 paß(x)", "AC1 paſs(x)", "AC1 pass(a(b)c)",
		"AC1 waived(dispatcher)", "AC1 waived(dispatcher: x (y) z)", "AC1 pass(x). ", "AC1 pass(x);AC2 pass(y)",
		"pass(x) pass(y)", "", "AC1 pass(x).\n", "AC1 pass(x)\n.", "AC1 pass(x) .", "AC1 pass(x),", "AC1 pass(x) ; ",
		"AC1: pass(x)", "AC1  pass(x)", "AC1 pass(x)", "AC1\u0085pass(x)", "AC1 pass(( x))", "AC1 pass(()x)",
		"AC1 pass(\tx)", "AC1 waived(dispatcher;x)", "AC1 pass(x)", "AC1 PAẞ(x)",
	}
	refused = []string{
		"junk\nAC1 pass(x)", "AC1 pass(x)\njunk", "AC1 pass( )", "AC1 waived(dispatcherx)", "AC1 pass(x).\nAC2 pass(y)",
		"AC1 pass(x);;", "(AC1 pass(x)", "AC1 pass (x)", "a b pass(x)", "AC1 pass(x)AC2 pass(y)",
		"AC1 waived(dispatcher x", "AC1 waived(dispatcher ((x)", "AC1 pass(x)\n\n.x",
	}
)

func TestConforms(t *testing.T) {
	for _, d := range accepted {
		if !conforms(d) {
			t.Errorf("conforms(%q) = false, want true", d)
		}
	}
	for _, d := range refused {
		if conforms(d) {
			t.Errorf("conforms(%q) = true, want false", d)
		}
	}
}

func TestItems(t *testing.T) {
	got := items("AC1 pass(x (y)) ; foo: waived(dispatcher: n (z)) pass(q\n)")
	want := []item{
		{id: "AC1", kind: "pass", ev: "(x (y))"},
		{id: "foo:", kind: "waived", ev: "(z)"},
		{id: "", kind: "pass", ev: "(q )"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("items\n got %q\nwant %q", got, want)
	}
}

// TestLongDetail keeps the grammar near-linear: a ledger the size of an argv
// must not take quadratic or exponential time.
func TestLongDetail(t *testing.T) {
	var b strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&b, "AC%d pass(x (y) z); ", i)
	}
	if !conforms(b.String()) {
		t.Fatal("long ledger refused")
	}
	if n := len(items(b.String())); n != 20000 {
		t.Fatalf("items = %d, want 20000", n)
	}
	deep := strings.Repeat("waived(dispatcher ", 20000) + strings.Repeat("(", 20000)
	if conforms(deep) || len(items(deep)) != 0 {
		t.Fatal("unbalanced ledger accepted")
	}
}

// TestAlnumAgainstGlibc runs the arm's ci_re and run_re under bash and the id
// lookup's next-character test under gawk, both in C.UTF-8, over a sample of
// code points (every mark and Other_Alphabetic one, every 61st of the rest).
// Go's tables may lag glibc's, so the regexes are pinned one way: Go finds CI
// whenever bash does and run evidence only where bash does. gawk's own tables
// lag both, so the id test is pinned on the plain cases and checked one way,
// and gawkAlnum, the part Go reads as surely alnum to gawk, the other.
func TestAlnumAgainstGlibc(t *testing.T) {
	bash, errB := exec.LookPath("bash")
	gawk, errG := exec.LookPath("gawk")
	if errB != nil || errG != nil {
		t.Skip("bash or gawk not installed")
	}
	env := append(os.Environ(), "LC_ALL=C.UTF-8")
	if err := (&exec.Cmd{Path: bash, Args: []string{"bash", "-c", `[[ é =~ ^[[:alnum:]]$ ]]`}, Env: env}).Run(); err != nil {
		t.Skip("no C.UTF-8 locale")
	}
	// Darwin's libc leaves Other_Alphabetic (Ⓐ) out of [[:alnum:]]; glibc does not.
	if err := (&exec.Cmd{Path: bash, Args: []string{"bash", "-c", `[[ Ⓐ =~ ^[[:alnum:]]$ ]]`}, Env: env}).Run(); err != nil {
		t.Skip("bash's libc is not glibc")
	}
	pinned := []rune{'A', 'z', '7', 'é', 0x093e, 0x0345, '-', ' ', '!'}
	sample := slices.Clone(pinned)
	for r := rune(1); r <= unicode.MaxRune; r++ {
		if r == '\n' || r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if r%61 == 0 || r < 0x80 || unicode.In(r, unicode.M, unicode.Other_Alphabetic) {
			sample = append(sample, r)
		}
	}
	slices.Sort(sample)
	sample = slices.Compact(sample)
	var in strings.Builder
	for _, r := range sample {
		in.WriteString(string(r) + "\n")
	}
	run := func(argv ...string) []string {
		t.Helper()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env, cmd.Stdin = env, strings.NewReader(in.String())
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", argv[0], err)
		}
		return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	}
	ciRun := run(bash, "-c", `ci='(^|[^-[:alnum:]_])CI([^[:alnum:]_]|$)'
run='actions/runs/[0-9]+|(^|[^[:alnum:]_-])[Rr]un([ _-]?[Ii][Dd])?[ :#=]*[0-9]{6,}'
while IFS= read -r c; do
  [[ ${c}CI =~ $ci ]] && a=1 || a=0
  [[ "${c}run 1234567" =~ $run ]] && b=1 || b=0
  echo "$a$b"
done`)
	idNext := run(gawk, `{ print ($0 !~ /[[:alnum:]_.]/) ? 1 : 0 }`)
	if len(ciRun) != len(sample) || len(idNext) != len(sample) {
		t.Fatalf("got %d bash and %d gawk lines for %d code points", len(ciRun), len(idNext), len(sample))
	}
	for i, r := range sample {
		c := string(r)
		if bashCI, goCI := ciRun[i][0] == '1', ciRe.MatchString(c+"CI"); bashCI && !goCI {
			t.Errorf("%U before CI: bash finds CI, Go does not", r)
		}
		if bashRun, goRun := ciRun[i][1] == '1', runRe.MatchString(c+"run 1234567"); goRun && !bashRun {
			t.Errorf("%U before run: Go finds run evidence, bash does not", r)
		}
		if gawkBoundary := idNext[i] == "1"; !gawkBoundary && !glibcAlnum(r) && r != '_' && r != '.' {
			t.Errorf("%U after an id: gawk calls it alnum, Go does not", r)
		}
		if gawkBoundary := idNext[i] == "1"; gawkBoundary && gawkAlnum(r) {
			t.Errorf("%U after an id: Go is sure gawk calls it alnum, gawk does not", r)
		}
	}
	for _, r := range pinned {
		i, ok := slices.BinarySearch(sample, r)
		if !ok {
			t.Fatalf("%U not sampled", r)
		}
		if gawkBoundary := idNext[i] == "1"; gawkBoundary == (glibcAlnum(r) || r == '_' || r == '.') {
			t.Errorf("%U after an id: gawk boundary %v, Go disagrees", r, gawkBoundary)
		}
	}
}

// jqBin returns jq, skipping the differential tests where it is absent (the Nix
// sandbox): the testdata programs are the arm's own, run by the jq the arm ran.
func jqBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	return bin
}

func program(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runJQ returns jq's stdout and exit status, -1 when jq could not run. It is
// called from parallel goroutines, so it reports with Errorf, never Fatal.
func runJQ(t *testing.T, bin, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode()
	}
	t.Errorf("jq: %v", err)
	return "", -1
}

// parallel runs f over every input with bounded concurrency; f reports through t.
func parallel(n int, f func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			f(i)
		}()
	}
	wg.Wait()
}

var ledgerAlphabet = []string{
	"pass", "PASS", "waived", "dispatcher", "(", ")", "(", ")", ";", ",", ".", ":", "\n", " ", " ", " ", "\u0085",
	"ß", "ẞ", "ſ", "İ", "AC1", "AC2", "CI", "x", "run id 1234567", "actions/runs/9", "\x1f", "\t", "\r",
	"pass(", "waived(dispatcher", "pa", "ss", "AC1 ", "Non-CI",
}

// ledgerCorpus is the probes plus seeded random strings: half free token soup,
// half item-shaped (id, keyword, evidence, separator) with a token perturbed so
// the conforming and nearly conforming shapes are both well covered.
func ledgerCorpus() []string {
	corpus := slices.Concat(accepted, refused)
	r := rand.New(rand.NewPCG(945, 2683))
	pick := func() string { return ledgerAlphabet[r.IntN(len(ledgerAlphabet))] }
	for range 1500 {
		var b strings.Builder
		for range 1 + r.IntN(12) {
			b.WriteString(pick())
		}
		corpus = append(corpus, b.String())
	}
	ids := []string{"", "", "AC1 ", "AC1 ", "AC2: ", "2 ", "x\x1fpass ", "a(b ", "ac-02\u00a0", "AC3\n"}
	passKw := []string{"pass", "PAss", "paß", "paſs", "PAẞ", "pass "}
	waivedKw := []string{"waived", "WAIVED", "waıved"}
	passEv := []string{"(x)", "(\tCI)", "(a (b) c)", "(()x)", "(run id 1234567)", "(ß\r)"}
	waivedEv := []string{"(dispatcher)", "(dispatcher: n (z))", "(dispatcher;x)", "(Dispatcher\n(y))", "(DISPATCHER,)", "(diſpatcher)"}
	badEv := []string{"( )", "(x", "(dispatcherx)", "(dispatcher (q)", "()"}
	seps := []string{"", " ", "; ", "; ", ",", ", ", ".", " .", ";;", "\n", ". \n", "x", "\u00a0;\u2028"}
	for range 3000 {
		var b strings.Builder
		for range 1 + r.IntN(3) {
			b.WriteString(ids[r.IntN(len(ids))])
			kw, ev := passKw, passEv
			if r.IntN(2) == 0 {
				kw, ev = waivedKw, waivedEv
			}
			if r.IntN(7) == 0 {
				ev = badEv
			}
			b.WriteString(kw[r.IntN(len(kw))])
			b.WriteString(ev[r.IntN(len(ev))])
			b.WriteString(seps[r.IntN(len(seps))])
		}
		s := b.String()
		if r.IntN(6) == 0 {
			i := r.IntN(len(s) + 1)
			for i > 0 && i < len(s) && s[i]&0xc0 == 0x80 {
				i--
			}
			s = s[:i] + pick() + s[i:]
		}
		corpus = append(corpus, s)
	}
	// Invalid UTF-8 that jq folds into one U+FFFD, sometimes with the next ASCII
	// byte: spliced into item-shaped details at rune boundaries, and set inside
	// the evidence where jq can still call the ledger conforming.
	bad := []string{"\xe2\x82", "\xff", "\xf0", "\xf0)", "\xf0(", "\xf0;", "\xc0\x80", "\xe2\x82)", "\xf0\x9f"}
	shaped := corpus[len(corpus)-3000:]
	for range 300 {
		s := shaped[r.IntN(len(shaped))]
		i := r.IntN(len(s) + 1)
		for i > 0 && i < len(s) && s[i]&0xc0 == 0x80 {
			i--
		}
		corpus = append(corpus, s[:i]+bad[r.IntN(len(bad))]+s[i:])
	}
	evs := []string{"ok", "x (y)", "", " ", "CI", "a;b"}
	tails := []string{")", ")", "x)", " )", "))", ") ", ");"}
	for range 300 {
		d := ids[r.IntN(len(ids))] + "pass(" + evs[r.IntN(len(evs))] + bad[r.IntN(len(bad))] + tails[r.IntN(len(tails))]
		corpus = append(corpus, d, d+seps[r.IntN(len(seps))])
	}
	return corpus
}

func TestDifferentialAgainstJQ(t *testing.T) {
	bin := jqBin(t)
	corpus := ledgerCorpus()

	t.Run("ledger", func(t *testing.T) {
		prog := program(t, "ledger.jq")
		var yes, bad, badYes atomic.Int64
		parallel(len(corpus), func(i int) {
			d := corpus[i]
			_, rc := runJQ(t, bin, "", "-en", "--arg", "d", d, prog)
			if rc != 0 && rc != 1 {
				t.Errorf("jq exit %d for %q", rc, d)
				return
			}
			valid := utf8.ValidString(d)
			want := rc == 1
			if want {
				yes.Add(1)
			}
			if !valid {
				bad.Add(1)
				if want {
					badYes.Add(1)
				}
			}
			// Check refuses invalid UTF-8 outright, where jq repairs it and may accept.
			got := valid && conforms(d)
			if got && !want || want && !got && valid {
				t.Errorf("conforms(%q) = %v, jq says %v", d, got, want)
			}
		})
		if bad.Load() == 0 {
			t.Error("no invalid UTF-8 details in the corpus")
		}
		t.Logf("%d details, %d conforming, %d invalid UTF-8 of which jq accepted %d", len(corpus), yes.Load(), bad.Load(), badYes.Load())
	})

	t.Run("items", func(t *testing.T) {
		prog := program(t, "items.jq")
		parallel(len(corpus), func(i int) {
			d := corpus[i]
			if !utf8.ValidString(d) {
				return // Check refuses these before the item scan
			}
			out, rc := runJQ(t, bin, "", "-nr", "--arg", "d", d, prog)
			if rc != 0 {
				t.Errorf("jq exit %d for %q", rc, d)
				return
			}
			var got strings.Builder
			for _, it := range items(d) {
				got.WriteString(it.id + "\x1f" + it.kind + "\x1f" + it.ev + "\n")
			}
			if got.String() != out {
				t.Errorf("items(%q)\n got %q\n jq %q", d, got.String(), out)
			}
		})
	})

	t.Run("waiver", func(t *testing.T) {
		prog := program(t, "waiver.jq")
		cases := waiverCorpus()
		var yes atomic.Int64
		var mu sync.Mutex
		outcome := map[string][2]int{} // shape -> jq [refused, waived]
		count := map[string]int{}
		invalid := 0
		for _, c := range cases {
			count[c.shape]++
			if !c.validUTF8() {
				invalid++
			}
		}
		parallel(len(cases), func(i int) {
			c := cases[i]
			_, rc := runJQ(t, bin, strings.Join(c.log, "\n")+"\n", "-e", "-n", "-R", "--arg", "c", c.crew, "--arg", "f", c.from, "--arg", "id", c.id, prog)
			if rc != 0 && rc != 1 {
				t.Errorf("jq exit %d for %.300q %.300q", rc, c.id, c.log)
				return
			}
			want := rc == 0
			if want {
				yes.Add(1)
			}
			mu.Lock()
			o := outcome[c.shape]
			if want {
				o[1]++
			} else {
				o[0]++
			}
			outcome[c.shape] = o
			mu.Unlock()
			// Refusing invalid UTF-8 where jq repairs it is the one allowed gap.
			got := waived(c.log, c.crew, c.from, c.id)
			if got && !want || want && !got && c.validUTF8() {
				t.Errorf("waived(%.300q, crew %q, from %q, id %q) = %v, jq says %v", c.log, c.crew, c.from, c.id, got, want)
			}
		})
		for _, s := range waiverShapes {
			if count[s] == 0 {
				t.Errorf("no %s cases generated", s)
			}
		}
		for _, s := range []string{"deep-array", "deep-object"} {
			if o := outcome[s]; o[0] == 0 || o[1] == 0 {
				t.Errorf("%s cases never fall on both sides of jq's limit: jq refused %d, waived %d", s, o[0], o[1])
			}
		}
		t.Logf("%d logs, %d waived, %d invalid UTF-8; per shape %v; jq [refused waived] per shape %v", len(cases), yes.Load(), invalid, count, outcome)
	})

	// A matching row with a long negated clause makes Oniguruma hit its retry
	// limit, which aborts the whole program, so a second plain waiver is refused too.
	t.Run("waiver retry limit", func(t *testing.T) {
		prog := program(t, "waiver.jq")
		run := func(log []string) int {
			_, rc := runJQ(t, bin, strings.Join(log, "\n")+"\n", "-e", "-n", "-R", "--arg", "c", crew, "--arg", "f", from, "--arg", "id", "AC3", prog)
			return rc
		}
		long := []string{reply("waive AC3 not " + strings.Repeat("x", 12_000_000)), reply("waive AC3")}
		if rc := run(long); rc != 5 {
			t.Errorf("jq exit %d on the long negated clause, want 5", rc)
		}
		if waived(long, crew, from, "AC3") {
			t.Error("waived on the long negated clause, jq aborts")
		}
		for name, line := range oversizeRows() {
			log := []string{line, reply("waive AC3")}
			if rc := run(log); rc != 5 {
				t.Errorf("jq exit %d on the oversize %s row, want 5", rc, name)
			}
			if waived(log, crew, from, "AC3") {
				t.Errorf("waived on the oversize %s row, jq aborts", name)
			}
		}
		atBound := []string{reply("waive AC3 " + strings.Repeat("x", maxBody-len("waive AC3 ")))}
		if rc := run(atBound); rc != 0 {
			t.Errorf("jq exit %d on a body at the bound, want 0", rc)
		}
		if !waived(atBound, crew, from, "AC3") {
			t.Error("not waived on a body at the bound, jq waives")
		}
	})

	t.Run("word class", func(t *testing.T) {
		out, rc := runJQ(t, bin, "", "-nr", `[range(1;1114112) | select(. < 55296 or . > 57343)] | map(select([.] | implode | test("\\w"))) | .[]`)
		if rc != 0 {
			t.Fatalf("jq exit %d", rc)
		}
		want := map[rune]bool{}
		for f := range strings.FieldsSeq(out) {
			n, err := strconv.Atoi(f)
			if err != nil {
				t.Fatal(err)
			}
			want[rune(n)] = true
		}
		for r := rune(1); r < 0x110000; r++ {
			if r >= 0xd800 && r <= 0xdfff {
				continue
			}
			if isWord(r) != want[r] {
				t.Errorf("isWord(%U) = %v, Oniguruma \\w says %v", r, !want[r], want[r])
			}
		}
	})
}

type waiverCase struct {
	shape, id, crew, from string
	log                   []string
}

func (c waiverCase) validUTF8() bool {
	return utf8.ValidString(c.id) && utf8.ValidString(c.crew) && utf8.ValidString(c.from) && !slices.ContainsFunc(c.log, func(l string) bool { return !utf8.ValidString(l) })
}

// waiverShapes are the corpus shapes the differential must generate.
var waiverShapes = []string{
	"object-body", "object-control", "dup-key", "surrogate", "bom", "deep-array", "deep-object",
	"invalid-body", "invalid-from", "invalid-id", "invalid-crew",
}

var waiverWords = []string{
	"waive", "waived", "waiver", "waives", "waiving", "waivers", "WAIVE", "not", "never", "no", "cannot", "without",
	"won't", "won’t", "don't", "nothing", "but", "BUT", "butter", "ac3", "AC3", "ac4", "ac33", "ac3-x", "ac3.x", "x",
	" ", " ", " ", ".", "!", "?", ";", "\n", ",", "é", "Ⓐ", "²", "_", "-", "(", ")", ":", "\"", " ", "ࢗ", "3",
	"\x7f", "\x01", "\x1f", "\b", "\f",
}

// badBytes pair an invalid UTF-8 sequence with how jq repairs it.
var badBytes = []struct{ bad, fixed string }{
	{"\xe2\x82", "�"}, {"\xff", "�"}, {"\xff\xff", "��"}, {"\xf0A", "�"}, {"\xc0\x80", "��"},
}

// waiverCorpus is half word soup and half waive-shaped phrases (an optional
// negation, a waive word, an id spelling, a tail), each in one of the row shapes
// the program must tell apart, then the shapes that need hand-built lines:
// deep nesting at jq's limit and invalid UTF-8 in a line or an argument.
func waiverCorpus() []waiverCase {
	r := rand.New(rand.NewPCG(2683, 945))
	ids := []string{"AC3", "ac3", "3", "AC-3", "ac3.x", "é1", "a(b", `a\b`, "AC3É"}
	pick := func(xs []string) string { return xs[r.IntN(len(xs))] }
	var out []waiverCase
	add := func(shape, id string, log ...string) {
		out = append(out, waiverCase{shape: shape, id: id, crew: crew, from: from, log: log})
	}
	const k = `,"k":[1,1.50,null,true,1e2,0.00000001,1E400,-0,12.30e-5,0e5]`
	for n := range 4000 {
		id := pick(ids)
		var b strings.Builder
		if n%2 == 0 {
			for range 1 + r.IntN(10) {
				b.WriteString(pick(waiverWords))
				if r.IntN(2) == 0 {
					b.WriteString(" ")
				}
			}
			if r.IntN(3) == 0 {
				b.WriteString(" " + strings.ToUpper(id))
			}
		} else {
			b.WriteString(pick([]string{"", "", "I will not ", "we won’t ", "never ", "no, ", "nothing to ", "cannot ", "Please ", "ok. ", "don't ", "not now but "}))
			b.WriteString(pick([]string{"waive", "Waived", "WAIVING", "waiver for", "waives", "éwaive", "waivex", "rewaive", "waive_", "waiv"}))
			b.WriteString(pick([]string{" ", " ", ": ", "\n", " item ", "²", " (", ".", "\x7f", "\x01"}))
			b.WriteString(pick([]string{id, strings.ToUpper(id), strings.ToLower(id), id + ".", id + "x", "x" + id, "-" + id, id + "-", "_" + id, "(" + id + ")", "ac4"}))
			b.WriteString(pick([]string{"", "", ".", "! thanks", " but not ac4", "; no", " now", "\n", ", not waiving ac4", " ok?"}))
		}
		body := b.String()
		str := jsonStr(body)
		switch r.IntN(16) {
		case 0, 1, 2, 3:
			shape := "object-body"
			if strings.ContainsAny(body, "\x7f\x01\x1f") {
				shape = "object-control"
			}
			add(shape, id, "garbage", rawRow(crew, "dispatcher:"+crew, from, `{"n":`+str+k+`}`))
		case 4:
			add("other-session", id, "garbage", row(crew, "dispatcher:"+crew, "worker:feat/x#s1", body))
		case 5:
			add("body-false", id, "garbage", rawRow(crew, "dispatcher:"+crew, from, "false"))
		case 6:
			add("trailing-object", id, "garbage", reply(body)+" {}")
		case 7:
			var nested string
			switch r.IntN(4) {
			case 0:
				nested = `{"n":` + str + `,"n":"x"}`
			case 1:
				nested = `{"n":"x","n":` + str + `}`
			case 2:
				nested = `{"a":{"n":` + str + `,"n":"x"}}`
			default:
				nested = `{"a":{"n":"x","n":` + str + `}}`
			}
			add("dup-key", id, "garbage", rawRow(crew, "dispatcher:"+crew, from, nested))
		case 8:
			esc := pick([]string{`\ud800`, `\udc00`, `\ud800A`, `\ud83d\ude00`})
			in := str[1 : len(str)-1]
			at := max(strings.LastIndex(asciiLower(in), asciiLower(inner(id))), 0)
			if r.IntN(2) == 0 {
				at = 0
			}
			lit := `"` + in[:at] + esc + in[at:] + `"`
			if r.IntN(3) == 0 {
				lit = `{"n":` + lit + `}`
			}
			add("surrogate", id, "garbage", rawRow(crew, "dispatcher:"+crew, from, lit))
		case 9:
			line := reply(body)
			if r.IntN(2) == 0 {
				line = rawRow(crew, "dispatcher:"+crew, from, `{"n":`+str+`}`)
			}
			add("bom", id, "garbage", "\xef\xbb\xbf"+line)
		default:
			add("plain", id, "garbage", reply(body))
		}
	}
	// The row object takes 2 of jq's 10000 stack slots, so 9998 arrays or 4999
	// objects still parse and one more does not.
	for _, phrase := range []string{"waive AC3", "do not waive AC3", "waive ac3 now", "waive AC3x"} {
		for depth := 9995; depth <= 10000; depth++ {
			add("deep-array", "AC3", rawRow(crew, "dispatcher:"+crew, from, strings.Repeat("[", depth)+jsonStr(phrase)+strings.Repeat("]", depth)))
		}
		for depth := 4996; depth <= 5000; depth++ {
			add("deep-object", "AC3", rawRow(crew, "dispatcher:"+crew, from, strings.Repeat(`{"a":`, depth)+jsonStr(phrase)+strings.Repeat("}", depth)))
		}
	}
	// A repaired spelling in the row can match the argument jq repairs.
	esc := func(s string) string { return strings.ReplaceAll(s, "�", `\ufffd`) }
	for n := range 600 {
		p := badBytes[r.IntN(len(badBytes))]
		fixed := p.fixed
		if r.IntN(3) == 0 {
			fixed = "�"
		}
		c := waiverCase{id: "AC3", crew: crew, from: from}
		sender := "dispatcher:" + crew
		to, cid, text := from, crew, "waive AC3"
		switch n % 4 {
		case 0:
			c.shape = "invalid-body"
			pre := pick([]string{"", "waive ", "x ", "waive AC3 ", "will not "})
			post := pick([]string{" waive AC3", "waive AC3", " AC3", "AC3 waive", ""})
			bodyJSON := `"` + inner(pre) + p.bad + inner(post) + `"`
			line := rawRow(crew, sender, to, bodyJSON)
			if r.IntN(4) == 0 {
				line = rawRow(crew, sender, to, jsonStr(text))
				line = line[:len(line)-1] + `,"note":"` + p.bad + `"}`
			}
			c.log = []string{"garbage", line}
			if r.IntN(4) == 0 {
				c.log = []string{"garbage" + p.bad, rawRow(crew, sender, to, jsonStr(text))}
			}
		case 1:
			c.shape = "invalid-from"
			c.from = "w" + p.bad
			to = "w" + fixed
		case 2:
			c.shape = "invalid-id"
			c.id = pick([]string{"AC", "x", "ac-"}) + p.bad
			text = "waive " + pick([]string{"AC", "ac", "x", "ac-"}) + fixed + pick([]string{"", ".", " ok"})
		default:
			c.shape = "invalid-crew"
			c.crew = "c" + p.bad
			cid = "c" + fixed
			sender = "dispatcher:" + cid
		}
		if c.log == nil {
			line := rawRow(cid, sender, to, jsonStr(text))
			if r.IntN(2) == 0 {
				line = esc(line)
			}
			c.log = []string{"garbage", line}
		}
		out = append(out, c)
	}
	return out
}

func TestJQNumberAgainstJQ(t *testing.T) {
	bin := jqBin(t)
	for _, lit := range []string{"1", "1.50", "1e2", "1E2", "0.00000001", "1.5e-10", "1e1000", "-0", "12.30e-5", "0e5", "-1E400", "123456789012345678901234567890", "0.000001", "100e-2", "1e+2", "0e1000000000", "-0e9999999999", "1e-900000000"} {
		out, rc := runJQ(t, bin, lit, "-c", ".|tostring")
		if rc != 0 {
			t.Fatalf("jq exit %d for %s", rc, lit)
		}
		want, err := strconv.Unquote(strings.TrimSpace(out))
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := jqNumber(lit); !ok || got != want {
			t.Errorf("jqNumber(%s) = %s, jq says %s", lit, got, want)
		}
	}
}
