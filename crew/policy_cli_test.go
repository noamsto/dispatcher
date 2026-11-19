package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/policy"
)

// policyRun runs argv through run() in a temp repo whose policy dir lives under
// a temp XDG_STATE_HOME, with no owner-refusing marker in the environment.
func policyRun(t *testing.T) (repo string, runPolicy func(args ...string) (string, string, int)) {
	t.Helper()
	repo, _ = repoWithLog(t, nil, 0)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"CREW_ID", "CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_OVERSEER", "DISPATCHER_LAUNCHER_PID", "DISPATCHER_HANDOFF_DIR", "TMUX_PANE"} {
		t.Setenv(k, "")
	}
	e := env{getwd: func() (string, error) { return repo, nil }}
	return repo, func(args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr, e)
		return stdout.String(), stderr.String(), code
	}
}

func TestPolicyWriteRefusals(t *testing.T) {
	_, runPolicy := policyRun(t)
	state := os.Getenv("XDG_STATE_HOME")
	for _, tt := range []struct {
		name, marker, want string
	}{
		{"worker", "CREW_WORKER_ID", "CREW_WORKER_ID is set, so this is a worker session"},
		{"no terminal", "", "stdin is not a terminal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.marker != "" {
				t.Setenv(tt.marker, "worker:feat/x#s1-1")
			}
			for _, args := range [][]string{{"policy", "set", "night", "merge when green"}, {"policy", "clear", "night"}} {
				out, errs, code := runPolicy(args...)
				if code != 1 || out != "" || !strings.Contains(errs, "crew: policy: ") || !strings.Contains(errs, tt.want) {
					t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errs)
				}
			}
			if _, err := os.Stat(filepath.Join(state, "crew")); !os.IsNotExist(err) {
				t.Errorf("a refused write left state behind: %v", err)
			}
		})
	}
}

func TestPolicyReadsAndUsage(t *testing.T) {
	repo, runPolicy := policyRun(t)
	if out, errs, code := runPolicy("policy", "show", "night"); code != 1 || out != "" || !strings.Contains(errs, "no standing policy night in this repo") {
		t.Errorf("show missing: code %d stdout %q stderr %q", code, out, errs)
	}
	if out, errs, code := runPolicy("policy", "list"); code != 0 || out != "no standing policy in this repo\n" || errs != "" {
		t.Errorf("empty list: code %d stdout %q stderr %q", code, out, errs)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"policy", "set", "night"}, policy.SetUsage},
		{[]string{"policy", "set"}, policy.SetUsage},
		{[]string{"policy", "clear"}, policy.ClearUsage},
		{[]string{"policy", "show"}, policy.ShowUsage},
		{[]string{"policy", "list", "--json", "--brief"}, policy.ListUsage},
		{[]string{"policy"}, policy.Usage},
	} {
		if out, errs, code := runPolicy(tt.args...); code != 1 || out != "" || errs != tt.want+"\n" {
			t.Errorf("%v: code %d stdout %q stderr %q, want %q", tt.args, code, out, errs, tt.want)
		}
	}

	common := filepath.Join(repo, ".git")
	_, real, err := policy.Dir(common, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	o := policy.DefaultOptions()
	rec := policy.Record{Name: "night", Text: "merge when green", Repo: real, By: "owner", Host: "h1", UID: os.Getuid(), TTY: "/dev/pts/9", At: "2026-11-19T01:02:03Z"}
	if err := policy.Write(common, rec, o); err != nil {
		t.Fatal(err)
	}
	out, errs, code := runPolicy("policy", "show", "night")
	if code != 0 || errs != "" || !strings.Contains(out, "policy night — set by owner@h1 from /dev/pts/9 at 2026-11-19T01:02:03Z, verbatim:") || !strings.Contains(out, "merge when green") {
		t.Errorf("show: code %d stdout %q stderr %q", code, out, errs)
	}
	out, _, code = runPolicy("policy", "show", "night", "--json")
	var got policy.Record
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got != rec {
		t.Errorf("show --json: code %d stdout %q", code, out)
	}
	out, errs, code = runPolicy("policy", "list", "--brief")
	if want := `'night' set by owner@h1 from /dev/pts/9 at 2026-11-19T01:02:03Z: "merge when green"` + "\n"; code != 0 || errs != "" || out != want {
		t.Errorf("list --brief: code %d stdout %q stderr %q, want %q", code, out, errs, want)
	}
}
