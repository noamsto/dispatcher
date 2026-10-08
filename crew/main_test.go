package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/roster"
)

func TestParseSessions(t *testing.T) {
	const (
		needsValue = "crew: --crew needs a value"
	)
	tests := []struct {
		name                string
		args                []string
		branch, crew, wants string
	}{
		{"branch only", []string{"b"}, "b", "", ""},
		{"crew filter", []string{"b", "--crew", "c1"}, "b", "c1", ""},
		{"later crew wins", []string{"b", "--crew", "c1", "--crew", "c2"}, "b", "c2", ""},
		{"no args", nil, "", "", sessionsUsage},
		{"empty branch", []string{""}, "", "", sessionsUsage},
		{"crew taken as branch", []string{"--crew", "x"}, "", "", sessionsUsage},
		{"missing value", []string{"b", "--crew"}, "", "", needsValue},
		{"empty value", []string{"b", "--crew", ""}, "", "", needsValue},
		{"second positional", []string{"b", "x"}, "", "", sessionsUsage},
		{"empty branch with crew", []string{"", "--crew", "c1"}, "", "", sessionsUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, crew, msg := parseSessions(tt.args)
			if branch != tt.branch || crew != tt.crew || msg != tt.wants {
				t.Errorf("parseSessions(%q) = %q, %q, %q; want %q, %q, %q", tt.args, branch, crew, msg, tt.branch, tt.crew, tt.wants)
			}
		})
	}
}

const exitedRow = `{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"exited"}}` + "\n"

// repoWithLog makes a git repo and returns it with the bus log's path. A nil
// log leaves the bus absent.
func repoWithLog(t *testing.T, log *string, mode os.FileMode) (repo, logPath string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "CREW_ID"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(repo))
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v\n%s", err, out)
	}
	logPath = filepath.Join(repo, ".git", "crew", "events.jsonl")
	if log == nil {
		return repo, logPath
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(*log), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0o600) })
	return repo, logPath
}

func TestRunMapsFoldErrorsToExitCodes(t *testing.T) {
	str := func(s string) *string { return &s }
	gitFailure := &roster.ExitError{Code: 128, Stderr: "fatal: not a git repository\n"}
	tests := []struct {
		name       string
		args       []string
		log        *string
		mode       os.FileMode
		worktrees  error
		colour     bool
		jqColors   string
		wantCode   int
		wantStdout string
		wantStderr string // exact; "" means the check is wantErrIn
		wantErrIn  string // substring, with the log path exactly once
	}{
		{name: "roster number event is a type error", args: []string{"roster", "c1"}, log: str("5\n"), mode: 0o644, wantCode: 5, wantErrIn: "crew: roster: %s: "},
		{name: "sessions number event is a type error", args: []string{"sessions", "x"}, log: str("5\n"), mode: 0o644, wantCode: 5, wantErrIn: "crew: sessions: %s: "},
		{name: "roster garbage line is a decode error", args: []string{"roster", "c1"}, log: str("garbage\n"), mode: 0o644, wantCode: 5, wantErrIn: "crew: roster: %s: "},
		{name: "sessions garbage line is a decode error", args: []string{"sessions", "x"}, log: str("garbage\n"), mode: 0o644, wantCode: 5, wantErrIn: "crew: sessions: %s: "},
		{name: "roster unreadable log", args: []string{"roster", "c1"}, log: str("{}\n"), mode: 0, wantCode: 2, wantErrIn: "crew: roster: %s: permission denied\n"},
		{name: "sessions unreadable log still prints []", args: []string{"sessions", "x"}, log: str("{}\n"), mode: 0, wantCode: 2, wantStdout: "[]\n", wantErrIn: "crew: sessions: %s: permission denied\n"},
		{name: "sessions unreadable log is coloured on a terminal", args: []string{"sessions", "x"}, log: str("{}\n"), mode: 0, colour: true, wantCode: 2, wantStdout: "\x1b[1;39m[]\x1b[0m\n", wantErrIn: "permission denied"},
		{name: "sessions unreadable log honours JQ_COLORS", args: []string{"sessions", "x"}, log: str("{}\n"), mode: 0, colour: true, jqColors: "0;31:0;31:0;31:0;31:0;31:4;35", wantCode: 2, wantStdout: "\x1b[4;35m[]\x1b[0m\n", wantErrIn: "permission denied"},
		{name: "sessions unreadable log warns about a bad JQ_COLORS first", args: []string{"sessions", "x"}, log: str("{}\n"), mode: 0, jqColors: "bad", wantCode: 2, wantStdout: "[]\n", wantErrIn: "Failed to set $JQ_COLORS\ncrew: sessions: %s: permission denied\n"},
		{name: "roster exit error keeps its code and stderr", args: []string{"roster", "c1"}, log: str(exitedRow), mode: 0o644, worktrees: gitFailure, wantCode: 128, wantStderr: gitFailure.Stderr},
		{name: "roster absent log", args: []string{"roster", "c1"}},
		{name: "sessions absent log", args: []string{"sessions", "x"}, wantStdout: "[]\n"},
		{name: "roster empty log", args: []string{"roster", "c1"}, log: str(""), mode: 0o644, wantStdout: "[]\n"},
		{name: "sessions empty log", args: []string{"sessions", "x"}, log: str(""), mode: 0o644, wantStdout: "[]\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mode == 0 && tt.log != nil && os.Geteuid() == 0 {
				t.Skip("root reads a mode 000 file")
			}
			repo, logPath := repoWithLog(t, tt.log, tt.mode)
			e := env{
				getwd:    func() (string, error) { return repo, nil },
				jqColors: tt.jqColors,
				color:    tt.colour,
				probes: func(context.Context, string) roster.Probes {
					return roster.Probes{
						Panes: func() string { return "" },
						Worktrees: func() (string, error) {
							if tt.worktrees != nil {
								return "", tt.worktrees
							}
							return "", nil
						},
					}
				},
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tt.args, &stdout, &stderr, e); code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			switch {
			case tt.wantStderr != "":
				if stderr.String() != tt.wantStderr {
					t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
				}
			case tt.wantErrIn != "":
				want := tt.wantErrIn
				if strings.Contains(want, "%s") {
					want = fmt.Sprintf(want, logPath)
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
				if n := strings.Count(stderr.String(), logPath); n > 1 {
					t.Errorf("stderr names the log %d times, want at most once: %q", n, stderr.String())
				}
			default:
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want empty", stderr.String())
				}
			}
		})
	}
}
