package identity

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

const crewScript = "../../../adapters/core/crew.sh"

var longBranch = "feat/" + strings.Repeat("0123456789", 29) + "01234"

// slotCases hold cksum values computed in the shell with
// `printf '%s' "$b" | cksum | cut -d' ' -f1`; the slot is that value mod 32.

var slotCases = []struct {
	branch string
	cksum  uint32
	slot   int
}{
	{"feat/x", 3862336945, 17},
	{"feat/207-a", 1492401910, 22},
	{"eng-6789-b", 3661012846, 14},
	{"a", 1220704766, 30},
	{"", 4294967295, 31},
	{"über/ü", 2932099393, 1},
	{longBranch, 802914209, 1},
}

func TestCksumAndSlot(t *testing.T) {
	for _, c := range slotCases {
		if got := CKsum([]byte(c.branch)); got != c.cksum {
			t.Errorf("cksum(%.12q) = %d, want %d", c.branch, got, c.cksum)
		}
		if got := Slot(c.branch); got != c.slot {
			t.Errorf("Slot(%.12q) = %d, want %d", c.branch, got, c.slot)
		}
	}
}

func TestSlotMatchesCksumBinary(t *testing.T) {
	bin, err := exec.LookPath("cksum")
	if err != nil {
		t.Skip("cksum not installed")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-/_.üé"
	runes := []rune(alphabet)
	for range 200 {
		b := make([]rune, rng.IntN(400))
		for i := range b {
			b[i] = runes[rng.IntN(len(runes))]
		}
		branch := string(b)
		cmd := exec.Command(bin)
		cmd.Stdin = strings.NewReader(branch)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseUint(strings.Fields(string(out))[0], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		if got := CKsum([]byte(branch)); uint64(got) != n {
			t.Fatalf("cksum(%q) = %d, cksum binary says %d", branch, got, n)
		}
	}
}

func TestAt(t *testing.T) {
	got := string(jsonv.Append(nil, At(0), jsonv.Options{}))
	if want := `{"name":"sage","color":"green","tmux":"colour28"}`; got != want {
		t.Errorf("At(0) = %s, want %s", got, want)
	}
	got = string(jsonv.Append(nil, At(31), jsonv.Options{}))
	if want := `{"name":"cobalt","color":"royalblue","tmux":"colour68"}`; got != want {
		t.Errorf("At(31) = %s, want %s", got, want)
	}
}

func events(t *testing.T, src string) []jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }

// fallback is the pool identity the branch hashes to.
func fallback(branch string) string { return compact(At(Slot(branch))) }

func TestRecorded(t *testing.T) {
	const b = "feat/x"
	tests := []struct {
		name   string
		events string
		want   string // "" means no recorded identity
	}{
		{"recorded wins", `{"kind":"dispatch","branch":"feat/x","name":"zed","color":"red","tmux":"colour9"}`,
			`{"name":"zed","color":"red","tmux":"colour9"}`},
		{"extra fields are dropped and key order is fixed", `{"tmux":"colour9","kind":"dispatch","name":"zed","branch":"feat/x","color":"red","crew_id":"c1"}`,
			`{"name":"zed","color":"red","tmux":"colour9"}`},
		{"absent color is null", `{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":"colour9"}`,
			`{"name":"zed","color":null,"tmux":"colour9"}`},
		{"color passes through as any json", `{"kind":"dispatch","branch":"feat/x","name":"zed","color":{"a":[1,2]},"tmux":"colour9"}`,
			`{"name":"zed","color":{"a":[1,2]},"tmux":"colour9"}`},
		{"latest valid dispatch wins", `{"kind":"dispatch","branch":"feat/x","name":"old","tmux":"colour1"}
{"kind":"dispatch","branch":"feat/x","name":"new","tmux":"colour2"}`,
			`{"name":"new","color":null,"tmux":"colour2"}`},
		{"latest invalid is skipped", `{"kind":"dispatch","branch":"feat/x","name":"old","tmux":"colour1"}
{"kind":"dispatch","branch":"feat/x","name":"Bad","tmux":"colour2"}`,
			`{"name":"old","color":null,"tmux":"colour1"}`},
		{"other crew still counts", `{"kind":"dispatch","crew_id":"c2","branch":"feat/x","name":"zed","tmux":"colour9"}`,
			`{"name":"zed","color":null,"tmux":"colour9"}`},
		{"different branch ignored", `{"kind":"dispatch","branch":"feat/y","name":"zed","tmux":"colour9"}`, ""},
		{"non-dispatch kind ignored", `{"kind":"status","branch":"feat/x","name":"zed","tmux":"colour9"}`, ""},
		{"non-string kind ignored", `{"kind":["dispatch"],"branch":"feat/x","name":"zed","tmux":"colour9"}`, ""},
		{"non-string branch ignored", `{"kind":"dispatch","branch":["feat/x"],"name":"zed","tmux":"colour9"}`, ""},
		{"invalid name", `{"kind":"dispatch","branch":"feat/x","name":"Zed","tmux":"colour9"}`, ""},
		{"name starting with digit", `{"kind":"dispatch","branch":"feat/x","name":"1ed","tmux":"colour9"}`, ""},
		{"empty name", `{"kind":"dispatch","branch":"feat/x","name":"","tmux":"colour9"}`, ""},
		{"name with trailing newline", `{"kind":"dispatch","branch":"feat/x","name":"zed\n","tmux":"colour9"}`, ""},
		{"non-string name", `{"kind":"dispatch","branch":"feat/x","name":7,"tmux":"colour9"}`, ""},
		{"null name", `{"kind":"dispatch","branch":"feat/x","name":null,"tmux":"colour9"}`, ""},
		{"absent name", `{"kind":"dispatch","branch":"feat/x","tmux":"colour9"}`, ""},
		{"invalid tmux", `{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":"red"}`, ""},
		{"tmux without digits", `{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":"colour"}`, ""},
		{"tmux with trailing newline", `{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":"colour9\n"}`, ""},
		{"non-string tmux", `{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":9}`, ""},
		{"absent tmux", `{"kind":"dispatch","branch":"feat/x","name":"zed"}`, ""},
		{"null and non-object events are skipped", `null
{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":"colour9"}
[1]
"x"
7`,
			`{"name":"zed","color":null,"tmux":"colour9"}`},
		{"no events", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Recorded(events(t, tc.events), b)
			if tc.want == "" {
				if ok {
					t.Fatalf("Recorded = %s, want none", compact(got))
				}
				return
			}
			if !ok {
				t.Fatalf("Recorded = none, want %s", tc.want)
			}
			if c := compact(got); c != tc.want {
				t.Errorf("Recorded = %s, want %s", c, tc.want)
			}
		})
	}
}

func TestFor(t *testing.T) {
	const b = "feat/x"
	rec := RecordedAll(events(t, `{"kind":"dispatch","branch":"feat/x","name":"zed","color":"red","tmux":"colour9"}`))
	if got, want := compact(For(rec, b)), `{"name":"zed","color":"red","tmux":"colour9"}`; got != want {
		t.Errorf("For recorded = %s, want %s", got, want)
	}
	for _, src := range []string{
		``,
		`{"kind":"dispatch","branch":"feat/x","name":"Bad","tmux":"colour9"}`,
		`{"kind":"dispatch","branch":"feat/x","name":"zed","tmux":"nope"}`,
		`{"kind":"dispatch","branch":"feat/x","name":5,"tmux":"colour9"}`,
		`{"kind":"dispatch","branch":"feat/y","name":"zed","tmux":"colour9"}`,
	} {
		if got := compact(For(RecordedAll(events(t, src)), b)); got != fallback(b) {
			t.Errorf("For(%s) = %s, want pool %s", src, got, fallback(b))
		}
	}
}

// TestRecordedAll cross-checks the one-pass map against the per-branch
// reference implementation on a mixed bus.
func TestRecordedAll(t *testing.T) {
	src := `
{"kind":"dispatch","branch":"feat/x","name":"zed","color":"red","tmux":"colour9"}
{"kind":"dispatch","branch":"feat/y","name":"old","tmux":"colour1"}
{"kind":"status","from":"worker:feat/y","body":{}}
5
{"kind":"dispatch","branch":"feat/y","name":"new","tmux":"colour2"}
{"kind":"dispatch","branch":"feat/z","name":"Bad","tmux":"colour3"}
{"kind":"dispatch","branch":"feat/z","name":"kai","color":"teal","tmux":"colour4"}
`
	evs := events(t, src)
	all := RecordedAll(evs)
	for _, b := range []string{"feat/x", "feat/y", "feat/z", "feat/none"} {
		want, ok := Recorded(evs, b)
		got, has := all[b]
		if ok != has || (ok && compact(got) != compact(want)) {
			t.Errorf("RecordedAll[%s] = %v %s, want %v %s", b, has, compact(got), ok, compact(want))
		}
	}
	if _, has := all["feat/none"]; has {
		t.Errorf("RecordedAll gained an unrecorded branch")
	}
}

func TestPoolsHaveThirtyTwoSlots(t *testing.T) {
	if len(names) != poolSize || len(colors) != poolSize || len(tmuxc) != poolSize {
		t.Fatalf("pool sizes %d/%d/%d, want %d", len(names), len(colors), len(tmuxc), poolSize)
	}
}

// bashArray returns the words of the multi-line `name=( ... )` array in script.
func bashArray(t *testing.T, script, name string) []string {
	t.Helper()
	f, err := os.Open(script)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var body strings.Builder
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !in {
			rest, ok := strings.CutPrefix(line, name+"=(")
			if !ok {
				continue
			}
			in, line = true, rest
		}
		end := strings.Contains(line, ")")
		body.WriteString(strings.TrimSuffix(strings.TrimSpace(line), ")") + " ")
		if end {
			return strings.Fields(body.String())
		}
	}
	t.Fatalf("array %s not found in %s", name, script)
	return nil
}

// TestPoolsMatchCrewSh guards the Go pools against drift from the bash arrays.
// crew.sh has no identity helper left, so this is what keeps the pool order a
// recorded identity depends on. The nix build sandbox holds only ./crew, so the
// script is absent there.
func TestPoolsMatchCrewSh(t *testing.T) {
	if _, err := os.Stat(crewScript); err != nil {
		t.Skipf("%s not present: %v", crewScript, err)
	}
	for _, p := range []struct {
		bash string
		want []string
	}{
		{"_names", names[:]},
		{"_colors", colors[:]},
		{"_tmuxc", tmuxc[:]},
	} {
		got := bashArray(t, crewScript, p.bash)
		if strings.Join(got, " ") != strings.Join(p.want, " ") {
			t.Errorf("%s drifted from crew.sh:\n bash: %v\n go:   %v", p.bash, got, p.want)
		}
	}
}

// rows reads src the way the arm reads the bus, so the fold tests work over the
// same events Run works over.
func rows(t *testing.T, src string) []bus.Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := bus.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// branchAtSlot names a branch that hashes to slot, so a test can hand a rival its
// own hash codename without hardcoding the pool.
func branchAtSlot(t *testing.T, slot int) string {
	t.Helper()
	for i := range 5000 {
		if b := fmt.Sprintf("feat/rival-%d", i); Slot(b) == slot {
			return b
		}
	}
	t.Fatalf("no branch hashes to slot %d", slot)
	return ""
}

// pool is the identity of a codename, for a want that names one.
func pool(t *testing.T, name string) string {
	t.Helper()
	for i, n := range names {
		if n == name {
			return compact(At(i))
		}
	}
	t.Fatalf("%q is not a pool codename", name)
	return ""
}

func TestAssign(t *testing.T) {
	const branch, crew = "feat/x", "c1"
	slot := Slot(branch)
	own := names[slot]
	next := names[(slot+1)%poolSize]
	third := names[(slot+2)%poolSize]
	// rival hashes to the slot the stepping reaches once our own is taken, so its
	// hash codename is `next`.
	rival := branchAtSlot(t, (slot+1)%poolSize)

	// dispatch is a rival of this crew, dispatched before us, holding name. An
	// empty name is a row that recorded none, as pre-#470 rows did.
	dispatch := func(name string, ts int) string {
		field := ""
		if name != "" {
			field = fmt.Sprintf(`"name":"%s",`, name)
		}
		return fmt.Sprintf(`{"kind":"dispatch","crew_id":"%s","branch":"%s",%s"tmux":"colour1","ts":%d}`,
			crew, rival, field, ts)
	}
	status := func(from, state string, ts int) string {
		return fmt.Sprintf(`{"kind":"status","crew_id":"%s","from":"%s","body":{"state":"%s"},"ts":%d}`,
			crew, from, state, ts)
	}
	// ours is a dispatch of our own: it sets the cut-off the rival is measured
	// against, and records no identity, so what follows is the stepping.
	ours := func(ts int) string {
		return fmt.Sprintf(`{"kind":"dispatch","crew_id":"%s","branch":"%s","tmux":"colour2","ts":%d}`,
			crew, branch, ts)
	}

	tests := []struct {
		name      string
		log       string
		windows   string
		tmuxFails bool
		emptyCrew bool   // ask with the empty crew `crew identity <b>` can resolve to
		wantName  string // a pool codename, the usual answer…
		wantID    string // …or the full identity, for a recorded one
	}{
		{
			name: "an empty bus takes the hash slot",
			// An empty file is still a bus: `jq -s` slurps zero events.
			log: "\n", wantName: own,
		},
		{
			name: "our own dispatch row never occupies our slot",
			log:  ours(100), wantName: own,
		},
		{
			name:   "the recorded identity is reused",
			log:    `{"kind":"dispatch","crew_id":"c1","branch":"feat/x","name":"zed","color":"red","tmux":"colour9","ts":100}`,
			wantID: `{"name":"zed","color":"red","tmux":"colour9"}`,
		},
		{
			name:   "a recorded name no live worker holds beats the hash slot",
			log:    dispatch(own, 100) + "\n" + `{"kind":"dispatch","crew_id":"c1","branch":"feat/x","name":"zed","tmux":"colour9","ts":200}`,
			wantID: `{"name":"zed","color":null,"tmux":"colour9"}`,
		},
		{
			name:     "a recorded name a live worker holds is dropped for the hash slot",
			log:      dispatch("zed", 100) + "\n" + `{"kind":"dispatch","crew_id":"c1","branch":"feat/x","name":"zed","tmux":"colour9","ts":200}`,
			wantName: own,
		},
		{
			name: "the hash slot steps forward past a live worker",
			log:  dispatch(own, 100), wantName: next,
		},
		{
			name: "a terminal worker frees the codename it held",
			log:  dispatch(own, 100) + "\n" + status("worker:"+rival, "done", 300), wantName: own,
		},
		{
			name: "a live worker holds it",
			log:  dispatch(own, 100) + "\n" + status("worker:"+rival, "working", 300), wantName: next,
		},
		{
			name: "the newest status decides, not the first",
			log: dispatch(own, 100) + "\n" + status("worker:"+rival, "working", 300) + "\n" +
				status("worker:"+rival, "exited", 400), wantName: own,
		},
		{
			name: "an older terminal status does not free it",
			log: dispatch(own, 100) + "\n" + status("worker:"+rival, "failed", 300) + "\n" +
				status("worker:"+rival, "working", 400), wantName: next,
		},
		{
			name: "the session suffix of a worker id is not part of its branch",
			log:  dispatch(own, 100) + "\n" + status("worker:"+rival+"#s1-1", "reaped", 300), wantName: own,
		},
		{
			name: "a role pane's status never frees a codename",
			log:  dispatch(own, 100) + "\n" + status("role:"+rival+":reviewer", "done", 300), wantName: next,
		},
		{
			name:     "a rival of another crew holds nothing",
			log:      fmt.Sprintf(`{"kind":"dispatch","crew_id":"c2","branch":"feat/z","name":"%s","tmux":"colour1","ts":100}`, own),
			wantName: own,
		},
		{
			// jq's `==` against a `--arg` string is false for null and for a number.
			name:      "a row that recorded no crew_id belongs to no crew, not the empty one",
			log:       fmt.Sprintf(`{"kind":"dispatch","branch":"feat/z","name":"%s","tmux":"colour1","ts":100}`, own),
			emptyCrew: true,
			wantName:  own,
		},
		{
			name:      "a numeric crew_id is no crew either",
			log:       fmt.Sprintf(`{"kind":"dispatch","crew_id":7,"branch":"feat/z","name":"%s","tmux":"colour1","ts":100}`, own),
			emptyCrew: true,
			wantName:  own,
		},
		{
			name: "a rival dispatched after us yields its codename",
			log:  ours(100) + "\n" + dispatch(own, 200), wantName: own,
		},
		{
			name: "with no dispatch of ours every live rival counts",
			log:  dispatch(own, 900), wantName: next,
		},
		{
			name: "the last dispatch row of a rival is the one that counts",
			log: dispatch(own, 100) + "\n" + dispatch(next, 200) + "\n" +
				status("worker:"+rival, "done", 300), wantName: own,
		},
		{
			// The rival holds `next`, its own hash codename, so we step to `third`.
			name:     "a rival that recorded no name counts by its hash slot",
			log:      ours(300) + "\n" + dispatch("", 200),
			windows:  own,
			wantName: third,
		},
		{
			// A number is a token no pool holds: the rival holds nothing.
			name:     "a rival whose name is not a string holds nothing",
			log:      ours(300) + "\n" + `{"kind":"dispatch","crew_id":"c1","branch":"` + rival + `","name":7,"tmux":"colour1","ts":200}`,
			windows:  own,
			wantName: next,
		},
		{
			name:     "a rival that recorded a name frees its hash slot",
			log:      ours(300) + "\n" + dispatch("zed", 200),
			windows:  own,
			wantName: next,
		},
		{
			name:    "a tmux window occupies its codename",
			log:     ours(100),
			windows: own, wantName: next,
		},
		{
			name:      "a window read that fails names nothing",
			log:       ours(100),
			tmuxFails: true, wantName: own,
		},
		{
			name:    "every codename taken falls back to the hash slot",
			log:     ours(100),
			windows: strings.Join(names[:], "\n"), wantName: own,
		},
		{
			name: "the bus gates a recorded name before tmux is asked",
			log:  `{"kind":"dispatch","crew_id":"c1","branch":"feat/x","name":"zed","tmux":"colour9","ts":100}`,
			// The arm returned before it read tmux: the probe is not even installed.
			wantID: `{"name":"zed","color":null,"tmux":"colour9"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Probes{}
			switch {
			case tc.tmuxFails:
				p.Windows = func() (string, error) { return "", errors.New("no server") }
			case tc.windows != "":
				wins := tc.windows
				p.Windows = func() (string, error) { return wins, nil }
			}
			ask := crew
			if tc.emptyCrew {
				ask = ""
			}
			got := compact(Assign(rows(t, tc.log), branch, ask, p))
			want := tc.wantID
			if want == "" {
				want = pool(t, tc.wantName)
			}
			if got != want {
				t.Errorf("Assign = %s, want %s", got, want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	const branch, crew = "feat/x", "c1"
	own := names[Slot(branch)]
	log := func(t *testing.T, src string) bus.Paths {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "events.jsonl")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return bus.Paths{Dir: dir, Log: path}
	}
	run := func(t *testing.T, argv []string, paths bus.Paths, crewID string) (string, string, int) {
		t.Helper()
		var out, errOut strings.Builder
		code := Run(argv, paths, &out, &errOut, Options{CrewID: func() string { return crewID }})
		return out.String(), errOut.String(), code
	}

	t.Run("no branch refuses", func(t *testing.T) {
		out, errOut, code := run(t, nil, bus.Paths{}, crew)
		if code != 1 || errOut != "crew: identity <branch> [crew]\n" || out != "" {
			t.Errorf("Run(nil) = %d %q %q", code, out, errOut)
		}
	})
	t.Run("an empty branch refuses", func(t *testing.T) {
		if _, _, code := run(t, []string{""}, bus.Paths{}, crew); code != 1 {
			t.Errorf("Run(\"\") = %d, want 1", code)
		}
	})
	t.Run("--hash maps a name to its slot", func(t *testing.T) {
		out, errOut, code := run(t, []string{"--hash", "reviewer"}, bus.Paths{}, crew)
		want := compact(At(Slot("reviewer"))) + "\n"
		if code != 0 || out != want || errOut != "" {
			t.Errorf("Run(--hash reviewer) = %d %q %q, want %s", code, out, errOut, want)
		}
	})
	t.Run("--hash without a name refuses", func(t *testing.T) {
		out, errOut, code := run(t, []string{"--hash"}, bus.Paths{}, crew)
		if code != 1 || errOut != "crew: identity --hash <name>\n" || out != "" {
			t.Errorf("Run(--hash) = %d %q %q", code, out, errOut)
		}
	})
	t.Run("the crew defaults to the caller's", func(t *testing.T) {
		rival := branchAtSlot(t, (Slot(branch)+1)%poolSize)
		paths := log(t, fmt.Sprintf(
			`{"kind":"dispatch","crew_id":"%s","branch":"%s","name":"%s","tmux":"colour1","ts":100}`,
			crew, rival, own)+"\n")
		out, _, code := run(t, []string{branch}, paths, crew)
		if code != 0 || out != pool(t, names[(Slot(branch)+1)%poolSize])+"\n" {
			t.Errorf("Run = %d %q, want the next codename", code, out)
		}
	})
	t.Run("no log at all is the pool fallback", func(t *testing.T) {
		out, errOut, code := run(t, []string{branch}, bus.Paths{Log: filepath.Join(t.TempDir(), "absent.jsonl")}, crew)
		if code != 0 || out != pool(t, own)+"\n" || errOut != "" {
			t.Errorf("Run = %d %q %q, want %s", code, out, errOut, pool(t, own))
		}
	})
	t.Run("a log jq could not slurp is the pool fallback", func(t *testing.T) {
		out, _, code := run(t, []string{branch}, log(t, "{not json\n"), crew)
		if code != 0 || out != pool(t, own)+"\n" {
			t.Errorf("Run = %d %q, want %s", code, out, pool(t, own))
		}
	})
}
