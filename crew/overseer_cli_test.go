package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/overseer"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

func TestOverseerProbeRunsOnlyTheRosterLiveProbe(t *testing.T) {
	probes := func(overseerEnv string) sender.Probes {
		return sender.Probes{
			Getenv: func(k string) string {
				if k == "CREW_OVERSEER" {
					return overseerEnv
				}
				return ""
			},
			Tmux: &tmuxtest.Fake{Out: map[string]string{"list-panes": ""}},
			Proc: proctest.New(nil, map[int]int{os.Getpid(): 1}),
			Self: func() (string, error) { return "h1", nil },
		}
	}
	good := []string{"crew", "roster", "--live", "--", "c1"}
	for _, tc := range []struct {
		name   string
		env    string
		argv   []string
		called bool
	}{
		{"exact argv", "h1", good, true},
		{"other verb", "h1", []string{"crew", "status"}, false},
		{"no separator", "h1", []string{"crew", "roster", "--live", "c1"}, false},
		{"extra word", "h1", append(slices.Clone(good), "x"), false},
		{"not the overseer", "", good, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []remote.Call
			run := func(_ context.Context, c remote.Call, _, _ io.Writer) int {
				got = append(got, c)
				return 0
			}
			capture := overseerProbeFor(probes(tc.env), run)("/clone")
			var stderr bytes.Buffer
			code := capture(t.Context(), "halo", tc.argv, io.Discard, &stderr)
			if !tc.called {
				if code != 1 || len(got) != 0 || stderr.Len() == 0 {
					t.Fatalf("exit %d, %d runs, stderr %q; want a refusal that ran nothing", code, len(got), stderr.String())
				}
				return
			}
			want := remote.Call{Host: "halo", Cwd: "/clone", Argv: good}
			if code != 0 || len(got) != 1 || !slices.Equal(got[0].Argv, want.Argv) || got[0].Host != want.Host || got[0].Cwd != want.Cwd || len(got[0].Env) != 0 || got[0].SpecFile != "" {
				t.Fatalf("exit %d, calls %+v, want one %+v", code, got, want)
			}
		})
	}
}

// overseerFar is a machine whose settings name zz-origin as a partner host, a
// registered repo with crew c1, and a tmux with no panes.
func overseerFar(t *testing.T) (e env, common string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sweep := filepath.Join(dir, "sweep")
	if err := os.Mkdir(sweep, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"hosts":{"zz-origin":{"ssh":"zz-origin","repos":{}}},"overseer":{"root":"` + sweep + `"}}`
	for name, body := range map[string]string{
		"tmux":     "#!/bin/sh\nexit 0\n",
		"settings": "#!/bin/sh\nprintf '%s' '" + settings + "'\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(dir, "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	common = filepath.Join(repo, ".git")
	if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"ts":1,"crew_id":"c1","from":"worker:feat/x#s1","kind":"status","body":{"state":"working"}}` + "\n"
	if err := os.WriteFile(filepath.Join(common, "crew", "events.jsonl"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "crew", "repos"), []byte(common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DISPATCH_CONFIG_BIN", filepath.Join(bin, "settings"))
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("CREW_SWEEP_ROOTS", sweep)
	t.Setenv("DISPATCH_FLEET_BUDGET", "8")
	for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_OVERSEER", "CREW_ID", "TMUX", "TMUX_PANE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	return env{
		getwd: func() (string, error) { return dir, nil },
		proc:  proc.OS{},
		peerHosts: func() (map[string]remote.Host, error) {
			return remote.Load(context.Background(), remote.ConfigBin(), io.Discard)
		},
	}, common
}

func TestOverseerDeliverPostsFromTheOrigin(t *testing.T) {
	e, common := overseerFar(t)
	body := `{"kind":"notice","detail":"x"}`
	code, out, errs := crewRun(t, e, "overseer", "deliver", "--origin", "zz-origin", "--", common, "c1", body)
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	var ts struct {
		TS int64 `json:"ts"`
	}
	if err := json.Unmarshal([]byte(out), &ts); err != nil || ts.TS == 0 {
		t.Fatalf("stdout %q: %v", out, err)
	}
	log, err := os.ReadFile(filepath.Join(common, "crew", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if rows := strings.Split(strings.TrimSpace(string(log)), "\n"); len(rows) != 2 || !strings.Contains(rows[1], `"from":"overseer:zz-origin"`) {
		t.Fatalf("bus rows %q, want the seed and one from overseer:zz-origin", rows)
	}
}

func TestOverseerDeliverRefusesAForeignOrigin(t *testing.T) {
	e, common := overseerFar(t)
	before, err := os.ReadFile(filepath.Join(common, "crew", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs := crewRun(t, e, "overseer", "deliver", "--origin", "zz-evil", "--", common, "c1", `{"kind":"notice","detail":"x"}`)
	if code != 1 || out != `{"refused":"relay"}`+"\n" || errs == "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	after, err := os.ReadFile(filepath.Join(common, "crew", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a refused relay wrote %q", after)
	}
}

func TestOverseerStatusPrintsOneVersionedLine(t *testing.T) {
	e, _ := overseerFar(t)
	code, out, errs := crewRun(t, e, "overseer", "status", "--origin", "zz-origin")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	var ans struct {
		V int `json:"v"`
	}
	if strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &ans) != nil || ans.V != 1 {
		t.Fatalf("stdout %q, want one JSON line with v 1", out)
	}
}

// TestOverseerFarVerbsFailOnUnreadableSettings: with the settings unreadable
// the far verbs fail rather than serve the registry alone, while the settings
// hosts still admit the origin.
func TestOverseerFarVerbsFailOnUnreadableSettings(t *testing.T) {
	for _, args := range [][]string{
		{"overseer", "status", "--origin", "zz-origin"},
		{"overseer", "deliver", "--origin", "zz-origin", "--", "repo", "c1", `{"kind":"notice","detail":"x"}`},
	} {
		t.Run(args[1], func(t *testing.T) {
			e, common := overseerFar(t)
			e.peerHosts = func() (map[string]remote.Host, error) {
				return map[string]remote.Host{"zz-origin": {SSH: "zz-origin"}}, nil
			}
			bin := filepath.Join(t.TempDir(), "settings")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DISPATCH_CONFIG_BIN", bin)
			before, err := os.ReadFile(filepath.Join(common, "crew", "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			code, out, errs := crewRun(t, e, args...)
			wantOut := ""
			if args[1] == "deliver" {
				wantOut = `{"refused":"root"}` + "\n"
			}
			if want := "crew: overseer " + args[1] + ": overseer root: " + bin + ": exit status 3\n"; code != exitFailure || out != wantOut || errs != want {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 1, %q, %q", code, out, errs, wantOut, want)
			}
			if after, _ := os.ReadFile(filepath.Join(common, "crew", "events.jsonl")); string(after) != string(before) {
				t.Errorf("a failed far verb wrote %q", after)
			}
		})
	}
}

func TestOverseerFarGrammarRefusals(t *testing.T) {
	e := env{getwd: func() (string, error) { return t.TempDir(), nil }}
	for _, tc := range []struct {
		args  []string
		usage string
	}{
		{[]string{"overseer", "status"}, overseer.StatusUsage},
		{[]string{"overseer", "status", "--origin", "a", "--since", "x"}, overseer.StatusUsage},
		{[]string{"overseer", "status", "--origin", "a", "--since", "-1"}, overseer.StatusUsage},
		{[]string{"overseer", "status", "--origin", "a", "--since", "99999999999999999999"}, overseer.StatusUsage},
		{[]string{"overseer", "status", "--origin", "a", "--bogus"}, overseer.StatusUsage},
		{[]string{"overseer", "deliver"}, overseer.DeliverUsage},
		{[]string{"overseer", "deliver", "--", "r", "c1", "{}"}, overseer.DeliverUsage},
		{[]string{"overseer", "deliver", "--origin", "a", "--", "r", "c1"}, overseer.DeliverUsage},
		{[]string{"overseer", "msg", "--host", "", "r", "c1", "{}"}, overseer.MsgUsage},
	} {
		code, out, errs := crewRun(t, e, tc.args...)
		if code != exitFailure || out != "" || errs != tc.usage+"\n" {
			t.Errorf("crew %v: %d %q %q", tc.args, code, out, errs)
		}
	}
}
