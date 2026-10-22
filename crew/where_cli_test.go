package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubTmuxLogged puts a tmux on PATH that answers `crew where`'s two reads from
// files and appends every call's argv to a log. The log is the point: after a
// refused argument it is still empty, which is the proof that the flag loop ran
// ahead of both reads.
func stubTmuxLogged(t *testing.T, windows, panes string) (calls func() string, reset func()) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	for name, body := range map[string]string{"wins.txt": windows, "panes.txt": panes} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_TMUX_LOG"
case "$1" in
list-windows) cat "$STUB_TMUX_DIR/wins.txt" ;;
list-panes) cat "$STUB_TMUX_DIR/panes.txt" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_TMUX_DIR", bin)
	t.Setenv("STUB_TMUX_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	calls = func() string {
		data, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return string(data)
	}
	reset = func() { _ = os.Remove(log) }
	return calls, reset
}

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
