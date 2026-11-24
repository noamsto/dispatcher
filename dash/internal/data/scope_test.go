package data

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const scopeHeader = "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n"

const (
	displayKey = "tmux display-message -p -t %9 #{@crew_id}\t#{start_time}"
	panesKey   = "tmux list-panes -t %9 -F #{pane_id}"
	gitKey     = "git rev-parse --path-format=absolute --git-common-dir"
)

// scopeRunner serves a repo with two alive crews, c1 and c2, each with
// a worker, a peer row and no holds.
func scopeRunner(t *testing.T) *FakeRunner {
	t.Helper()
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{Stdout: []byte(scopeHeader + "c1\t0\t0\t1\t1\tyes\nc2\t0\t0\t1\t1\tyes\n")}
	for _, id := range []string{"c1", "c2"} {
		peer := `{"name":"p","crew_id":"` + id + `","branch":"feat/p-` + id + `","host":"mbp","peer":true,"ts":1}`
		r.Responses["crew roster "+id+" --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w-` + id + `","branch":"feat/` + id + `"},` + peer + `]`)}
		r.Responses["crew hold list --crew "+id+" --json"] = FakeResponse{Stdout: []byte("[]")}
		r.Responses["crew stream --crew "+id+" --status"] = FakeResponse{Stdout: []byte(`{"state":"alive"}`)}
	}
	return r
}

// paneBus makes a bus dir the fake git reports and writes one pane file per
// crew, each with the given mtime.
func paneBus(t *testing.T, r *FakeRunner, panes map[string]string, mtimes map[string]time.Time) {
	t.Helper()
	common := t.TempDir()
	r.Responses[gitKey] = FakeResponse{Stdout: []byte(common + "\n")}
	for id, pane := range panes {
		dir := filepath.Join(common, "crew", "crews", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "pane")
		if err := os.WriteFile(file, []byte(pane+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, mtimes[id], mtimes[id]); err != nil {
			t.Fatal(err)
		}
	}
}

func tmuxPane(r *FakeRunner, crewID string, start int64, listed ...string) {
	r.Responses[displayKey] = FakeResponse{Stdout: []byte(crewID + "\t" + strconv.FormatInt(start, 10) + "\n")}
	r.Responses[panesKey] = FakeResponse{Stdout: []byte(strings.Join(listed, "\n") + "\n")}
}

func scopedConfig(pane string) Config {
	cfg := baseConfig()
	cfg.Pane = pane
	return cfg
}

func crewIDs(sec RosterSection) []string {
	var ids []string
	for _, c := range sec.Crews {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestScopeFlagWinsAndEnvIsRecorded(t *testing.T) {
	for _, source := range []string{"flag", "env"} {
		t.Run(source, func(t *testing.T) {
			r := scopeRunner(t)
			cfg := scopedConfig("%9")
			cfg.CrewID, cfg.CrewIDSource = "c2", source
			sec := CollectRoster(context.Background(), r, cfg)
			if want := (Scope{Crew: "c2", Source: source, Known: true, Crews: 1}); sec.Scope != want {
				t.Errorf("Scope = %+v, want %+v", sec.Scope, want)
			}
			for _, c := range r.CallsSnapshot() {
				if c.Name == "tmux" {
					t.Errorf("explicit id still probed tmux: %v", c.Args)
				}
			}
		})
	}
}

func TestScopeWindowCrewIDWinsOverPaneFiles(t *testing.T) {
	r := scopeRunner(t)
	now := time.Unix(baseConfig().Now, 0)
	paneBus(t, r, map[string]string{"c1": "%1"}, map[string]time.Time{"c1": now})
	tmuxPane(r, "c2", 100, "%9", "%1")
	sec := CollectRoster(context.Background(), r, scopedConfig("%9"))
	if want := (Scope{Crew: "c2", Source: "window", Known: true, Crews: 1}); sec.Scope != want {
		t.Errorf("Scope = %+v, want %+v", sec.Scope, want)
	}
}

func TestScopePane(t *testing.T) {
	start := int64(1000)
	at := func(s int64) time.Time { return time.Unix(s, 0) }
	cases := []struct {
		name   string
		panes  map[string]string
		mtimes map[string]time.Time
		listed []string
		want   string
		source string
	}{
		{"newest mtime wins", map[string]string{"c1": "%1", "c2": "%1"}, map[string]time.Time{"c1": at(1100), "c2": at(1200)}, []string{"%9", "%1"}, "c2", "pane"},
		{"mtime tie takes larger id", map[string]string{"c1": "%1", "c2": "%1"}, map[string]time.Time{"c1": at(1100), "c2": at(1100)}, []string{"%1"}, "c2", "pane"},
		{"older than window start ignored", map[string]string{"c1": "%1", "c2": "%1"}, map[string]time.Time{"c1": at(1100), "c2": at(900)}, []string{"%1"}, "c1", "pane"},
		{"own pane ignored", map[string]string{"c1": "%9"}, map[string]time.Time{"c1": at(1100)}, []string{"%9"}, "", "none"},
		{"pane outside window ignored", map[string]string{"c1": "%7"}, map[string]time.Time{"c1": at(1100)}, []string{"%9", "%1"}, "", "none"},
		{"no pane files", nil, nil, []string{"%9", "%1"}, "", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scopeRunner(t)
			paneBus(t, r, tc.panes, tc.mtimes)
			tmuxPane(r, "", start, tc.listed...)
			sc := CollectRoster(context.Background(), r, scopedConfig("%9")).Scope
			if sc.Crew != tc.want || sc.Source != tc.source {
				t.Errorf("Scope = %+v, want crew %q source %q", sc, tc.want, tc.source)
			}
		})
	}
}

func TestScopeTmuxOrGitFailureFallsThrough(t *testing.T) {
	r := scopeRunner(t)
	sec := CollectRoster(context.Background(), r, scopedConfig("%9"))
	if sec.Scope.Source != "none" || len(sec.Crews) != 2 {
		t.Errorf("tmux failure: Scope = %+v crews = %v", sec.Scope, crewIDs(sec))
	}

	r = scopeRunner(t)
	tmuxPane(r, "", 1000, "%1")
	sec = CollectRoster(context.Background(), r, scopedConfig("%9"))
	if sec.Scope.Source != "none" || len(sec.Crews) != 2 {
		t.Errorf("git failure: Scope = %+v crews = %v", sec.Scope, crewIDs(sec))
	}
}

func TestScopeProbeFailureIsReported(t *testing.T) {
	r := scopeRunner(t)
	r.Responses[displayKey] = FakeResponse{Stderr: []byte("no server running\nmore\n"), Err: FakeExitError{Code: 1}}
	sc := CollectRoster(context.Background(), r, scopedConfig("%9")).Scope
	if want := "tmux display-message: no server running"; sc.Err != want {
		t.Errorf("tmux failure Err = %q, want %q", sc.Err, want)
	}

	r = scopeRunner(t)
	tmuxPane(r, "", 1000, "%1")
	sc = CollectRoster(context.Background(), r, scopedConfig("%9")).Scope
	if !strings.HasPrefix(sc.Err, "git rev-parse --git-common-dir: ") {
		t.Errorf("git failure Err = %q", sc.Err)
	}

	r = scopeRunner(t)
	paneBus(t, r, nil, nil)
	tmuxPane(r, "", 1000, "%1")
	if sc := CollectRoster(context.Background(), r, scopedConfig("%9")).Scope; sc.Err != "" {
		t.Errorf("no pane files is not an error, got Err %q", sc.Err)
	}
}

func TestScopedRosterHoldsOnlyThatCrewAndItsPeers(t *testing.T) {
	r := scopeRunner(t)
	cfg := baseConfig()
	cfg.CrewID, cfg.CrewIDSource = "c1", "flag"
	sec := CollectRoster(context.Background(), r, cfg)
	if got := crewIDs(sec); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("crews = %v, want [c1]", got)
	}
	if len(sec.Peers) != 1 || sec.Peers[0]["crew_id"] != "c1" {
		t.Errorf("Peers = %+v, want only c1's", sec.Peers)
	}
	for _, c := range r.CallsSnapshot() {
		if strings.Contains(fakeKey(c.Name, c.Args), "c2") {
			t.Errorf("scoped to c1 still called %v", c.Args)
		}
	}
}

func TestScopedCrewNotAliveNorForeignHasNoDispatcherAndNoLaneProbe(t *testing.T) {
	r := scopeRunner(t)
	r.Responses["crew crews"] = FakeResponse{Stdout: []byte(scopeHeader + "c1\t0\t0\t0\t1\tno\n")}
	cfg := baseConfig()
	cfg.CrewID, cfg.CrewIDSource = "c1", "flag"
	sec := CollectRoster(context.Background(), r, cfg)
	if len(sec.Crews) != 1 || sec.Crews[0].Dispatcher == nil || sec.Crews[0].Dispatcher.Where != "none" {
		t.Fatalf("crews = %+v, want c1 with Where none", sec.Crews)
	}
	for _, c := range r.CallsSnapshot() {
		if k := fakeKey(c.Name, c.Args); strings.Contains(k, "stream") {
			t.Errorf("unexpected lane probe %q", k)
		}
	}
}

func TestScopedCrewElsewhereUsesForeignLookup(t *testing.T) {
	r := foreignRunner(t)
	cfg := baseConfig()
	cfg.CrewID, cfg.CrewIDSource = "f1", "flag"
	sec := CollectRoster(context.Background(), r, cfg)
	if len(sec.Crews) != 1 || sec.Crews[0].ID != "f1" || sec.Crews[0].Dispatcher.Where != "elsewhere" {
		t.Fatalf("crews = %+v, want f1 elsewhere", sec.Crews)
	}
}

func TestScopeUnknownCrew(t *testing.T) {
	r := scopeRunner(t)
	r.Responses["crew roster nope --peers"] = FakeResponse{Stdout: []byte("[]")}
	r.Responses["crew hold list --crew nope --json"] = FakeResponse{Stdout: []byte("[]")}
	cfg := baseConfig()
	cfg.CrewID, cfg.CrewIDSource = "nope", "env"
	sec := CollectRoster(context.Background(), r, cfg)
	if want := (Scope{Crew: "nope", Source: "env", Known: false, Crews: 1}); sec.Scope != want {
		t.Errorf("Scope = %+v, want %+v", sec.Scope, want)
	}
	if got := crewIDs(sec); len(got) != 1 || got[0] != "nope" {
		t.Errorf("crews = %v, want [nope]", got)
	}
}

func TestUnscopedRosterIsTheUnion(t *testing.T) {
	r := scopeRunner(t)
	sec := CollectRoster(context.Background(), r, baseConfig())
	if want := (Scope{Source: "none", Crews: 2}); sec.Scope != want {
		t.Errorf("Scope = %+v, want %+v", sec.Scope, want)
	}
	if got := crewIDs(sec); len(got) != 2 || len(sec.Peers) != 2 {
		t.Errorf("crews = %v peers = %d, want both crews and both peers", got, len(sec.Peers))
	}
}

func TestScopeSurvivesCrewsFailure(t *testing.T) {
	r := scopeRunner(t)
	r.Responses["crew crews"] = FakeResponse{Stderr: []byte("boom\n"), Err: FakeExitError{Code: 1}}
	cfg := baseConfig()
	cfg.CrewID, cfg.CrewIDSource = "c1", "flag"
	if sc := CollectRoster(context.Background(), r, cfg).Scope; sc.Crew != "c1" || sc.Source != "flag" || !sc.Known {
		t.Errorf("Scope = %+v, want flag c1", sc)
	}
	if sc := CollectRoster(context.Background(), r, baseConfig()).Scope; sc.Source != "none" {
		t.Errorf("Scope = %+v, want none", sc)
	}
}

func TestDispatcherSinceIsPidFileMtime(t *testing.T) {
	r := scopeRunner(t)
	common := t.TempDir()
	r.Responses[gitKey] = FakeResponse{Stdout: []byte(common + "\n")}
	dir := filepath.Join(common, "crew", "crews", "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(dir, "pid")
	if err := os.WriteFile(pid, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pid, time.Unix(1234, 0), time.Unix(1234, 0)); err != nil {
		t.Fatal(err)
	}
	sec := CollectRoster(context.Background(), r, baseConfig())
	for _, c := range sec.Crews {
		switch c.ID {
		case "c1":
			if c.Dispatcher.Since == nil || *c.Dispatcher.Since != 1234 {
				t.Errorf("c1 Since = %v, want 1234", c.Dispatcher.Since)
			}
		case "c2":
			if c.Dispatcher.Since != nil {
				t.Errorf("c2 without pid file: Since = %d, want nil", *c.Dispatcher.Since)
			}
		}
	}
}

func TestScopePaneResolvesLayout2CrewDir(t *testing.T) {
	r := scopeRunner(t)
	paneBus(t, r, nil, nil)
	dir := filepath.Join(t.TempDir(), "state", "c2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "pane")
	if err := os.WriteFile(file, []byte("%1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, time.Unix(1100, 0), time.Unix(1100, 0)); err != nil {
		t.Fatal(err)
	}
	r.Responses["crew where --dir --crew c2"] = FakeResponse{Stdout: []byte(dir + "\n")}
	r.Responses["crew where --dir --crew c1"] = FakeResponse{Err: FakeExitError{Code: 3}}
	tmuxPane(r, "", 1000, "%9", "%1")
	cfg := scopedConfig("%9")
	cfg.Dirs = NewCrewDirs()
	for range 2 {
		sc := CollectRoster(context.Background(), r, cfg).Scope
		if sc.Crew != "c2" || sc.Source != "pane" {
			t.Fatalf("Scope = %+v, want c2 via pane", sc)
		}
	}
	c2 := 0
	for _, c := range r.CallsSnapshot() {
		if fakeKey(c.Name, c.Args) == "crew where --dir --crew c2" {
			c2++
		}
	}
	if c2 != 1 {
		t.Errorf("c2 dir looked up %d times, want 1 (cached)", c2)
	}
}
