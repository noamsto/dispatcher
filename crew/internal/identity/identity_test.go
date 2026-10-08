package identity

import (
	"bufio"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

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
		if got := cksum([]byte(c.branch)); got != c.cksum {
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
		if got := cksum([]byte(branch)); uint64(got) != n {
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
	rec := events(t, `{"kind":"dispatch","branch":"feat/x","name":"zed","color":"red","tmux":"colour9"}`)
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
		if got := compact(For(events(t, src), b)); got != fallback(b) {
			t.Errorf("For(%s) = %s, want pool %s", src, got, fallback(b))
		}
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

// TestPoolsMatchCrewSh guards the Go pools against drift from the bash arrays
// until crew.sh's identity helpers are deleted. The nix build sandbox holds
// only ./crew, so the script is absent there.
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
