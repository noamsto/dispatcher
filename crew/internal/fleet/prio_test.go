package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

func (f *queueFix) prio(repo, n string) (code int, stdout, stderr string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code = Prio(f.t.Context(), PrioRequest{Repo: repo, N: n}, &out, &errb, f.opts)
	return code, out.String(), errb.String()
}

func (f *queueFix) priorityPaths() (file, log string) {
	dir := filepath.Join(f.env["XDG_DATA_HOME"], "crew")
	return filepath.Join(dir, "fleet-priority.json"), filepath.Join(dir, "fleet-priority.log")
}

func (f *queueFix) noPriorityFile() {
	f.t.Helper()
	file, log := f.priorityPaths()
	for _, p := range []string{file, log} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			f.t.Errorf("%s exists (%v)", p, err)
		}
	}
}

// A worker following an injected instruction cannot put its repo first.
func TestPrioRefusedUnderTheTripwire(t *testing.T) {
	for name, setup := range map[string]func(*queueFix){
		"CREW_WORKER_ID": func(f *queueFix) { f.env["CREW_WORKER_ID"] = "worker:feat/x#s1" },
		"CREW_ROLE_ID":   func(f *queueFix) { f.env["CREW_ROLE_ID"] = "role:feat/x:reviewer" },
		"a crew-stamped pane": func(f *queueFix) {
			f.procs.Procs[os.Getpid()] = proctest.Proc{Parent: 200}
			f.procs.Procs[200] = proctest.Proc{Parent: 1}
			f.stamps = strings.Join([]string{"200", "feat/x", "", "c1", "@1", "%1"}, "\x1f") + "\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newQueueFix(t, "alpha")
			setup(f)
			code, out, errb := f.prio("acme/alpha", "5")
			if code != 1 || out != "" || !strings.HasPrefix(errb, "crew: fleet prio: refused — ") || strings.Count(errb, "\n") != 1 {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errb)
			}
			f.noPriorityFile()
		})
	}
}

func TestPrioWritesAndAudits(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	defer func(tty func() string) { stdinTTY = tty }(stdinTTY)
	stdinTTY = func() string { return "/dev/pts/3" }
	f.env["TMUX_PANE"] = "%7"
	alpha := f.commons["alpha"]
	file, logPath := f.priorityPaths()
	read := func() (map[string]int64, int64) {
		t.Helper()
		var doc struct {
			Repos     map[string]int64 `json:"repos"`
			UpdatedTS int64            `json:"updated_ts"`
		}
		data, err := os.ReadFile(file)
		if err != nil || json.Unmarshal(data, &doc) != nil {
			t.Fatalf("priority file %q: %v", data, err)
		}
		return doc.Repos, doc.UpdatedTS
	}
	audit := func() []string {
		t.Helper()
		data, _ := os.ReadFile(logPath)
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}

	if code, out, errb := f.prio("acme/alpha", "5"); code != 0 || out != "acme/alpha: priority 0 → 5\n" || errb != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	t1 := f.now.UnixMilli()
	if repos, ts := read(); fmt.Sprint(repos) != fmt.Sprint(map[string]int64{alpha: 5}) || ts != t1 {
		t.Errorf("file %v updated %d, want alpha 5 at %d", repos, ts, t1)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v (%v), want 0644", info.Mode(), err)
	}
	if got, want := audit(), []string{fmt.Sprintf("%d\t%s\t0\t5\t%%7", t1, alpha)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit %q, want %q", got, want)
	}

	f.advance(10)
	before, _ := os.ReadFile(file)
	if code, out, _ := f.prio(alpha, "5"); code != 0 || out != "acme/alpha: priority 5 (unchanged)\n" {
		t.Errorf("unchanged: exit %d %q", code, out)
	}
	if after, _ := os.ReadFile(file); !bytes.Equal(before, after) || len(audit()) != 1 {
		t.Errorf("an unchanged value rewrote the file or audited: %s, %q", after, audit())
	}

	delete(f.env, "TMUX_PANE")
	if code, out, _ := f.prio(filepath.Dir(alpha), "-3"); code != 0 || out != "acme/alpha: priority 5 → -3\n" {
		t.Errorf("by checkout: exit %d %q", code, out)
	}
	t2 := f.now.UnixMilli()
	if repos, ts := read(); repos[alpha] != -3 || ts != t2 {
		t.Errorf("file %v updated %d", repos, ts)
	}
	stdinTTY = func() string { return "pipe:[42]" }
	if code, out, _ := f.prio("acme/alpha", "0"); code != 0 || out != "acme/alpha: priority -3 → 0\n" {
		t.Errorf("to zero: exit %d %q", code, out)
	}
	if repos, _ := read(); len(repos) != 0 {
		t.Errorf("a zero priority stays in the file: %v", repos)
	}
	want := []string{
		fmt.Sprintf("%d\t%s\t0\t5\t%%7", t1, alpha),
		fmt.Sprintf("%d\t%s\t5\t-3\t/dev/pts/3", t2, alpha),
		fmt.Sprintf("%d\t%s\t-3\t0\t-", t2, alpha),
	}
	if got := audit(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit\n%q\nwant\n%q", got, want)
	}
}

func TestPrioReordersTheQueue(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.add("alpha", "c1", "a", 1, 600)
	f.add("beta", "c2", "b", 1, 10)
	if got := strings.Join(f.doc().ids(), ","); got != "a,b" {
		t.Fatalf("order %s", got)
	}
	if code, _, errb := f.prio("acme/beta", "1"); code != 0 {
		t.Fatalf("prio: %s", errb)
	}
	d := f.doc()
	if strings.Join(d.ids(), ",") != "b,a" || d.row(t, "b").RepoPrio != 1 || d.dueID() != "b" {
		t.Errorf("after prio: order %v, due %q", d.ids(), d.dueID())
	}
	file, _ := f.priorityPaths()
	f.write(file, "{corrupt")
	if got := strings.Join(f.doc().ids(), ","); got != "a,b" {
		t.Errorf("a corrupt file reads as all 0: order %s", got)
	}
}

func TestPrioRefusals(t *testing.T) {
	f := newQueueFix(t, "alpha", "solo")
	twin := f.repo("twin", true)
	f.origins[twin] = "https://github.com/acme/alpha.git"
	walk := f.repo("walk", false)
	for _, tc := range []struct{ repo, n, want string }{
		{"acme/alpha", "1", fmt.Sprintf("crew: fleet prio: acme/alpha matches more than one registered repo: %s, %s\n", f.commons["alpha"], twin)},
		{"/nope", "1", "crew: fleet prio: /nope is not a registered repo\n"},
		{walk, "1", "crew: fleet prio: " + walk + " is not a registered repo\n"},
		{"acme/solo", "x", PrioUsage + "\n"},
		{"acme/solo", "1.5", PrioUsage + "\n"},
		{"acme/solo", "", PrioUsage + "\n"},
		{"acme/solo", "1000001", PrioUsage + "\n"},
		{"acme/solo", "-1000001", PrioUsage + "\n"},
	} {
		if code, out, errb := f.prio(tc.repo, tc.n); code != 1 || out != "" || errb != tc.want {
			t.Errorf("prio %q %q: exit %d, stdout %q, stderr %q; want %q", tc.repo, tc.n, code, out, errb, tc.want)
		}
	}
	f.noPriorityFile()
	if code, out, _ := f.prio("acme/solo", "-1000000"); code != 0 || out != "acme/solo: priority 0 → -1000000\n" {
		t.Errorf("the floor: exit %d %q", code, out)
	}
}
