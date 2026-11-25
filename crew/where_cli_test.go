package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tmuxServerIdentity is the tmux server identity (`pid:start_time`) the fake
// tmux server answers to the arm's display-message read.
const tmuxServerIdentity = "12345:1700000000"

// crewWin is one `list-windows` line: window id, branch, crew dir, crew id,
// codename, session, window index, window name, bridge branch and bridge crew id.
func crewWin(dir, id, branch, crew, name, index, winName string) string {
	return strings.Join([]string{id, branch, dir, crew, name, "sess", index, winName, "", ""}, "\t")
}

// A bad argument is the arm's own refusal, and it is reached without asking tmux
// anything.
func TestWhereRefusesBeforeReadingTmux(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	dir := filepath.Join(repo, ".git", "crew")
	calls, reset := stubCrewTmuxLogged(t,
		crewWin(dir, "@624", "feat/1-a", "c1", "nova", "3", "win-a")+"\n",
		"@624\t%204\tlead\t1\n")
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	// The stub is the tmux crew reads: a target that resolves lists the windows and
	// the panes, then reads each stamp of the windows one value per call.
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"where", "nova"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("where nova: code %d stderr %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(calls(), "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "list-windows -a -F ") || !strings.HasPrefix(lines[1], "list-panes -a -F ") {
		t.Fatalf("a resolved target's tmux calls = %q, want the two lists first", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "list-") && !strings.HasPrefix(l, "show-options -") && !strings.HasPrefix(l, "display-message -p") {
			t.Errorf("where made a tmux call that is not a read: %q", l)
		}
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
	stubCrewTmuxLogged(t,
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

// stubCrewTmuxLogged is stubTmuxLogged for the commands that read a window's stamps
// one value per call: the stub answers each list and each per-target read from the
// rows, as tmux would.
func stubCrewTmuxLogged(t *testing.T, windows, panes string) (calls func() string, reset func()) {
	t.Helper()
	calls, reset = stubTmuxLogged(t, windows, panes)
	t.Setenv("TMUX_WINDOWS", strings.TrimSuffix(windows, "\n"))
	t.Setenv("TMUX_PANES", strings.TrimSuffix(panes, "\n"))
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$STUB_TMUX_LOG\"\n" + stampReadsHead +
		"case \"$1 $2 $4\" in\n" + stampReadsCases + "esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("STUB_TMUX_DIR"), "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return calls, reset
}

// stampReadsHead and stampReadsCases make a tmux stub answer what crew nudge and
// crew where read from a window's stamps: the two id-only lists and each stamp one
// value per call, from $TMUX_WINDOWS (crewWin's rows) and $TMUX_PANES (rows of
// window id, pane id, role, then the pane index or its foreground command).
const (
	stampReadsHead = `w=$(printf '#{window_id}\t#{window_index}\t#{session_id}')
p=$(printf '#{window_id}\t#{pane_id}\t#{pane_index}')
col() {
  case "$1" in
  @crew_branch) echo 2 ;; @crew_dir) echo 3 ;; @crew_id) echo 4 ;; @crew_name) echo 5 ;;
  @bridge_crew_branch) echo 9 ;; @bridge_crew_id) echo 10 ;; '#{session_name}') echo 6 ;;
  '#{window_name}') echo 8 ;; @crew_role) echo 3 ;; '#{pane_current_command}') echo 4 ;;
  esac
}
pick() { printf '%s\n' "$1" | awk -F'\t' -v k="$2" -v t="$3" -v c="$4" '$k == t { print $c; exit }'; }
`
	stampReadsCases = `"list-windows -a $w") printf '%s\n' "$TMUX_WINDOWS" | awk -F'\t' 'NF { print $1 "\t" ($7 ~ /^[0-9]+$/ ? $7 : 1) "\t$0" }' ;;
"list-panes -a $p") printf '%s\n' "$TMUX_PANES" | awk -F'\t' 'NF { print $1 "\t" $2 "\t" ($4 ~ /^[0-9]+$/ ? $4 : 1) }' ;;
"show-options -wqv "*) pick "$TMUX_WINDOWS" 1 "$4" "$(col "$5")" ;;
"show-options -pqv "*) pick "$TMUX_PANES" 2 "$4" "$(col "$5")" ;;
"display-message -p "*"@"*) pick "$TMUX_WINDOWS" 1 "${4#*:}" "$(col "$5")" ;;
"display-message -p %"*) pick "$TMUX_PANES" 2 "$4" "$(col "$5")" ;;
`
)
