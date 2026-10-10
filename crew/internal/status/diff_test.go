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
// documented patch to the programs: `$lines | <prog>`.
func runJQ(t *testing.T, jq, prog, engine string, lines []string) string {
	t.Helper()
	arr := make([]jsonv.Value, len(lines))
	for i, l := range lines {
		arr[i] = jsonv.Str(l)
	}
	linesJSON := string(jsonv.Append(nil, jsonv.Array(arr...), jsonv.Options{}))
	cmd := exec.Command(jq, "-nc", "--argjson", "lines", linesJSON,
		"--arg", "c", crewID, "--arg", "b", branch, "--arg", "r", rev, "--arg", "e", engine,
		"$lines | "+prog)
	out, err := cmd.Output()
	if err != nil {
		return "error"
	}
	return strings.TrimSpace(string(out))
}

// runJQOriginal undoes the two patches and reads the lines the arm's way,
// `jq -Rnr` over the log: the check that the patch changed only how lines arrive.
func runJQOriginal(t *testing.T, jq, prog, engine string, lines []string) string {
	t.Helper()
	orig := strings.NewReplacer("reduce .[] as $line", "reduce inputs as $line", "first(.[]", "first(inputs").Replace(prog)
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
