package ledger

import (
	"bytes"
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

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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
	return string(jsonv.Append(nil, jsonv.Object(
		jsonv.Member{Key: "ts", Val: jsonv.Num(1)},
		jsonv.Member{Key: "crew_id", Val: jsonv.Str(c)},
		jsonv.Member{Key: "kind", Val: jsonv.Str("msg")},
		jsonv.Member{Key: "from", Val: jsonv.Str(sender)},
		jsonv.Member{Key: "to", Val: jsonv.Str(to)},
		jsonv.Member{Key: "body", Val: jsonv.Str(body)},
	), jsonv.Options{}))
}

func reply(body string) string { return row(crew, "dispatcher:"+crew, from, body) }

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
// lag both, so the id test is pinned on the plain cases and checked one way.
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
	return corpus
}

func TestDifferentialAgainstJQ(t *testing.T) {
	bin := jqBin(t)
	corpus := ledgerCorpus()

	t.Run("ledger", func(t *testing.T) {
		prog := program(t, "ledger.jq")
		var yes atomic.Int64
		parallel(len(corpus), func(i int) {
			d := corpus[i]
			_, rc := runJQ(t, bin, "", "-en", "--arg", "d", d, prog)
			if rc != 0 && rc != 1 {
				t.Errorf("jq exit %d for %q", rc, d)
				return
			}
			if rc == 1 {
				yes.Add(1)
			}
			if want := rc == 1; conforms(d) != want {
				t.Errorf("conforms(%q) = %v, jq says %v", d, !want, want)
			}
		})
		t.Logf("%d details, %d conforming", len(corpus), yes.Load())
	})

	t.Run("items", func(t *testing.T) {
		prog := program(t, "items.jq")
		parallel(len(corpus), func(i int) {
			d := corpus[i]
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
		parallel(len(cases), func(i int) {
			c := cases[i]
			_, rc := runJQ(t, bin, strings.Join(c.log, "\n")+"\n", "-e", "-n", "-R", "--arg", "c", crew, "--arg", "f", from, "--arg", "id", c.id, prog)
			if rc != 0 && rc != 1 {
				t.Errorf("jq exit %d for %q %q", rc, c.id, c.log)
				return
			}
			if rc == 0 {
				yes.Add(1)
			}
			if want := rc == 0; waived(c.log, crew, from, c.id) != want {
				t.Errorf("waived(%q, id %q) = %v, jq says %v", c.log, c.id, !want, want)
			}
		})
		t.Logf("%d logs, %d waived", len(cases), yes.Load())
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
	id  string
	log []string
}

var waiverWords = []string{
	"waive", "waived", "waiver", "waives", "waiving", "waivers", "WAIVE", "not", "never", "no", "cannot", "without",
	"won't", "won’t", "don't", "nothing", "but", "BUT", "butter", "ac3", "AC3", "ac4", "ac33", "ac3-x", "ac3.x", "x",
	" ", " ", " ", ".", "!", "?", ";", "\n", ",", "é", "Ⓐ", "²", "_", "-", "(", ")", ":", "\"", " ", "ࢗ", "3",
}

// waiverCorpus is half word soup and half waive-shaped phrases (an optional
// negation, a waive word, an id spelling, a tail), each in one of the row shapes
// the program must tell apart.
func waiverCorpus() []waiverCase {
	r := rand.New(rand.NewPCG(2683, 945))
	ids := []string{"AC3", "ac3", "3", "AC-3", "ac3.x", "é1", "a(b", `a\b`, "AC3É"}
	pick := func(from []string) string { return from[r.IntN(len(from))] }
	var out []waiverCase
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
			b.WriteString(pick([]string{" ", " ", ": ", "\n", " item ", "²", " (", "."}))
			b.WriteString(pick([]string{id, strings.ToUpper(id), strings.ToLower(id), id + ".", id + "x", "x" + id, "-" + id, id + "-", "_" + id, "(" + id + ")", "ac4"}))
			b.WriteString(pick([]string{"", "", ".", "! thanks", " but not ac4", "; no", " now", "\n", ", not waiving ac4", " ok?"}))
		}
		body := b.String()
		var line string
		switch r.IntN(8) {
		case 0:
			line = `{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":{"n":` + strconv.Quote(body) + `,"k":[1,1.50,null,true]}}`
		case 1:
			line = row(crew, "dispatcher:"+crew, "worker:feat/x#s1", body)
		case 2:
			line = `{"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + from + `","body":false}`
		case 3:
			line = reply(body) + " {}"
		default:
			line = reply(body)
		}
		out = append(out, waiverCase{id: id, log: []string{"garbage", line}})
	}
	return out
}
