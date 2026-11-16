package view

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func TestOrigin(t *testing.T) {
	cases := []struct {
		row  map[string]any
		want Origin
	}{
		{map[string]any{}, OriginLocal},
		{map[string]any{"host": "halo"}, OriginRemote},
		{map[string]any{"host": "mbp", "peer": true}, OriginPeer},
	}
	for _, c := range cases {
		if got := OriginOf(c.row); got != c.want {
			t.Errorf("OriginOf(%v) = %q, want %q", c.row, got, c.want)
		}
	}
}

func verbs(acts []Action) []string {
	var v []string
	for _, a := range acts {
		v = append(v, a.Key)
	}
	return v
}

func TestActionsByOrigin(t *testing.T) {
	crew := []string{"crew"}
	pr := 12
	if got := verbs(WorkerActions(OriginLocal, crew, "c1", "feat/a", "otter", nil)); !slices.Equal(got, []string{"p", "n", "r"}) {
		t.Errorf("local no PR keys = %v", got)
	}
	if got := verbs(WorkerActions(OriginRemote, crew, "c1", "feat/a", "", &pr)); !slices.Equal(got, []string{"p", "n", "r", "m"}) {
		t.Errorf("remote PR keys = %v", got)
	}
	peer := WorkerActions(OriginPeer, crew, "c1", "feat/a", "", &pr)
	if peer == nil || len(peer) != 0 {
		t.Errorf("peer actions = %#v, want empty non-nil", peer)
	}
}

func TestCommandQuoting(t *testing.T) {
	pr := 12
	acts := WorkerActions(OriginLocal, []string{"crew"}, "c1", "feat/a", "", &pr)
	byKey := map[string]Action{}
	for _, a := range acts {
		byKey[a.Key] = a
	}
	reply := byKey["r"]
	if want := []string{"crew", "reply", "--crew", "c1", "--", "worker:feat/a", "<text>"}; !slices.Equal(reply.Argv, want) {
		t.Errorf("reply argv = %v", reply.Argv)
	}
	if want := "crew reply --crew c1 -- worker:feat/a '<text>'"; reply.Command != want || !reply.NeedsInput {
		t.Errorf("reply command = %q needs_input=%v", reply.Command, reply.NeedsInput)
	}
	merge := byKey["m"]
	if want := []string{"crew", "merge", "12", "--crew", "c1"}; !slices.Equal(merge.Argv, want) {
		t.Errorf("merge argv = %v", merge.Argv)
	}
	if merge.Command != "crew merge 12 --crew c1" || merge.Label != "squash-merge" {
		t.Errorf("merge = %+v", merge)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
	if got := byKey["n"].Argv; got[len(got)-1] != "feat/a" {
		t.Errorf("nudge target = %v", got)
	}
}

func TestPRNumber(t *testing.T) {
	if n := PRNumber("https://github.com/o/r/pull/12"); n == nil || *n != 12 {
		t.Errorf("PRNumber = %v", n)
	}
	for _, u := range []string{"", "https://github.com/o/r/issues/12", "nope"} {
		if PRNumber(u) != nil {
			t.Errorf("PRNumber(%q) != nil", u)
		}
	}
}

func fullInputs(t *testing.T) Inputs {
	t.Helper()
	b, err := os.ReadFile("../ui/testdata/snapshot-full.json")
	if err != nil {
		t.Fatal(err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	return Inputs{Snap: snap, NowMS: snap.Now * 1000, Crew: []string{"crew"}, Loaded: Loaded{true, true, true, true, true, true}}
}

func TestBuildRoster(t *testing.T) {
	in := fullInputs(t)
	v := BuildRoster(in)
	if v.SchemaVersion != 1 || v.View != "roster" || len(v.Crews) != len(in.Snap.Roster.Crews) {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Crews[0].Workers) != 2 || v.Crews[0].Workers[0].Origin != OriginLocal {
		t.Errorf("c1 workers = %+v", v.Crews[0].Workers)
	}
	if len(v.Crews[0].Holds) != 1 || v.Crews[0].Holds[0].ID != "h1" {
		t.Errorf("holds = %+v", v.Crews[0].Holds)
	}
	if h := v.Crews[0].Holds[0]; h.Matured != (*h.ResetAt <= in.Snap.Now) {
		t.Errorf("matured = %v", h.Matured)
	}
	out, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, ok := m["peers"].([]any); !ok {
		t.Errorf("peers not []: %s", out)
	}
}

func TestBuildTabs(t *testing.T) {
	in := fullInputs(t)
	if v := BuildBudget(in); v.View != "budget" || v.Warnings == nil || v.Now != in.Snap.Now {
		t.Errorf("budget = %+v", v)
	}
	if v := BuildRuns(in); v.View != "runs" || v.RatingsGroups == nil {
		t.Errorf("runs = %+v", v)
	}
	if v := BuildSettings(in); v.View != "settings" || v.Rows == nil || v.Warnings == nil {
		t.Errorf("settings = %+v", v)
	}
	m := BuildMap(in)
	if m.View != "map" || len(m.Workers) != 2 || m.Workers[0].Crew != "c1" {
		t.Errorf("map = %+v", m)
	}
}

func TestBuildMapSkipsCrewsWithWorkersError(t *testing.T) {
	msg := "boom"
	w := []map[string]any{{"branch": "feat/x", "name": "x", "state": "idle"}}
	var in Inputs
	in.Snap.Roster.Crews = []data.RosterCrew{
		{ID: "ok", Workers: w},
		{ID: "bad", Workers: w, WorkersError: &msg},
	}
	m := BuildMap(in)
	if len(m.Workers) != 1 || m.Workers[0].Crew != "ok" {
		t.Errorf("workers = %+v, want only crew ok", m.Workers)
	}
}

func TestBuildRunsLoadedPerSource(t *testing.T) {
	v := BuildRuns(Inputs{Loaded: Loaded{Retro: true}})
	if v.Loaded || !v.RetroLoaded || v.RatingsLoaded {
		t.Errorf("runs loaded = %v/%v/%v", v.Loaded, v.RetroLoaded, v.RatingsLoaded)
	}
	if v := BuildRuns(Inputs{Loaded: Loaded{Retro: true, Ratings: true}}); !v.Loaded {
		t.Errorf("Loaded = false with both sources loaded")
	}
}

func TestBuildDispatch(t *testing.T) {
	in := fullInputs(t)
	for _, n := range []string{"roster", "budget", "runs", "settings", "map"} {
		if _, err := Build(n, in); err != nil {
			t.Errorf("Build(%s): %v", n, err)
		}
	}
	if _, err := Build("nope", in); err == nil {
		t.Error("unknown view should error")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain/word-1":   "plain/word-1",
		"it's":           `'it'\''s'`,
		`a\b`:            `'a'\\'b'`,
		`x\'; echo I; #`: `'x'\\''\''; echo I; #'`,
		"a b":            "'a b'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// roundTripTokens is every printable ASCII character alone, after "a", and
// before each suffix that completes a shell special (%self, ~x, $1, ~~), plus
// the hostile strings earlier quoting bugs let through.
func roundTripTokens() []string {
	toks := []string{
		`x\'; echo INJECTED; #`, `a\\b`, `\`, `it's`, `$HOME $(id) ` + "`id`", `a;b&&c|d`, "a b  c", "line1\nline2", `"dq"`, `\n`, `trailing\`,
	}
	for c := byte(' '); c <= '~'; c++ {
		for _, pre := range []string{"", "a"} {
			for _, suf := range []string{"", "self", "x", "1", "~"} {
				toks = append(toks, pre+string(c)+suf)
			}
		}
	}
	return toks
}

func TestShellQuoteRoundTripsThroughShells(t *testing.T) {
	toks := roundTripTokens()
	quoted := make([]string, len(toks))
	var want strings.Builder
	for i, s := range toks {
		quoted[i] = shellQuote(s)
		want.WriteString("[" + s + "]\n")
	}
	shells := []struct{ name, flags string }{{"sh", "-c"}, {"fish", "--no-config -c"}}
	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) {
			path, err := exec.LookPath(sh.name)
			if err != nil {
				if testing.Short() {
					t.Skip(sh.name + " not installed")
				}
				t.Fatalf("%s not installed; the round trip needs it (go test -short skips)", sh.name)
			}
			run := func(args ...string) string {
				argv := append(strings.Fields(sh.flags), `printf '[%s]\n' `+strings.Join(args, " "))
				out, err := exec.Command(path, argv...).Output()
				if err != nil {
					t.Fatalf("%s: %v", sh.name, err)
				}
				return string(out)
			}
			if run(quoted...) == want.String() {
				return
			}
			for i, s := range toks {
				if got := run(quoted[i]); got != "["+s+"]\n" {
					t.Errorf("%s read %q (quoted %s) back as %q", sh.name, s, quoted[i], got)
				}
			}
		})
	}
}

func TestMergeActionCarriesPR(t *testing.T) {
	pr := 12
	for _, a := range WorkerActions(OriginLocal, []string{"crew"}, "c1", "feat/a", "", &pr) {
		if (a.Key == "m") != (a.PR != nil) {
			t.Errorf("action %s: PR = %v", a.Key, a.PR)
		}
		if a.Key == "m" && *a.PR != 12 {
			t.Errorf("merge PR = %d", *a.PR)
		}
	}
}

func TestActionDirCommand(t *testing.T) {
	dir := "/r/a b"
	a := newAction("p", "peek", "peek", false, PeekArgv([]string{"crew"}, "c1", "feat/a"))
	got := withDir(a, &dir)
	if want := "cd '/r/a b' && crew where --crew c1 -- feat/a"; got.Command != want {
		t.Errorf("command = %q, want %q", got.Command, want)
	}
	if got.Dir == nil || *got.Dir != dir {
		t.Errorf("dir = %v", got.Dir)
	}
	if plain := withDir(a, nil); plain.Command != "crew where --crew c1 -- feat/a" || plain.Dir != nil {
		t.Errorf("nil dir changed the action: %+v", plain)
	}
}

func TestRelaunchArgv(t *testing.T) {
	want := []string{"dispatcher", "--relaunch", "c1", "--repo", "/r"}
	if got := RelaunchArgv("c1", "/r"); !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestHoldMatured(t *testing.T) {
	const now = 1000
	cases := []struct {
		name string
		hold map[string]any
		want bool
	}{
		{"window at now", map[string]any{"id": "h", "wait": map[string]any{"kind": "window", "resets_at": float64(1000)}}, true},
		{"quota without wait kind, past reset", map[string]any{"id": "h", "wait": map[string]any{"resets_at": float64(900)}}, true},
		{"future reset", map[string]any{"id": "h", "wait": map[string]any{"resets_at": float64(1100)}}, false},
		{"fleet wait", map[string]any{"id": "h", "wait": map[string]any{"kind": "fleet", "resets_at": float64(900)}}, false},
		{"fleet status quota due", map[string]any{"id": "h", "kind": "quota", "due": true}, true},
		{"fleet status quota not due", map[string]any{"id": "h", "kind": "quota", "due": false}, false},
		{"fleet status fleet due", map[string]any{"id": "h", "kind": "fleet", "due": true}, false},
	}
	for _, c := range cases {
		if got := HoldRowOf(c.hold, now).Matured; got != c.want {
			t.Errorf("%s: matured = %v, want %v", c.name, got, c.want)
		}
	}
}
