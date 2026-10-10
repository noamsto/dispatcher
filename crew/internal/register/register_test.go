package register

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
)

type world struct {
	id     string
	alive  map[int]bool
	parent map[int]int
	comm   map[int]string
	env    map[string]string
}

func (w *world) options() Options {
	return Options{
		CrewID: func() string { return w.id },
		Probes: crews.Probes{
			Alive:   func(pid int) bool { return w.alive[pid] },
			Elapsed: func(int) (int64, bool) { return 0, false },
			Mtime:   func(string) (int64, bool) { return 0, false },
			Parent:  func(pid int) (int, bool) { n, ok := w.parent[pid]; return n, ok },
			Comm:    func(pid int) (string, bool) { c, ok := w.comm[pid]; return c, ok },
		},
		Now:    func() time.Time { return time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC) },
		PID:    4242,
		PPID:   4241,
		Pwd:    "/repo",
		Args:   func(int) string { return "claude --resume" },
		Getenv: func(k string) string { return w.env[k] },
	}
}

func newWorld(id string) *world {
	return &world{id: id, alive: map[int]bool{}, parent: map[int]int{}, comm: map[int]string{}, env: map[string]string{}}
}

func testPaths(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	return bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
}

func seed(t *testing.T, p bus.Paths, id, pid string) {
	t.Helper()
	if err := os.MkdirAll(p.CrewDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.CrewDir(id)+"/pid", []byte(pid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(sub string, args []string, p bus.Paths, w *world) (int, string) {
	var errb bytes.Buffer
	code := Run(sub, args, p, &errb, w.options())
	return code, errb.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func logText(p bus.Paths) string {
	data, _ := os.ReadFile(p.Dir + "/pidfile.log")
	return string(data)
}

func TestUnsetCrewID(t *testing.T) {
	for _, sub := range []string{"register", "deregister"} {
		code, stderr := run(sub, nil, testPaths(t), newWorld(""))
		want := "crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one\n"
		if code != 1 || stderr != want {
			t.Errorf("%s: code=%d stderr=%q", sub, code, stderr)
		}
	}
}

func TestInvalidCrewID(t *testing.T) {
	for _, id := range []string{"..", ".", "-x", "a/b"} {
		for _, sub := range []string{"register", "deregister"} {
			p := testPaths(t)
			code, stderr := run(sub, nil, p, newWorld(id))
			want := "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n"
			if code != 1 || stderr != want {
				t.Errorf("%s %q: code=%d stderr=%q", sub, id, code, stderr)
			}
			if _, err := os.Stat(p.Dir); err == nil {
				t.Errorf("%s %q touched the bus", sub, id)
			}
		}
	}
}

func TestRegisterRefusedOnLiveOtherPid(t *testing.T) {
	p := testPaths(t)
	seed(t, p, "c1", "777")
	w := newWorld("c1")
	w.alive[777] = true
	code, stderr := run("register", []string{"888"}, p, w)
	want := "crew: crew 'c1' still has a live dispatcher (pid 777) — 'crew new' starts your own crew; if that pid is a stale reuse, recover with 'crew adopt --force c1'\n"
	if code != 1 || stderr != want {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if got := readFile(t, p.CrewDir("c1")+"/pid"); got != "777\n" {
		t.Errorf("pid file = %q", got)
	}
	wantLine := "2026-10-10T01:02:03Z register refused crew=c1 old=777 new=888 pid=4242 ppid=4241 cwd=/repo by=claude --resume\n"
	if got := logText(p); got != wantLine {
		t.Errorf("log = %q", got)
	}
}

func TestRegisterOK(t *testing.T) {
	cases := []struct {
		name  string
		epid  string // "" = no crew dir
		alive bool
		pane  string
	}{
		{"absent", "", false, ""},
		{"dead", "777", false, "%3"},
		{"same pid live", "888", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			if tc.epid != "" {
				seed(t, p, "c1", tc.epid)
			}
			w := newWorld("c1")
			w.alive[888] = tc.alive
			if tc.epid == "777" {
				w.alive[777] = false
			}
			if tc.pane != "" {
				w.env["TMUX_PANE"] = tc.pane
			}
			code, stderr := run("register", []string{"888"}, p, w)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			if got := readFile(t, p.CrewDir("c1")+"/pid"); got != "888\n" {
				t.Errorf("pid file = %q", got)
			}
			panePath := p.CrewDir("c1") + "/pane"
			if tc.pane == "" {
				if _, err := os.Stat(panePath); err == nil {
					t.Error("pane file written without TMUX_PANE")
				}
			} else if got := readFile(t, panePath); got != tc.pane+"\n" {
				t.Errorf("pane file = %q", got)
			}
			old := tc.epid
			if old == "" {
				old = "-"
			}
			want := "2026-10-10T01:02:03Z register ok crew=c1 old=" + old + " new=888 pid=4242 ppid=4241 cwd=/repo by=claude --resume\n"
			if got := logText(p); got != want {
				t.Errorf("log = %q, want %q", got, want)
			}
		})
	}
}

func TestRegisterDefaultsToOwnerPID(t *testing.T) {
	p := testPaths(t)
	w := newWorld("c1")
	w.comm[4241] = "bash"
	w.parent[4241] = 4000
	w.comm[4000] = "claude"
	code, _ := run("register", nil, p, w)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if got := readFile(t, p.CrewDir("c1")+"/pid"); got != "4000\n" {
		t.Errorf("pid file = %q", got)
	}
}

func TestRegisterWritesPidVerbatim(t *testing.T) {
	p := testPaths(t)
	code, _ := run("register", []string{"abc"}, p, newWorld("c1"))
	if code != 0 || readFile(t, p.CrewDir("c1")+"/pid") != "abc\n" {
		t.Errorf("code=%d", code)
	}
}

func TestDeregisterRefused(t *testing.T) {
	p := testPaths(t)
	seed(t, p, "c1", "777")
	w := newWorld("c1")
	w.alive[777] = true
	w.comm[4241] = "claude"
	code, stderr := run("deregister", nil, p, w)
	want := "crew: crew 'c1' still has a live dispatcher (pid 777) that is not this caller's parent or owner — left in place\n"
	if code != 0 || stderr != want {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(p.CrewDir("c1")); err != nil {
		t.Error("crew dir removed")
	}
	wantLine := "2026-10-10T01:02:03Z deregister refused crew=c1 old=777 new=- pid=4242 ppid=4241 cwd=/repo by=claude --resume\n"
	if got := logText(p); got != wantLine {
		t.Errorf("log = %q", got)
	}
}

func TestDeregisterRemoved(t *testing.T) {
	cases := []struct {
		name string
		epid string
		w    func(*world)
	}{
		{"parent", "4241", func(w *world) { w.alive[4241] = true }},
		{"owner", "4000", func(w *world) {
			w.alive[4000] = true
			w.comm[4241] = "bash"
			w.parent[4241] = 4000
			w.comm[4000] = "claude"
		}},
		{"dead", "777", func(w *world) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			seed(t, p, "c1", tc.epid)
			w := newWorld("c1")
			tc.w(w)
			code, stderr := run("deregister", nil, p, w)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			if _, err := os.Stat(p.CrewDir("c1")); err == nil {
				t.Error("crew dir left")
			}
			want := "2026-10-10T01:02:03Z deregister removed crew=c1 old=" + tc.epid + " new=- pid=4242 ppid=4241 cwd=/repo by=claude --resume\n"
			if got := logText(p); got != want {
				t.Errorf("log = %q", got)
			}
		})
	}
}

func TestDeregisterAbsentDir(t *testing.T) {
	p := testPaths(t)
	code, stderr := run("deregister", nil, p, newWorld("c1"))
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.TrimSpace(logText(p)) != "" {
		t.Errorf("log = %q", logText(p))
	}
}

func TestDeregisterRemoveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	p := testPaths(t)
	seed(t, p, "c1", "777")
	locked := p.CrewDir("c1") + "/locked"
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locked+"/f", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	code, stderr := run("deregister", nil, p, newWorld("c1"))
	if code != 1 || !strings.HasPrefix(stderr, "crew: deregister: ") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}
