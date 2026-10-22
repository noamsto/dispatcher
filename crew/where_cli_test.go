package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// crewWin is one `list-windows` line: window id, branch, crew dir, crew id,
// codename, session, window index, window name.
func crewWin(dir, id, branch, crew, name, index, winName string) string {
	return strings.Join([]string{id, branch, dir, crew, name, "sess", index, winName}, "\t")
}

// A bad argument is the arm's own refusal, and it is reached without asking tmux
// anything: an accepted target reads its two lists, a refused one reads neither.
func TestWhereRefusesBeforeReadingTmux(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	dir := filepath.Join(repo, ".git", "crew")
	calls, reset := stubTmuxLogged(t,
		crewWin(dir, "@624", "feat/1-a", "c1", "nova", "3", "win-a")+"\n",
		"@624\t%204\tlead\t1\n")
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	// The stub is the tmux crew reads: a target that resolves asks it twice.
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"where", "nova"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("where nova: code %d stderr %q", code, stderr.String())
	}
	if got := strings.Count(calls(), "\n"); got != 2 {
		t.Fatalf("a resolved target made %d tmux calls, want 2: %s", got, calls())
	}
	reset()

	for _, tc := range []struct {
		argv   []string
		stderr string
	}{
		{[]string{"where"}, "crew: where: " + whereUsage + "\n"},
		{[]string{"where", "--bogus", "nova"}, "crew: where: unknown flag '--bogus' (" + whereUsage + ")\n"},
		{[]string{"where", "--crew"}, "crew: where: --crew needs an id (" + whereUsage + ")\n"},
		{[]string{"where", "--crew", "--bogus"}, "crew: where: " + whereUsage + "\n"},
		{[]string{"where", "nova", "iris"}, "crew: where: one target only (" + whereUsage + ")\n"},
	} {
		stdout.Reset()
		stderr.Reset()
		code := run(context.Background(), tc.argv, &stdout, &stderr, e)
		if code != exitFailure || stdout.Len() != 0 || stderr.String() != tc.stderr {
			t.Errorf("%v: code %d stdout %q stderr %q, want 1 and %q", tc.argv, code, stdout.String(), stderr.String(), tc.stderr)
		}
		if got := calls(); got != "" {
			t.Errorf("%v read tmux before refusing: %s", tc.argv, got)
		}
	}
}

// `--crew` is the crew anchor: it overrides `CREW_ID`, and a window of the crew
// left behind stops answering.
func TestWhereCrewFlagReplacesTheAmbientCrew(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	dir := filepath.Join(repo, ".git", "crew")
	stubTmuxLogged(t,
		crewWin(dir, "@624", "feat/1-a", "c1", "nova", "3", "win-a")+"\n"+
			crewWin(dir, "@625", "feat/2-b", "c2", "iris", "4", "win-b")+"\n",
		"@624\t%204\tlead\t1\n@625\t%304\tlead\t1\n")
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	for _, tc := range []struct {
		argv           []string
		code           int
		stdout, stderr string
	}{
		{argv: []string{"where", "--crew", "c2", "iris"},
			stdout: `iris — sess:4.1 "win-b" (lead pane)   jump: ! tmux switch-client -t %304` + "\n"},
		{argv: []string{"where", "--crew", "c2", "nova"}, code: exitFailure, stderr: "crew: where: no worker matches 'nova'\n"},
		{argv: []string{"where", "nova"},
			stdout: `nova — sess:3.1 "win-a" (lead pane)   jump: ! tmux switch-client -t %204` + "\n"},
		{argv: []string{"where", "iris"}, code: exitFailure, stderr: "crew: where: no worker matches 'iris'\n"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.argv, &stdout, &stderr, e)
		if code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
			t.Errorf("%v: code %d stdout %q stderr %q, want %d %q %q",
				tc.argv, code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
		}
	}
}
