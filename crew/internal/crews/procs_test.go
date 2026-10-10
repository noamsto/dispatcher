package crews

import "testing"

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

func TestShellName(t *testing.T) {
	cases := map[string]string{
		"bash\n":              "bash",
		"  sh  ":              "sh",
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
