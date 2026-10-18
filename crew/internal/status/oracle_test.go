package status

import (
	"math/rand/v2"
	"os/exec"
	"strings"
	"testing"
)

// The status arm's original programs, run as the arm ran them: `jq -Rnr` over
// the log, $c crew, $b branch session id, $r reviewer role id, $e engine.
// Output 1 when the seam is there, else 0.
const (
	jqReview = `reduce inputs as $line (
  {ok: false, rejected: false, pending: false};
  (try [$line | fromjson] catch null) as $p
  | if $p == null then
      if $e == "pi" and ($line | test("\\S")) and ($line | contains("\"crew_id\":" + ($c | tojson)) and contains($r | tojson | .[1:-1]))
      then .ok = false | .rejected = true
      else . end
    elif ($p[0] | type) != "object" then .
    else
      $p[0] as $m
      | ($m.crew_id == $c and $m.kind == "msg") as $mine
      | (if $mine then (($m.body | fromjson?) // null) else null end) as $o
      | (($m.from // "") | tostring | sub("#s[^#]*$"; "")) as $f
      | (($m.to // "") | tostring | sub("#s[^#]*$"; "")) as $t
      | if $e == "pi" and $mine and ($o | type) != "object" then
          if $f == $r then .ok = false | .rejected = true
          elif $f == $b and $m.to == $r then .pending = true | .ok = false
          else . end
        elif ($o | type) != "object" then .
        elif $e == "pi" and $f == $r and ($o | has("seam") or has("verdict"))
             and (($o | has("tag") and (has("verdict") | not)) | not) then
          if $o.seam == "review" and $o.verdict == "accept" then
            if $t == $b then .pending = false | .ok = true | .rejected = false else . end
          elif $o.seam == "review" and $o.verdict == "revise" then
            if $t == $b then .pending = false | .ok = false | .rejected = false else .ok = false end
          else .ok = false | .rejected = true end
        elif $e == "pi" and $f == $r
             and (($o | has("seam") or has("verdict")) | not)
             and (($o | has("event") or has("tag")) | not) then
          .ok = false | .rejected = true
        elif $e == "pi" and $f == $b and $m.to == $r
             and (($o | keys) == ["final"] and $o.final == true | not) then
          .pending = true | .ok = false
        elif $o.seam != "review" or ($o | has("tag")) then .
        elif $f == $b and $m.to == ("review:" + $c)
             and (($o | has("review_mode") | not) or ($o.review_mode | IN("full", "downgraded"))) then
          if .rejected or .pending then . else .ok = true end
        else . end
    end
) | if .ok then 1 else 0 end`

	jqDeslop = `first(inputs
  | (try fromjson catch null)
  | select(type == "object" and .crew_id == $c and .kind == "msg"
           and .to == ("review:" + $c)
           and ((.from // "") | tostring | sub("#s[^#]*$"; "")) == $b)
  | (.body | fromjson? // null)
  | select(type == "object" and .seam == "deslop" and (has("tag") | not))
  | 1) // 0`

	jqPlan = `($b | ltrimstr("worker:")) as $n
| reduce inputs as $line (
  {seam: false, resumed: false};
  (try [$line | fromjson] catch null) as $p
  | if $p == null or ($p[0] | type) != "object" then .
    else
      $p[0] as $m
      | if $m.crew_id != $c then .
        elif $m.kind == "dispatch" and $m.branch == $n then .resumed = false | .seam = false
        elif $m.kind == "resume" and $m.branch == $n then .resumed = true
        elif $m.kind == "msg" and $m.to == ("review:" + $c)
             and (($m.from // "") | tostring | sub("#s[^#]*$"; "")) == $b then
          (($m.body | fromjson?) // null) as $o
          | if ($o | type) == "object" and $o.seam == "plan" and ($o | has("tag") | not)
               and ($o.plan_critic_first_pass | IN("accept", "revise", "reject"))
            then .seam = true else . end
        else . end
    end
) | if .seam or .resumed then 1 else 0 end`
)

const otherSession = "worker:feat/x#sY#Z"

var (
	resumeRow   = `{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/x"}`
	dispatchRow = `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x"}`

	rejectBody         = jsonStr(`{"seam":"review","verdict":"reject"}`)
	nanReject          = `{"ts":1,"crew_id":"c1","from":"` + rev + `","to":"` + branch + `","kind":"msg","body":` + rejectBody + `,"n":NaN}`
	nanRejectSpaced    = `{"ts": 1, "crew_id": "c1", "from": "` + rev + `", "to": "` + branch + `", "kind": "msg", "body": ` + rejectBody + `, "n": NaN}`
	nanRejectEscaped   = `{"ts":1,"crew_id":"c1","from":"role:feat\/x:reviewer","to":"` + branch + `","kind":"msg","body":` + rejectBody + `,"n":NaN}`
	nanRejectUnicode   = `{"ts":1,"crew_id":"c1","from":"\u0072ole:feat/x:reviewer","to":"` + branch + `","kind":"msg","body":` + rejectBody + `,"n":NaN}`
	bomRequest         = "\ufeff" + msgRow(worker, rev, `{"seam":"review","request":true}`)
	nanDispatch        = `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x","n":NaN}`
	nanDispatchEscaped = `{"ts":2,"crew_id":"c1","kind":"\u0064ispatch","branch":"feat\/x","n":NaN}`

	// jq refuses nesting past 5000 objects; encoding/json allows 10000.
	deepObjects    = strings.Repeat(`{"a":`, 6000) + "1" + strings.Repeat("}", 6000)
	deepArrays     = strings.Repeat("[", 9999) + strings.Repeat("]", 9999)
	paddedReview   = `{"ts":1,"crew_id":"c1","from":"` + worker + `","to":"review:c1","kind":"msg","body":` + jsonStr(`{"seam":"review","review_mode":"full"}`) + `,"pad":` + deepObjects + `}`
	paddedDeslop   = msgRow(worker, "review:c1", `{"seam":"deslop","pad":`+deepObjects+`}`)
	paddedAccept   = `{"ts":1,"crew_id":"c1","from":"` + rev + `","to":"` + branch + `","kind":"msg","body":` + jsonStr(`{"seam":"review","verdict":"accept"}`) + `,"pad":` + deepObjects + `}`
	paddedResume   = `{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/x","pad":` + deepObjects + `}`
	paddedResume5k = `{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/x","pad":` + strings.Repeat(`{"a":`, 5000) + "1" + strings.Repeat("}", 5000) + `}`
	tornDispatchr  = `{"ts":1,"crew_id":"c1","from":"dispatcher:c1","to":"worker:feat/x#s1-1","kind":"msg","body":"x"`
	nanDispatchr   = tornDispatchr + `,"n":NaN}`
)

// corpus is every shape of line the folds treat differently: the lead's seams,
// a pi reviewer's verdicts and re-requests, and the malformed or odd rows a
// torn append or a stray writer leaves in a log.
var corpus = []string{
	msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full"}`),
	msgRow(worker, "review:c1", `{"seam":"review","review_mode":"downgraded"}`),
	msgRow(worker, "review:c1", `{"seam":"review","review_mode":"weird"}`),
	msgRow(worker, "review:c1", `{"seam":"review","tag":"note"}`),
	msgRow("worker:feat/y#s1-1", "review:c1", `{"seam":"review"}`),
	msgRow(worker, "review:c1", `{"seam":"deslop"}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","tag":"note"}`),
	msgRow(worker, "review:c2", `{"seam":"deslop"}`),
	msgRow(worker, rev, `{"seam":"review","request":true}`),
	msgRow(worker, rev, `{"final":true}`),
	msgRow(worker, rev, `{"final":true,"x":1}`),
	msgRow(worker, rev, `{"final":false}`),
	msgRow(worker, rev, `plain text`),
	msgRow(rev, branch, `{"seam":"review","verdict":"accept"}`),
	msgRow(rev+"#s2-3", branch, `{"seam":"review","verdict":"accept"}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"revise"}`),
	msgRow(rev, "worker:feat/y", `{"seam":"review","verdict":"accept"}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"reject"}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"maybe"}`),
	msgRow(rev, branch, `{"verdict":"accept"}`),
	msgRow(rev, branch, `{"seam":"review","tag":"note"}`),
	msgRow(rev, branch, `{"seam":"review","tag":"note","verdict":"accept"}`),
	msgRow(rev, branch, `{"event":"role_exited"}`),
	msgRow(rev, branch, `{"note":"hi"}`),
	msgRow(rev, branch, `not json`),
	msgRow(rev, branch, `{"seam":"review","verdict":NaN}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"accept"} {"x":1}`),
	msgRow(rev, branch, `[1,2]`),
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + rev + `","to":"` + branch + `","body":{"seam":"review","verdict":"accept"}}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + worker + `","to":"review:c1","body":null}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":5,"to":"review:c1","body":"{\"seam\":\"deslop\"}"}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + worker + `"}`,
	`{"ts":1,"crew_id":"c2","kind":"msg","from":"` + rev + `","to":"` + branch + `","body":"{\"seam\":\"review\",\"verdict\":\"reject\"}"}`,
	statusRow(worker, `{"state":"working"}`),
	`{"crew_id":"c1","from":"` + rev,
	`{"crew_id":"c1","from":"` + worker,
	`garbage`,
	``,
	`   `,
	`NaN`,
	`nan`,
	`5`,
	`null`,
	`true`,
	`"str"`,
	`[1,2]`,
	`[]`,
	`{"a":1} {"b":2}`,
	`{"crew_id":"c1","kind":"msg"} trailing`,
	// Surrogate escapes: jq's fromjson errors on a high one not followed by a
	// low one, and decodes a lone low one to U+FFFD.
	msgRow(worker, "review:c1", `{"seam":"deslop","note":"\ud83d"}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","note":"\udc00"}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","note":"\ud83d\u0041"}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","note":"\udc00\ud83d"}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","note":"\ud83d\ude00"}`),
	msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full","note":"\ud83d"}`),
	msgRow(worker, "review:c1", `{"seam":"review","review_mode":"full","note":"\udc00"}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"accept","note":"\ud83d"}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"accept","note":"\udc00"}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"revise","note":"\ud83dx"}`),
	msgRow(worker, rev, `{"final":true,"note":"\ud83d"}`),
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + worker + `","to":"review:c1","body":"{\"seam\":\"deslop\"}","x":"\ud83d"}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + worker + `","to":"review:c1","body":"{\"seam\":\"deslop\"}","x":"\udc00"}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + worker + `","to":"review:c1","body":"{\"seam\":\"review\"}","x":"\ud83d"}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + rev + `","to":"` + branch + `","body":"{\"seam\":\"review\",\"verdict\":\"accept\"}","x":"\ud83d"}`,
	`{"ts":1,"crew_id":"c1","kind":"msg","from":"` + rev + `","to":"` + branch + `","body":"{\"seam\":\"review\",\"verdict\":\"accept\"}","x":"\udc00"}`,
	// Literal edges of fromjson, in the line and in the body.
	`Infinity`, `-Infinity`, `nan1`, `nan0`, `snan`, `1e1000`, `100000000000000000000000001`,
	`  {"crew_id":"c1","kind":"msg"}  `, `1 2`, `{} x`,
	"\ufeff{\"crew_id\":\"c1\"}",
	"{\"ts\":1,\"crew_id\":\"c1\",\"kind\":\"msg\",\"from\":\"" + worker + "\",\"to\":\"review:c1\",\"body\":\"{\\\"seam\\\":\\\"deslop\\\",\\\"n\\\":\\\"\xff\\\"}\"}",
	"{\"ts\":1,\"crew_id\":\"c1\",\"kind\":\"msg\",\"from\":\"\xff" + worker + "\",\"to\":\"review:c1\",\"body\":\"{\\\"seam\\\":\\\"deslop\\\"}\"}",
	msgRow(worker, "review:c1", ``),
	msgRow(worker, "review:c1", `   `),
	msgRow(worker, "review:c1", ` {"seam":"deslop"} `),
	msgRow(worker, "review:c1", `{"seam":"deslop"} {"x":1}`),
	msgRow(worker, "review:c1", `{"seam":"deslop"} x`),
	msgRow(worker, "review:c1", `{"seam":"deslop","n":NaN}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","n":nan1}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","n":Infinity}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","n":1e1000}`),
	msgRow(worker, "review:c1", `{"seam":"deslop","n":100000000000000000000000001}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"accept","n":nan1}`),
	msgRow(rev, branch, `{"seam":"review","verdict":"accept","n":-nan}`),
	msgRow(rev, branch, ``),
	msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`),
	msgRow(worker, "review:c1", `{"seam":"plan","plan_critic_first_pass":"maybe"}`),
	resumeRow,
	dispatchRow,
	// A from or to whose last `#` is not a session suffix names another id.
	msgRow(otherSession, "review:c1", `{"seam":"review","review_mode":"full"}`),
	msgRow(otherSession, "review:c1", `{"seam":"deslop"}`),
	msgRow(otherSession, "review:c1", `{"seam":"plan","plan_critic_first_pass":"accept"}`),
	msgRow(otherSession, rev, `{"seam":"review","request":true}`),
	msgRow(rev+"#sY#Z", branch, `{"seam":"review","verdict":"reject"}`),
	msgRow(rev+"#sY#Z", branch, `{"seam":"review","verdict":"accept"}`),
	msgRow(rev, otherSession, `{"seam":"review","verdict":"accept"}`),
	msgRow(rev, otherSession, `{"seam":"review","verdict":"revise"}`),
	// Revoking rows only jq decodes (NaN, a BOM), raw, spaced and escaped.
	nanReject,
	nanRejectSpaced,
	nanRejectEscaped,
	nanRejectUnicode,
	bomRequest,
	nanDispatch,
	nanDispatchEscaped,
	// Nesting deeper than jq parses, in the line and in the body.
	paddedReview,
	paddedDeslop,
	msgRow(worker, "review:c1", `{"seam":"deslop","pad":`+deepArrays+`}`),
	paddedAccept,
	paddedResume,
	paddedResume5k,
	// A dispatcher msg to the worker that merely names crew, dispatch and branch.
	tornDispatchr,
	nanDispatchr,
}

// TestGatesNeverAcceptMoreThanJQ holds the Go folds to the hard rule: a gate
// never accepts what the arm's jq programs refuse. Go may refuse more only
// where it cannot decode a line, or a msg body, that jq parses.
func TestGatesNeverAcceptMoreThanJQ(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not on PATH")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	var seqs [][]string
	for _, l := range corpus {
		seqs = append(seqs, []string{l})
	}
	seqs = append(seqs, nil)
	for range 150 {
		seq := make([]string, 2+rng.IntN(5))
		for i := range seq {
			seq[i] = corpus[rng.IntN(len(corpus))]
		}
		seqs = append(seqs, seq)
	}
	accept := msgRow(rev, branch, `{"seam":"review","verdict":"accept"}`)
	seqs = append(seqs,
		[]string{corpus[0], corpus[5]},
		[]string{corpus[13], corpus[5]},
		[]string{corpus[13], corpus[8], corpus[5]},
		[]string{corpus[8], corpus[13], corpus[5]},
		[]string{corpus[13], corpus[9], corpus[5]},
		[]string{corpus[17], corpus[0], corpus[5]},
		[]string{corpus[17], corpus[15], corpus[0]},
		[]string{corpus[13], corpus[20], corpus[22], corpus[5]},
		[]string{corpus[0], corpus[17], corpus[5]},
		[]string{corpus[0], corpus[36], corpus[5]},
		// Each revoking row only jq decodes, after a row that grants.
		[]string{accept, nanReject, deslopSeam},
		[]string{accept, nanRejectSpaced, deslopSeam},
		[]string{accept, nanRejectEscaped, deslopSeam},
		[]string{accept, nanRejectUnicode, deslopSeam},
		[]string{accept, bomRequest, deslopSeam},
		[]string{reviewSeam, bomRequest, deslopSeam},
		[]string{planSeam, nanDispatch},
		[]string{planSeam, nanDispatchEscaped},
		[]string{resumeRow, nanDispatch},
		[]string{resumeRow, nanDispatchEscaped},
		[]string{planSeam, tornDispatchr},
		[]string{resumeRow, nanDispatchr},
		[]string{deslopSeam, paddedReview},
		[]string{paddedAccept, deslopSeam},
	)

	folds := []struct {
		name    string
		jq      string
		fold    func([]string, seamKeys) bool
		engines []string
	}{
		{"review", jqReview, foldReview, []string{"pi", "claude"}},
		{"deslop", jqDeslop, foldDeslop, []string{"claude"}},
		{"plan", jqPlan, foldPlan, []string{"claude"}},
	}
	for _, f := range folds {
		for _, engine := range f.engines {
			t.Run(f.name+"/"+engine, func(t *testing.T) {
				t.Parallel()
				k := seamKeys{crew: crewID, branch: branch, reviewer: rev, pi: engine == "pi"}
				for _, seq := range seqs {
					want := runOracle(t, jq, f.jq, engine, seq)
					got := f.fold(seq, k)
					switch {
					case got && !want:
						t.Errorf("accepts what jq refuses: lines %q", seq)
					case !got && want && !undecodable(seq):
						t.Errorf("refuses what jq accepts with every line decodable: lines %q", seq)
					}
				}
			})
		}
	}
}

// undecodable reports a line, or a msg body, that fromJSON refuses.
func undecodable(lines []string) bool {
	for _, l := range lines {
		v, ok := fromJSON(l)
		if !ok {
			return true
		}
		m, _ := v.(map[string]any)
		if s, isString := m["body"].(string); isString {
			if _, ok := fromJSON(s); !ok {
				return true
			}
		}
	}
	return false
}

func runOracle(t *testing.T, jq, prog, engine string, lines []string) bool {
	t.Helper()
	cmd := exec.Command(jq, "-Rnr", "--arg", "c", crewID, "--arg", "b", branch, "--arg", "r", rev, "--arg", "e", engine, prog)
	if len(lines) > 0 {
		cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq on %q: %v", lines, err)
	}
	switch got := strings.TrimSpace(string(out)); got {
	case "1":
		return true
	case "0":
		return false
	default:
		t.Fatalf("jq on %q printed %q", lines, got)
		return false
	}
}
