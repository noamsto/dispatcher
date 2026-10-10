package crews

import (
	"os"
	"testing"
)

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
			p := Probes{
				Parent: func(pid int) (int, bool) { n, ok := tc.parent[pid]; return n, ok },
				Comm:   func(pid int) (string, bool) { c, ok := tc.comm[pid]; return c, ok },
			}
			if got := p.OwnerPID(ppid); got != tc.want {
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
		if calls > 2*ownerDepth {
			t.Fatalf("the walk made %d probes, want at most %d", calls, 2*ownerDepth)
		}
	}
	p := Probes{
		Comm:   func(int) (string, bool) { probe(); return "bash", true },
		Parent: func(pid int) (int, bool) { probe(); return pid, true },
	}
	if got := p.OwnerPID(4241); got != 4241 {
		t.Errorf("OwnerPID = %d, want the fallback parent", got)
	}
}

// commName is the arm's `tr -d '[:space:]'` and `[ -n "$c" ]`: nothing left after
// the whitespace is "no name", and the walk breaks there rather than recording a
// pid ps could not read.
func TestCommName(t *testing.T) {
	cases := map[string]string{
		"bash\n":         "bash",
		"  node  \n":     "node",
		"/usr/lib/exe\n": "/usr/lib/exe",
		"\n":             "",
		"   ":            "",
		"":               "",
	}
	for in, want := range cases {
		name, ok := commName(in)
		if name != want || ok != (want != "") {
			t.Errorf("commName(%q) = %q, %v; want %q, %v", in, name, ok, want, want != "")
		}
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

// psComm execs `ps`, so this needs a ps binary on PATH: the Nix check phase
// provides one through nativeCheckInputs.
func TestPsCommNamesOwnProcess(t *testing.T) {
	name, ok := psComm(os.Getpid())
	if !ok || name == "" {
		t.Errorf("psComm(own pid) = %q, %v; want a name and true", name, ok)
	}
}
