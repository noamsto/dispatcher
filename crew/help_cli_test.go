package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// These are the help screens and the engine-cmd verdict as the binary answers
// them: the grouped list's rows against the subcommand list it is built from,
// every subcommand's own --help with nothing around it, `hold`'s per-action
// screens, and the two positions where --help stops being a request.

// helpRow is one subcommand row of the grouped list: two spaces, the name,
// padding, its purpose. Group headers sit at column 0, so they never count as a
// row, and a name printed twice reads as two rows.
var helpRow = regexp.MustCompile(`^  ([a-z][a-z0-9-]+) +[^ ].*$`)

// unlistedSubs are the words crew admits and the grouped list never carries:
// `help` is the screen printer rather than a row of its own, and `git-guard`
// and `reap-autosweep` are callable but deliberately undocumented.
var unlistedSubs = []string{"help", "git-guard", "reap-autosweep"}

// listedSubs is every subcommand the grouped list has to carry, sorted: the
// `subcommands` literal the router admits, minus the three it never advertises.
func listedSubs() []string {
	got := make([]string, 0, len(subcommands))
	for _, name := range subcommands {
		if !slices.Contains(unlistedSubs, name) {
			got = append(got, name)
		}
	}
	slices.Sort(got)
	return slices.Compact(got)
}

// helpRows are the list's rows in a screen, sorted, a duplicate kept so it
// reads as two rows.
func helpRows(screen string) []string {
	rows := []string{}
	for _, line := range strings.Split(screen, "\n") {
		if m := helpRow.FindStringSubmatch(line); m != nil {
			rows = append(rows, m[1])
		}
	}
	slices.Sort(rows)
	return rows
}

// noCwdEnv is the env of a run that has no working directory at all:
// a screen can only come from the help routing, never from a repo read.
var noCwdEnv = env{getwd: func() (string, error) {
	return "", errors.New("getwd: no such file or directory")
}}

// TestGroupedListIsTheSubcommandList is the invariant behind `help: --help, -h
// and help each print every subcommand exactly once` and the rows half of
// `help: bare crew and an unknown subcommand print the list to stderr and fail`:
// one row per admitted subcommand, and the five spellings that print the list
// differ only in where it goes and what they exit with. A command that gains an
// arm and no row fails here, which is the point.
func TestGroupedListIsTheSubcommandList(t *testing.T) {
	want := listedSubs()
	if len(want) == 0 {
		t.Fatal("subcommands admits nothing")
	}
	if len(want) != len(slices.Compact(slices.Clone(want))) {
		t.Fatalf("subcommands lists a name twice: %v", want)
	}

	for _, tc := range []struct {
		args []string
		code int
		on   string // the stream the list goes to
	}{
		{[]string{"--help"}, 0, "stdout"},
		{[]string{"-h"}, 0, "stdout"},
		{[]string{"help"}, 0, "stdout"},
		{nil, exitFailure, "stderr"},
		{[]string{"bogus"}, exitFailure, "stderr"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, noCwdEnv)
		if code != tc.code {
			t.Errorf("crew %v exited %d, want %d (stderr %q)", tc.args, code, tc.code, stderr.String())
			continue
		}
		screen := stdout.String()
		if tc.on == "stderr" {
			screen = stderr.String()
			if stdout.Len() != 0 {
				t.Errorf("crew %v wrote %q to stdout, want the list on stderr only", tc.args, stdout.String())
			}
		} else if stderr.Len() != 0 {
			t.Errorf("crew %v wrote %q to stderr, want the list on stdout only", tc.args, stderr.String())
		}
		if got := helpRows(screen); !slices.Equal(got, want) {
			t.Errorf("crew %v: top-level help drifted\nrows:\n%s\nsubcommands:\n%s",
				tc.args, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if !strings.Contains(screen, "Run 'crew <command> --help'") {
			t.Errorf("crew %v printed a list with no pointer to a subcommand's own screen", tc.args)
		}
	}
}

// TestEverySubcommandAnswersHelpOutsideARepo is `help: every subcommand answers
// --help with no repo and no crew id`: from a directory no git repo owns, with
// no crew id in the environment and no tmux to read, every subcommand prints
// its own synopsis and exits 0. The help preamble runs before the repo, crew-id
// and bus reads, which is what this pins.
func TestEverySubcommandAnswersHelpOutsideARepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	for _, k := range []string{"CREW_ID", "CREW_WORKER_ID", "CREW_ROLE_ID"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	e := env{getwd: func() (string, error) { return dir, nil }, probes: noTouchProbes()}

	for _, sub := range listedSubs() {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{sub, "--help"}, &stdout, &stderr, e)
		if code != 0 {
			t.Errorf("crew %s --help exited %d: %s", sub, code, stderr.String())
			continue
		}
		if stderr.Len() != 0 {
			t.Errorf("crew %s --help wrote %q to stderr", sub, stderr.String())
		}
		first, _, _ := strings.Cut(stdout.String(), "\n")
		if first != "usage: crew "+sub && !strings.HasPrefix(first, "usage: crew "+sub+" ") {
			t.Errorf("crew %s --help printed %q", sub, first)
		}
	}
}

// TestHoldActionHelpScreens is `help: crew hold <action> --help prints that
// action's synopsis` and `help: hold due --help prints that action's screen,
// not the flag's`: after an action, --help is that action's screen, the bare
// screen is the one that has to name every action, and an unknown action is
// refused rather than routed to a screen.
func TestHoldActionHelpScreens(t *testing.T) {
	actions := []string{"add", "list", "due", "park", "release"}

	// `crew hold due --help`, the screen the bats case compared bytes of.
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"hold", "due", "--help"}, &stdout, &stderr, noCwdEnv); code != 0 {
		t.Fatalf("hold due --help exited %d (stderr %q)", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("hold due --help wrote %q to stderr", stderr.String())
	}
	if got, _, _ := strings.Cut(stdout.String(), "\n"); got != "usage: crew hold due [--crew ID] [--json]" {
		t.Errorf("hold due --help printed %q", got)
	}
	if !strings.Contains(stdout.String(), "quota window has already reset") {
		t.Errorf("hold due --help printed no word of the due screen: %q", stdout.String())
	}

	for _, action := range actions {
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), []string{"hold", action, "--help"}, &stdout, &stderr, noCwdEnv); code != 0 {
			t.Errorf("hold %s --help exited %d (stderr %q)", action, code, stderr.String())
			continue
		}
		if first, _, _ := strings.Cut(stdout.String(), "\n"); !strings.HasPrefix(first, "usage: crew hold "+action+" ") {
			t.Errorf("hold %s --help printed %q", action, first)
		}
	}

	// `crew hold --help` names every action, since it is the one that has to.
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"hold", "--help"}, &stdout, &stderr, noCwdEnv); code != 0 {
		t.Fatalf("hold --help exited %d (stderr %q)", code, stderr.String())
	}
	if first, _, _ := strings.Cut(stdout.String(), "\n"); !strings.HasPrefix(first, "usage: crew hold add ") {
		t.Errorf("hold --help printed %q, want the add synopsis first", first)
	}
	for _, action := range actions {
		if !strings.Contains(stdout.String(), "crew hold "+action+" ") {
			t.Errorf("hold --help never names %s: %q", action, stdout.String())
		}
	}

	// An action with no screen refuses one line; the --help that asked for it
	// never reaches hold's arm.
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"hold", "bogus", "--help"}, &stdout, &stderr, noCwdEnv); code != exitFailure {
		t.Errorf("hold bogus --help exited %d, want %d", code, exitFailure)
	}
	if stdout.Len() != 0 || stderr.String() != "crew: no help for hold bogus\n" {
		t.Errorf("hold bogus --help: stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

// TestHelpDetailIsNoRequestAndSeparatorKeepsItLiteral is the other half of the
// routing: only the first position means help, so a `--help` that is detail
// text still reads as the parser's own rejection — and the documented escape
// posts that same word to the bus verbatim.
func TestHelpDetailIsNoRequestAndSeparatorKeepsItLiteral(t *testing.T) {
	repo, logPath := repoWithLog(t, nil, 0)
	t.Setenv("CREW_ID", "c1")
	for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "TMUX", "TMUX_PANE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"status", "w1", "working", "--help"}, &stdout, &stderr, e)
	if code != exitFailure {
		t.Fatalf("status w1 working --help exited %d, want %d", code, exitFailure)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want nothing", stdout.String())
	}
	const refusal = "crew: status: unknown arg '--help' (use -- before a detail that starts with dashes)\n"
	if stderr.String() != refusal {
		t.Errorf("stderr %q, want %q", stderr.String(), refusal)
	}
	// The status screen's own purpose line: its absence says no screen printed.
	if strings.Contains(stderr.String(), "Post or update a session's state") {
		t.Errorf("the refusal printed the status screen: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"status", "w1", "working", "--", "--help"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("status w1 working -- --help exited %d (stderr %q)", code, stderr.String())
	}
	if got := statusDetail(t, logPath); got != "--help" {
		t.Errorf("the bus row's detail is %q, want --help posted verbatim", got)
	}
}

// statusDetail is the detail of the one status row the bus holds.
func statusDetail(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	detail := ""
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var row struct {
			Kind string `json:"kind"`
			Body struct {
				Detail string `json:"detail"`
			} `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bus row %q: %v", line, err)
		}
		if row.Kind == "status" {
			detail = row.Body.Detail
		}
	}
	return detail
}
