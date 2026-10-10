package status

import (
	"math/rand/v2"
	"os/exec"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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
	`{"ts":1,"crew_id":"c1","kind":"resume","branch":"feat/x"}`,
	`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x"}`,
}

// TestFoldsMatchJQ runs seam.jq and deslop.jq under the real jq and under the
// Go fold over the corpus, singly, in pairs, and in seeded random sequences.
func TestFoldsMatchJQ(t *testing.T) {
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
		n := 2 + rng.IntN(5)
		seq := make([]string, n)
		for i := range seq {
			seq[i] = corpus[rng.IntN(len(corpus))]
		}
		seqs = append(seqs, seq)
	}
	// Named shapes the arm's comment documents.
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
	)

	progs := []struct {
		name, prog string
		engines    []string
	}{
		{"seam", seamProg, []string{"pi", "claude"}},
		{"deslop", deslopProg, []string{"claude"}},
		{"plan", planProg, []string{"claude"}},
	}
	for _, p := range progs {
		for _, engine := range p.engines {
			t.Run(p.name+"/"+engine, func(t *testing.T) {
				for _, seq := range seqs {
					want := runJQ(t, jq, p.prog, engine, seq)
					got := runGo(p.prog, engine, seq)
					if got != want {
						t.Errorf("lines %q:\n jq %q\n go %q", seq, want, got)
					}
					if orig := runJQOriginal(t, jq, p.prog, engine, seq); orig != want {
						t.Errorf("lines %q:\n patched jq %q\n original jq %q", seq, want, orig)
					}
				}
			})
		}
	}
}

func varsFor(engine string) map[string]jsonv.Value {
	return map[string]jsonv.Value{
		"c": jsonv.Str(crewID),
		"b": jsonv.Str(branch),
		"r": jsonv.Str(rev),
		"e": jsonv.Str(engine),
	}
}

func runGo(prog, engine string, lines []string) string {
	rows := make([]jsonv.Value, len(lines))
	for i, l := range lines {
		rows[i] = jsonv.Str(l)
	}
	v, err := Options{}.withDefaults().Fold(prog, rows, varsFor(engine))
	if err != nil {
		return "error"
	}
	return string(jsonv.Append(nil, v, jsonv.Options{}))
}

// runJQ is the arm's call with the lines handed over as one array, which is the
// documented line patch to the programs: `$lines | <prog>`.
func runJQ(t *testing.T, jq, prog, engine string, lines []string) string {
	t.Helper()
	arr := make([]jsonv.Value, len(lines))
	for i, l := range lines {
		arr[i] = jsonv.Str(l)
	}
	linesJSON := string(jsonv.Append(nil, jsonv.Array(arr...), jsonv.Options{}))
	cmd := exec.Command(jq, "-nc", "--argjson", "lines", linesJSON,
		"--arg", "c", crewID, "--arg", "b", branch, "--arg", "r", rev, "--arg", "e", engine,
		"$lines | "+unpatchFromJSON(prog))
	out, err := cmd.Output()
	if err != nil {
		return "error"
	}
	return strings.TrimSpace(string(out))
}

// unpatchFromJSON hands jq its own fromjson back, the one `_jqfromjson` copies.
func unpatchFromJSON(prog string) string { return strings.ReplaceAll(prog, "_jqfromjson", "fromjson") }

// runJQOriginal undoes the three patches and reads the lines the arm's way,
// `jq -Rnr` over the log: the check that the patch changed only how lines arrive.
func runJQOriginal(t *testing.T, jq, prog, engine string, lines []string) string {
	t.Helper()
	orig := strings.NewReplacer("reduce .[] as $line", "reduce inputs as $line", "first(.[]", "first(inputs").Replace(unpatchFromJSON(prog))
	cmd := exec.Command(jq, "-Rnr", "--arg", "c", crewID, "--arg", "b", branch, "--arg", "r", rev, "--arg", "e", engine, orig)
	if len(lines) > 0 {
		cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	}
	out, err := cmd.Output()
	if err != nil {
		return "error"
	}
	return strings.TrimSpace(string(out))
}

func TestPlanFold(t *testing.T) {
	n := len(corpus)
	planOK, planBad, resume, dispatch := corpus[n-4], corpus[n-3], corpus[n-2], corpus[n-1]
	for _, c := range []struct {
		name string
		seq  []string
		want string
	}{
		{"plan seam", []string{planOK}, "1"},
		{"resume", []string{resume}, "1"},
		{"resume then dispatch", []string{resume, dispatch}, "0"},
		{"seam then dispatch", []string{planOK, dispatch}, "0"},
		{"dispatch then seam", []string{dispatch, planOK}, "1"},
		{"bad value", []string{planBad}, "0"},
	} {
		if got := runGo(planProg, "claude", c.seq); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
