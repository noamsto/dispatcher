package proc

import (
	"os"
	"slices"
	"testing"
)

// walkTable scripts the two reads an ancestor walk makes.
type walkTable struct {
	Table
	parent func(int) (int, bool)
	comm   func(int) (string, bool)
}

func (w walkTable) Parent(pid int) (int, bool)  { return w.parent(pid) }
func (w walkTable) Comm(pid int) (string, bool) { return w.comm(pid) }

// OwnerPID is `_owner_pid`: the caller's parent is the shell crew.sh ran under,
// so the walk climbs past the shells to the first ancestor that is not one, and
// falls back to the direct parent when the walk cannot continue.
func TestOwnerPID(t *testing.T) {
	const ppid = 4241
	cases := []struct {
		name   string
		parent map[int]int
		comm   map[int]string
		want   int
	}{
		{
			name:   "the parent is already the owner",
			parent: map[int]int{},
			comm:   map[int]string{ppid: "claude"},
			want:   ppid,
		},
		{
			name:   "shells are climbed past",
			parent: map[int]int{ppid: 4240, 4240: 4239},
			comm:   map[int]string{ppid: "bash", 4240: "zsh", 4239: "claude"},
			want:   4239,
		},
		{
			name:   "ps' path and login-shell dash are stripped",
			parent: map[int]int{ppid: 4240, 4240: 4239},
			comm:   map[int]string{ppid: "/usr/local/bin/dash", 4240: "-ksh", 4239: "claude"},
			want:   4239,
		},
		{
			// The arm's `[ -n "$c" ] || break`: a pid ps cannot name ends the walk.
			name:   "an unanswerable ps falls back",
			parent: map[int]int{ppid: 4240},
			comm:   map[int]string{ppid: "sh"},
			want:   ppid,
		},
		{
			name:   "init ends the walk",
			parent: map[int]int{ppid: 1},
			comm:   map[int]string{ppid: "fish"},
			want:   ppid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := walkTable{parent: func(pid int) (int, bool) { n, ok := tc.parent[pid]; return n, ok },
				comm: func(pid int) (string, bool) { c, ok := tc.comm[pid]; return c, ok }}
			if got := OwnerPID(p, ppid); got != tc.want {
				t.Errorf("OwnerPID(%d) = %d, want %d", ppid, got, tc.want)
			}
		})
	}
}

// The arm bounds the walk (`while [ "$depth" -lt 32 ]`), so a parent chain that
// never resolves — a ps that keeps answering with the same pid — cannot spin it.
// The bound is reported from inside the probe: a lost bound has to fail at once,
// not after the walk spins to the test binary's timeout.
func TestOwnerPIDWalkIsBounded(t *testing.T) {
	calls := 0
	probe := func() {
		calls++
		if calls > 64 {
			t.Fatalf("the walk made %d probes, want at most 64", calls)
		}
	}
	p := walkTable{
		comm:   func(int) (string, bool) { probe(); return "bash", true },
		parent: func(pid int) (int, bool) { probe(); return pid, true },
	}
	if got := OwnerPID(p, 4241); got != 4241 {
		t.Errorf("OwnerPID = %d, want the fallback parent", got)
	}
}

// shellName is the arm's `${c##*/}` then `${c#-}`, in that order: the path goes
// first, so a login shell spelled with its path still reads as one.
func TestShellName(t *testing.T) {
	cases := map[string]string{
		"bash":                "bash",
		"/usr/local/bin/dash": "dash",
		"-zsh":                "zsh",
		"-/bin/bash":          "bash",
		"":                    "",
	}
	for in, want := range cases {
		if got := shellName(in); got != want {
			t.Errorf("shellName(%q) = %q, want %q", in, got, want)
		}
	}
}

// OS.Comm execs `ps`, so this needs a ps binary on PATH: the Nix check phase
// provides one through nativeCheckInputs.
func TestCommNamesOwnProcess(t *testing.T) {
	name, ok := OS{}.Comm(os.Getpid())
	if !ok || name == "" {
		t.Errorf("Comm(own pid) = %q, %v; want a name and true", name, ok)
	}
}

func TestParseCwds(t *testing.T) {
	got := parseCwds("12\t/a/b\n\nx\t/c\n\t34\t/d\t\n")
	want := []Cwd{{12, "/a/b"}, {34, "/d"}}
	if !slices.Equal(got, want) {
		t.Errorf("parseCwds = %v, want %v", got, want)
	}
}

func TestRepresentable(t *testing.T) {
	for dir, want := range map[string]bool{
		"/wt/feat-x":              true,
		"":                        false,
		"/wt/evil\n1\t/wt/feat-x": false,
		"/wt/a\tb":                false,
	} {
		if got := representable(dir); got != want {
			t.Errorf("representable(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]int64{
		"05":         5,
		"01:05":      65,
		"02:01:05":   7265,
		"3-02:01:05": 3*86400 + 7265,
		"-01:05":     65,
		"1-00:00:00": 86400,
	} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("parseEtime(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "1:2:3:4", "5-", "1-2-3", "ab", "1:", "12:3x"} {
		if got, ok := parseEtime(in); ok {
			t.Errorf("parseEtime(%q) = %d, true; want false", in, got)
		}
	}
}
