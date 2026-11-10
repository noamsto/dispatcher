package adopt

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// reattachWorld is a dispatcher engine that outlived its registration:
// `dispatch` runs in bash 500 under claude 400, in pane %7 whose own process is
// fish 300.
func reattachWorld() *stub {
	return &stub{
		now:    time.Unix(1700000000, 0),
		env:    map[string]string{"TMUX_PANE": "%7"},
		parent: map[int]int{500: 400, 400: 300},
		comm:   map[int]string{500: "bash", 400: "claude", 300: "fish"},
		self:   "300\t",
	}
}

func reattachReq(id string) Request { return Request{ID: id, PID: "500", Reattach: true} }

func fileText(t *testing.T, path string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// Every gate that fails is silent and writes nothing: `dispatch` runs this on
// each call, from workers and role panes as well as the dispatcher.
func TestReattachSkips(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		unknown   bool
		recorded  string
		alive     []int
		pid       *string
		env       map[string]string
		comm      map[int]string
		parent    map[int]int
		self      *string
		tmuxFails bool
	}{
		{name: "worker pane", env: map[string]string{"CREW_WORKER_ID": "w1"}},
		{name: "role pane", env: map[string]string{"CREW_ROLE_ID": "r1"}},
		{name: "no TMUX_PANE", env: map[string]string{"TMUX_PANE": ""}},
		{name: "window id for a pane", env: map[string]string{"TMUX_PANE": "@5"}},
		{name: "non-digit pane", env: map[string]string{"TMUX_PANE": "%x"}},
		{name: "bare percent", env: map[string]string{"TMUX_PANE": "%"}},
		{name: "zero start pid", pid: ptr("0")},
		{name: "non-digit start pid", pid: ptr("abc")},
		{name: "first non-shell is no engine", comm: map[int]string{400: "houston"}},
		{name: "shells all the way to init", comm: map[int]string{400: "zsh"}, parent: map[int]int{300: 1}},
		{name: "tmux fails", tmuxFails: true},
		{name: "tmux answers nothing", self: ptr("")},
		{name: "worker window", self: ptr("300\tfeat/1-x")},
		{name: "pane pid not an ancestor", self: ptr("777\t")},
		{name: "unknown crew", id: "c-nope", unknown: true},
		{name: "another live owner", recorded: "999", alive: []int{999}},
		{name: "already registered to the engine", recorded: "400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			id := tc.id
			if id == "" {
				id = "c-re"
			}
			recorded := tc.recorded
			if recorded == "" {
				recorded = "4321"
			}
			if !tc.unknown {
				registerCrew(t, p, id, recorded)
			}
			s := reattachWorld()
			for k, v := range tc.env {
				s.env[k] = v
			}
			for k, v := range tc.comm {
				s.comm[k] = v
			}
			for k, v := range tc.parent {
				s.parent[k] = v
			}
			s.alive = map[int]bool{}
			for _, pid := range tc.alive {
				s.alive[pid] = true
			}
			if tc.self != nil {
				s.self = *tc.self
			}
			req := reattachReq(id)
			if tc.pid != nil {
				req.PID = *tc.pid
			}
			o := s.options()
			if tc.tmuxFails {
				o.Tmux = &tmuxtest.Fake{}
			}

			var out, errOut strings.Builder
			code := Run(t.Context(), req, p, &out, &errOut, o)
			if code != 0 || out.String() != "" || errOut.String() != "" {
				t.Errorf("code = %d, stdout = %q, stderr = %q; want a silent 0", code, out.String(), errOut.String())
			}
			pid, ok := fileText(t, p.CrewDir(id)+"/pid")
			if tc.unknown {
				if _, err := os.Stat(p.CrewDir(id)); !os.IsNotExist(err) {
					t.Errorf("an unknown crew got a directory: %v", err)
				}
			} else if !ok || pid != recorded+"\n" {
				t.Errorf("pid file = %q, want untouched %q", pid, recorded)
			}
			if pane, ok := fileText(t, panePath(p, id)); ok {
				t.Errorf("pane file written: %q", pane)
			}
			if line, ok := fileText(t, p.Dir+"/pidfile.log"); ok {
				t.Errorf("pidfile.log written: %q", line)
			}
			if len(s.calls) != 0 {
				t.Errorf("calls = %v, want none", s.calls)
			}
		})
	}
}

func TestReattachWrites(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, p bus.Paths)
		world   func(s *stub)
		wantOld string
	}{
		{
			name:    "crew dir without pid or pane",
			setup:   func(t *testing.T, p bus.Paths) { mkCrewDir(t, p) },
			wantOld: "-",
		},
		{
			name: "recorded dead pid and a stale pane",
			setup: func(t *testing.T, p bus.Paths) {
				registerCrew(t, p, "c-re", "4321")
				if err := os.WriteFile(panePath(p, "c-re"), []byte("%3\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantOld: "4321",
		},
		{
			name:    "known from a bus row alone",
			setup:   func(*testing.T, bus.Paths) {},
			wantOld: "-",
		},
		{
			name:    "nix-wrapped engine",
			setup:   func(t *testing.T, p bus.Paths) { mkCrewDir(t, p) },
			world:   func(s *stub) { s.comm[400] = "/nix/store/x/bin/.claude-wrapped" },
			wantOld: "-",
		},
		{
			name:  "engine above two shells",
			setup: func(t *testing.T, p bus.Paths) { mkCrewDir(t, p) },
			world: func(s *stub) {
				s.parent[500] = 450
				s.parent[450] = 400
				s.comm[450] = "fish"
			},
			wantOld: "-",
		},
		{
			name:    "pane runs the engine itself",
			setup:   func(t *testing.T, p bus.Paths) { mkCrewDir(t, p) },
			world:   func(s *stub) { s.self = "400\t" },
			wantOld: "-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			tc.setup(t, p)
			// A claim this crew still holds: reattaching must not release it.
			writeLog(t, p, claimRow("c-re", "83", "feat/83-foo", 100))
			s := reattachWorld()
			if tc.world != nil {
				tc.world(s)
			}
			got := run(t, s, p, reattachReq("c-re"))
			if got.code != 0 || got.stdout != "" {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", got.code, got.stdout, got.stderr)
			}
			want := "crew adopt: re-registered crew c-re to dispatcher pid 400 in pane %7 — it had no live registration\n"
			if got.stderr != want {
				t.Errorf("stderr = %q, want %q", got.stderr, want)
			}
			if pid := readPid(t, p, "c-re"); pid != "400" {
				t.Errorf("pid file = %q, want the engine's 400", pid)
			}
			if pane, _ := fileText(t, panePath(p, "c-re")); pane != "%7\n" {
				t.Errorf("pane file = %q, want %%7\\n", pane)
			}
			line := pidfileLog(t, p)
			if strings.Contains(line, "\n") || !strings.Contains(line, " reattach ok crew=c-re old="+tc.wantOld+" new=400 ") {
				t.Errorf("pidfile.log = %q, want one reattach row with old=%s", line, tc.wantOld)
			}
			if len(s.calls) != 0 {
				t.Errorf("calls = %v, want no claim release", s.calls)
			}
		})
	}
}

func TestReattachNeverTakesForce(t *testing.T) {
	p := testPaths(t)
	mkCrewDir(t, p)
	req := reattachReq("c-re")
	req.Force = true
	got := run(t, reattachWorld(), p, req)
	if got.code != 1 || got.stdout != "" {
		t.Fatalf("code = %d, stdout = %q", got.code, got.stdout)
	}
	if !strings.Contains(got.stderr, "--reattach never takes --force") || !strings.Contains(got.stderr, usage) {
		t.Errorf("stderr = %q", got.stderr)
	}
	if _, ok := fileText(t, p.CrewDir("c-re")+"/pid"); ok {
		t.Error("pid file written")
	}
	if _, ok := fileText(t, p.Dir+"/pidfile.log"); ok {
		t.Error("pidfile.log written")
	}
}

// A crew id that is not a valid id never reaches the filesystem: ".." would
// land the pid file in the bus dir, and a leading dash is a flag to other tools.
func TestReattachSkipsUnsafeIDs(t *testing.T) {
	for _, id := range []string{"..", "-x"} {
		t.Run(id, func(t *testing.T) {
			p := testPaths(t)
			got := run(t, reattachWorld(), p, reattachReq(id))
			if got.code != 0 || got.stdout != "" || got.stderr != "" {
				t.Errorf("code = %d, stdout = %q, stderr = %q; want a silent 0", got.code, got.stdout, got.stderr)
			}
			for _, path := range []string{
				p.CrewDir(id) + "/pid", p.CrewDir(id) + "/pane",
				p.Dir + "/pid", p.Dir + "/pane", p.Dir + "/crews/../pid",
				p.Dir + "/pidfile.log",
			} {
				if text, ok := fileText(t, path); ok {
					t.Errorf("%s written: %q", path, text)
				}
			}
		})
	}
}

// A missing id or pid is a usage error, not a silent success: only `dispatch`
// calls this with both, and a hand-run `crew adopt --reattach c1` did nothing.
func TestReattachNeedsIDAndPID(t *testing.T) {
	for _, req := range []Request{
		{ID: "c-re", Reattach: true},
		{PID: "500", Reattach: true},
		{Reattach: true},
	} {
		p := testPaths(t)
		mkCrewDir(t, p)
		got := run(t, reattachWorld(), p, req)
		if got.code != 1 || got.stdout != "" || got.stderr != usage+"\n" {
			t.Errorf("%+v: code = %d, stdout = %q, stderr = %q; want 1 and the usage", req, got.code, got.stdout, got.stderr)
		}
		if _, ok := fileText(t, p.CrewDir("c-re")+"/pid"); ok {
			t.Errorf("%+v: pid file written", req)
		}
		if _, ok := fileText(t, p.Dir+"/pidfile.log"); ok {
			t.Errorf("%+v: pidfile.log written", req)
		}
	}
}

func mkCrewDir(t *testing.T, p bus.Paths) {
	t.Helper()
	if err := os.MkdirAll(p.CrewDir("c-re"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }
