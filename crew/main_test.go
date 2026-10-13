package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// crewsProbes fakes the pid probes crews reads: liveness is the live set and
// the ancestor walk follows the parents map.
func crewsProbes(live map[int]bool, parents map[int]int) crews.Probes {
	return crews.Probes{
		Alive:   func(pid int) bool { return live[pid] },
		Elapsed: func(int) (int64, bool) { return 0, false },
		Mtime:   func(string) (int64, bool) { return 0, false },
		Parent:  func(pid int) (int, bool) { n, ok := parents[pid]; return n, ok },
	}
}

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

func TestRunCrews(t *testing.T) {
	const crewsHeader = "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n"

	t.Run("absent bus prints just the header", func(t *testing.T) {
		repo, _ := repoWithLog(t, nil, 0)
		var stdout, stderr bytes.Buffer
		e := env{getwd: func() (string, error) { return repo, nil }}
		if code := run(context.Background(), []string{"crews"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		if stdout.String() != crewsHeader || stderr.Len() != 0 {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
	})

	t.Run("unknown arg is the arm's usage exit", func(t *testing.T) {
		repo, _ := repoWithLog(t, nil, 0)
		var stdout, stderr bytes.Buffer
		e := env{getwd: func() (string, error) { return repo, nil }}
		code := run(context.Background(), []string{"crews", "--mine=yes"}, &stdout, &stderr, e)
		if code != 64 || stdout.Len() != 0 || stderr.String() != "crew: crews: unknown arg '--mine=yes'\n" {
			t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("bad JQ_COLORS warns once when the table runs", func(t *testing.T) {
		log := `{"ts":1785951264000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1"}` + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		var stdout, stderr bytes.Buffer
		e := env{getwd: func() (string, error) { return repo, nil }, jqColors: "bad"}
		if code := run(context.Background(), []string{"crews"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		if stderr.String() != "Failed to set $JQ_COLORS\n" {
			t.Fatalf("stderr %q", stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), crewsHeader+"c1\t") || strings.Count(stdout.String(), "\n") != 2 {
			t.Fatalf("stdout %q", stdout.String())
		}
	})

	t.Run("--mine lists the live ancestor through the probes", func(t *testing.T) {
		repo, logPath := repoWithLog(t, nil, 0)
		crewDir := filepath.Join(filepath.Dir(logPath), "crews")
		for _, id := range []string{"c-anc", "c-str"} {
			if err := os.MkdirAll(filepath.Join(crewDir, id), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write := func(id, pid string) {
			if err := os.WriteFile(filepath.Join(crewDir, id, "pid"), []byte(pid+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("c-anc", "4242")
		write("c-str", "9999")
		var stdout, stderr bytes.Buffer
		e := env{
			getwd: func() (string, error) { return repo, nil },
			procs: crewsProbes(map[int]bool{4242: true, 9999: true}, map[int]int{os.Getpid(): 4242}),
		}
		if code := run(context.Background(), []string{"crews", "--mine"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		if stdout.String() != "c-anc\n" || stderr.Len() != 0 {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
	})
}

// The bus rows the two new arms read; the expectations are the bash arms'.
const (
	logRowC1 = `{"ts":1000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`
	logRowC2 = `{"ts":1100,"crew_id":"c2","kind":"msg","from":"dispatcher:c2","to":"worker:feat/y#s2-1"}`
	dispatch = `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"claude","model":"sonnet","tier":"standard"}`
)

func TestRunLogAndReport(t *testing.T) {
	const reportHeader = "engine\tmodel\ttier\tshape\toutcome\tduration_s\n"

	t.Run("log filters the crew and report folds its dispatch", func(t *testing.T) {
		log := logRowC1 + "\n" + logRowC2 + "\n" + dispatch + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		e := env{getwd: func() (string, error) { return repo, nil }}

		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"log", "c1"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		if want := logRowC1 + "\n" + dispatch + "\n"; stdout.String() != want || stderr.Len() != 0 {
			t.Fatalf("log: stdout %q stderr %q", stdout.String(), stderr.String())
		}

		stdout, stderr = bytes.Buffer{}, bytes.Buffer{}
		if code := run(context.Background(), []string{"report", "c1"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		// The status row above is the dispatched session's, so the fold resolves
		// its outcome against it.
		want := reportHeader + "claude\tsonnet\tstandard\t\u2014\tworking\t0\n"
		if stdout.String() != want || stderr.Len() != 0 {
			t.Fatalf("report: stdout %q stderr %q", stdout.String(), stderr.String())
		}
	})

	t.Run("the crew defaults to this repo's", func(t *testing.T) {
		log := logRowC2 + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		t.Setenv("CREW_ID", "c2")
		e := env{getwd: func() (string, error) { return repo, nil }}

		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"log"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		if want := logRowC2 + "\n"; stdout.String() != want || stderr.Len() != 0 {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
	})

	// Both arms print nothing before the `[ -f "$log" ]` guard, and report keeps
	// its header through a fold failure because it prints it before jq starts.
	t.Run("an absent bus is silent for both", func(t *testing.T) {
		repo, _ := repoWithLog(t, nil, 0)
		e := env{getwd: func() (string, error) { return repo, nil }}
		for _, sub := range []string{"log", "report"} {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{sub, "c1"}, &stdout, &stderr, e); code != 0 {
				t.Fatalf("%s: code %d", sub, code)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("%s: stdout %q stderr %q", sub, stdout.String(), stderr.String())
			}
		}
	})

	t.Run("a torn log costs report every row but keeps log's prefix", func(t *testing.T) {
		log := logRowC1 + "\n" + dispatch + "\n" + `{"crew_id":"c1","ts":3` + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		e := env{getwd: func() (string, error) { return repo, nil }}

		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"log", "c1"}, &stdout, &stderr, e); code != 5 {
			t.Fatalf("log: code %d, want 5", code)
		}
		if want := logRowC1 + "\n" + dispatch + "\n"; stdout.String() != want {
			t.Fatalf("log: stdout %q, want %q", stdout.String(), want)
		}

		stdout, stderr = bytes.Buffer{}, bytes.Buffer{}
		if code := run(context.Background(), []string{"report", "c1"}, &stdout, &stderr, e); code != 5 {
			t.Fatalf("report: code %d, want 5", code)
		}
		if stdout.String() != reportHeader {
			t.Fatalf("report: stdout %q, want just the header", stdout.String())
		}
	})

	t.Run("an unknown subcommand is the grouped help on stderr", func(t *testing.T) {
		repo, _ := repoWithLog(t, nil, 0)
		var stdout, stderr bytes.Buffer
		e := env{getwd: func() (string, error) { return repo, nil }}
		if code := run(context.Background(), []string{"logs"}, &stdout, &stderr, e); code != exitFailure {
			t.Fatalf("code %d, want %d", code, exitFailure)
		}
		if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), helpTopTitle) {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
	})
}

// helpTopTitle is the grouped list's first line: its presence says a screen was
// printed, and its absence says the invocation was routed to a subcommand.
const helpTopTitle = "crew — file-based coordination bus for dispatcher and worker sessions"

// TestRunHelpRouting is crew.sh's help preamble, position for position: which
// `--help` answers with a screen and which stays an argument (#812). The screen
// rows run with an unreadable cwd, so a screen can only come from the routing —
// `crew <sub> --help` answers with no repo, no crew id and no bus.
func TestRunHelpRouting(t *testing.T) {
	noCwd := env{getwd: func() (string, error) { return "", errors.New("getwd: no such file") }}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"--help", []string{"--help"}, helpTopTitle},
		{"-h", []string{"-h"}, helpTopTitle},
		{"help", []string{"help"}, helpTopTitle},
		{"--help with a sub", []string{"--help", "roster"}, "usage: crew roster"},
		{"--help with a sub and a subsub", []string{"--help", "hold", "add"}, "usage: crew hold add"},
		{"help sub", []string{"help", "roster"}, "usage: crew roster"},
		{"sub --help", []string{"roster", "--help"}, "usage: crew roster"},
		{"sub -h", []string{"roster", "-h"}, "usage: crew roster"},
		{"hold --help is the bare screen", []string{"hold", "--help"}, "usage: crew hold add"},
		{"hold action --help", []string{"hold", "list", "--help"}, "usage: crew hold list"},
		{"help hold action", []string{"help", "hold", "list"}, "usage: crew hold list"},
		{"fleet --help", []string{"fleet", "--help"}, "usage: crew fleet"},
		{"fleet stream --help", []string{"fleet", "stream", "--help"}, "usage: crew fleet"},
		{"pi-agent-dir --help", []string{"pi-agent-dir", "--help"}, "usage: crew pi-agent-dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tc.args, &stdout, &stderr, noCwd); code != 0 {
				t.Fatalf("code %d, want 0 (stderr %q)", code, stderr.String())
			}
			if !strings.HasPrefix(stdout.String(), tc.want) {
				t.Fatalf("stdout %q, want a screen starting %q", stdout.String(), tc.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr %q, want none", stderr.String())
			}
		})
	}

	// --help counts in the first position only: past the first argument it is
	// detail text, so this is the `status` arm refusing for want of a repo and
	// not a screen.
	t.Run("--help past the first argument is detail text", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GIT_CEILING_DIRECTORIES", dir)
		e := env{getwd: func() (string, error) { return dir, nil }}
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"status", "w1", "working", "--help"}, &stdout, &stderr, e)
		if code != exitFailure || stdout.Len() != 0 || stderr.String() != "crew: not in a git repo\n" {
			t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
		}
	})

	// The two invocations crew.sh's `*)` arm answered: exit 1, and the whole
	// grouped list on stderr rather than stdout.
	t.Run("bare crew and an unknown subcommand list on stderr", func(t *testing.T) {
		for _, args := range [][]string{nil, {}, {"nosuchcmd"}, {"logs"}, {"--bogus"}} {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr, noCwd); code != exitFailure {
				t.Fatalf("%v: code %d, want %d", args, code, exitFailure)
			}
			if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), helpTopTitle) {
				t.Fatalf("%v: stdout %q stderr %q", args, stdout.String(), stderr.String())
			}
		}
	})
}

func TestRunStallWatchUsage(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	var stdout, stderr bytes.Buffer
	e := env{getwd: func() (string, error) { return repo, nil }}
	code := run(context.Background(), []string{"stall-watch"}, &stdout, &stderr, e)
	if code != exitFailure || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "crew: stall-watch <worker-id|branch|role:branch:role> --pane <id> ") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// The arm has two halves: the msgs it prints, and the delivered-marks file it
// leaves for `crew await` and the in-process `internal/unread` scan to read.
// Both are wired through bus.Paths, so the marks land under the repo's crew dir next to the bus.
func TestRunInbox(t *testing.T) {
	const (
		me   = "worker:feat/x#s1-1"
		msg  = `{"ts":1000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"worker:feat/x#s1-1","body":{"text":"go"}}`
		name = ".git/crew/await"
	)

	// marksFile is the one file the run left in the await dir.
	marksFile := func(t *testing.T, repo string) string {
		t.Helper()
		files, err := filepath.Glob(filepath.Join(repo, name, "*"))
		if err != nil || len(files) != 1 {
			t.Fatalf("await dir holds %v (%v)", files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("prints the agent's msgs and records its marks", func(t *testing.T) {
		log := msg + "\n" + logRowC2 + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		e := env{getwd: func() (string, error) { return repo, nil }}

		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"inbox", me, "c1"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d stderr %q", code, stderr.String())
		}
		if stdout.String() != msg+"\n" || stderr.Len() != 0 {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
		if got := marksFile(t, repo); got != "{\"dispatcher:c1\":1000}\n" {
			t.Fatalf("marks %q", got)
		}
	})

	t.Run("the crew defaults to this repo's", func(t *testing.T) {
		log := msg + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		t.Setenv("CREW_ID", "c1")
		e := env{getwd: func() (string, error) { return repo, nil }}

		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"inbox", me}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d stderr %q", code, stderr.String())
		}
		if stdout.String() != msg+"\n" {
			t.Fatalf("stdout %q", stdout.String())
		}
	})

	t.Run("an absent bus is silent", func(t *testing.T) {
		repo, _ := repoWithLog(t, nil, 0)
		e := env{getwd: func() (string, error) { return repo, nil }}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"inbox", me, "c1"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d", code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
		if files, _ := filepath.Glob(filepath.Join(repo, name, "*")); len(files) != 0 {
			t.Fatalf("await dir holds %v", files)
		}
	})

	t.Run("a branch-only worker id is the arm's error", func(t *testing.T) {
		log := msg + "\n"
		repo, _ := repoWithLog(t, &log, 0o644)
		e := env{getwd: func() (string, error) { return repo, nil }}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"inbox", "worker:feat/x", "c1"}, &stdout, &stderr, e); code != exitFailure {
			t.Fatalf("code %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "has no session suffix") {
			t.Fatalf("stderr %q", stderr.String())
		}
	})
}

// busRepo makes a git repo with a one-row bus and returns its git dir.
func busRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v\n%s", err, out)
	}
	common := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	row := `{"ts":1700000000000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}` + "\n"
	if err := os.WriteFile(filepath.Join(common, "crew", "events.jsonl"), []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	return common
}

// TestRunRateSweepAllOutsideRepo pins the routing the bash preamble used to own:
// --sweep-all runs from anywhere, the repo-keyed modes do not.
func TestRunRateSweepAllOutsideRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CREW_SWEEP_ROOTS", "")
	root := t.TempDir()
	common := busRepo(t, root, "alpha") // a bus, but no origin remote
	e := env{getwd: func() (string, error) { return t.TempDir(), nil }}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"rate", "--sweep-all", "--root", root}, &stdout, &stderr, e)
	want := filepath.Dir(common) + ": skipped (no origin remote)\n"
	if code != 0 || stdout.String() != want {
		t.Fatalf("code %d stdout %q (want %q) stderr %q", code, stdout.String(), want, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"rate"}, &stdout, &stderr, e); code != exitFailure ||
		stdout.Len() != 0 || stderr.String() != "crew: not in a git repo\n" {
		t.Errorf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"rate", "--sweep-all", "--report"}, &stdout, &stderr, e); code != exitFailure ||
		stderr.String() != "crew: rate takes --report, --json, --pooled, and --sweep-all/--root\n" {
		t.Errorf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// Every subcommand that reads the bus refuses outside a git repo with one
// line, whatever its arguments: the check that crew.sh's preamble used to make.
func TestRunBusSubsRefuseOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	e := env{getwd: func() (string, error) { return dir, nil }}
	for _, sub := range []string{
		"crews", "log", "report", "sessions", "roster", "identity", "inbox", "hold", "await", "watch",
		"retro", "reply", "resolve-target", "pr-watch", "where", "stream", "roster-render", "adopt",
		"status", "msg", "register", "deregister", "rate", "git-baseline", "reap", "stack", "nudge", "stall-watch",
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{sub}, &stdout, &stderr, e)
		if code != exitFailure || stdout.Len() != 0 || stderr.String() != "crew: not in a git repo\n" {
			t.Errorf("%s: code %d stdout %q stderr %q", sub, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunFleetOutsideRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CREW_SWEEP_ROOTS", "")
	common := busRepo(t, filepath.Join(home, "git"), "alpha")
	e := env{getwd: func() (string, error) { return "", os.ErrNotExist }}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"fleet", "--all"}, &stdout, &stderr, e)
	if code != 0 || !strings.Contains(stdout.String(), "feat/x") || !strings.HasPrefix(stdout.String(), "REPO") {
		t.Fatalf("code %d stdout %q stderr %q (bus %s)", code, stdout.String(), stderr.String(), common)
	}
}

func TestRunFleetStreamNeedsCrew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CREW_SWEEP_ROOTS", "")
	busRepo(t, filepath.Join(home, "git"), "alpha")
	e := env{getwd: func() (string, error) { return "", os.ErrNotExist }}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"fleet", "stream"}, &stdout, &stderr, e)
	if code != 64 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--crew ID is required") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"fleet", "--all"}, &stdout, &stderr, e); code != 0 || !strings.HasPrefix(stdout.String(), "REPO") {
		t.Fatalf("fleet --all after fleet stream: code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunGitGuardAndBaselineOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	e := env{getwd: func() (string, error) { return dir, nil }}
	for _, tc := range []struct {
		args           []string
		code           int
		stdout, stderr string
	}{
		{[]string{"git-guard"}, exitUsage, "", "crew-go: git-guard: usage: crew-go git-guard check|check-cwd|init|commondir|pairs|union|show-url|show-key|show-pair|canon …\n"},
		{[]string{"git-guard", "canon", "/r", "core.hookspath\n.git/hooks"}, 0, "core.hookspath\n/r/hooks", ""},
		{[]string{"git-baseline"}, exitFailure, "", "crew: not in a git repo\n"},
		{[]string{"git-baseline", "--bogus"}, exitFailure, "", "usage: crew git-baseline [--accept]\n"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
			t.Errorf("%q: code %d stdout %q stderr %q", tc.args, code, stdout.String(), stderr.String())
		}
	}
}

// TestRunEntryArmsSurviveAnUnreadableCwd: a worktree reaped out from under a
// live shell leaves bash printing a shell-init warning and answering anyway,
// because crew.sh's entry arms never asked where they were. The Go arms must not
// turn that into a refusal: `crew engine-cmd`'s exit 0 is a verdict dispatch
// reads, and `crew --help` answering is the whole point of a preamble that runs
// before the repo check (#1007).
func TestRunEntryArmsSurviveAnUnreadableCwd(t *testing.T) {
	e := env{getwd: func() (string, error) {
		return "", errors.New("getwd: no such file or directory")
	}}
	t.Setenv("CREW_ID", "c-env")

	t.Run("help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"help"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "crew — file-based coordination bus") {
			t.Fatalf("stdout %q, want the grouped list", stdout.String())
		}
	})

	t.Run("help <sub>", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"help", "status"}, &stdout, &stderr, e); code != 0 ||
			!strings.HasPrefix(stdout.String(), "usage: crew status") {
			t.Fatalf("code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("new", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"new"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr.String())
		}
		if !regexp.MustCompile(`^[0-9]+-[0-9]+\n$`).MatchString(stdout.String()) {
			t.Fatalf("stdout %q, want a timestamp-pid", stdout.String())
		}
	})

	// engine-cmd answers from its table alone, and its exit is the answer.
	for _, c := range []struct {
		cmd  string
		want int
	}{{"pi", 0}, {"fish", 1}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"engine-cmd", c.cmd}, &stdout, &stderr, e); code != c.want {
			t.Fatalf("engine-cmd %s = %d, want %d (stderr %q)", c.cmd, code, c.want, stderr.String())
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("engine-cmd %s wrote %q / %q", c.cmd, stdout.String(), stderr.String())
		}
	}

	// id resolves what it can and prints it: from a cwd git cannot read that is
	// the env, and the only thing it must never do is fail over where it was.
	t.Run("id", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"id"}, &stdout, &stderr, e); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr.String())
		}
		if strings.Contains(stderr.String(), "getwd") {
			t.Fatalf("stderr %q, want no working-directory error", stderr.String())
		}
		if line := strings.TrimSuffix(stdout.String(), "\n"); line == "" || strings.Contains(line, "\n") {
			t.Fatalf("stdout %q, want one id", stdout.String())
		}
	})
}

// The kong root (#862): which names the grammar admits, what kong's capture
// actually holds, and that an arm's argv arrives as its caller typed it.
//
// Every expected value below is copied from the base build of this branch:
//
//	mkdir -p /tmp/crew862 && git archive 5e53264b crew | tar -x -C /tmp/crew862
//	(cd /tmp/crew862/crew && go build -o /tmp/crew862/crew-base .)
//	tmp=$(mktemp -d) && cd "$tmp" && git init -q . && mkdir -p .git/crew
//	printf '%s\n' '<the bus row below>' > .git/crew/events.jsonl
//	CREW_ID=c1 /tmp/crew862/crew-base <the row's args>; echo $?
//
// and its `code` is that shell exit status.
var statusRow = `{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working","detail":"go"}}` + "\n"

// noTouchProbes is the roster probe set for a run that must read neither tmux
// nor a worktree list: the folds answer from the bus alone.
func noTouchProbes() func(context.Context, string) roster.Probes {
	return func(context.Context, string) roster.Probes {
		return roster.Probes{
			Panes:     func() string { return "" },
			Worktrees: func() (string, error) { return "", nil },
		}
	}
}

// TestAdmittedSubcommands pins the grammar against the switch that admitted
// these names before this file had one. A name that silently drops out of the
// grammar becomes "unknown subcommand" for every protocol, script and agent
// that shells out to it, which no test of the arm itself would notice.
func TestAdmittedSubcommands(t *testing.T) {
	// The `case` list of run() at 5e53264b, copied out of that revision.
	admitted := []string{
		"roster", "sessions", "crews", "log", "report", "inbox", "hold", "await",
		"watch", "retro", "rate", "reply", "resolve-target", "pr-watch", "where",
		"identity", "stream", "roster-render", "adopt", "nudge", "stall-watch",
		"status", "msg", "register", "deregister", "fleet", "git-guard",
		"git-baseline", "occupants", "reap", "reap-autosweep", "stack", "help",
		"id", "new", "engine-cmd", "dash", "pi-agent-dir",
	}
	for _, name := range []string{"engine-cmd", "id", "new", "occupants"} {
		if !typedSubcommands[name] {
			t.Errorf("%s types no argv", name)
		}
		if !slices.Contains(subcommands, name) {
			t.Errorf("%s is typed but admitted by no list", name)
		}
	}
	p, err := newCrewParser(&root{}, &passthroughCmd{})
	if err != nil {
		t.Fatalf("grammar: %v", err)
	}
	got := make([]string, 0, len(admitted))
	for _, child := range p.Model.Children {
		got = append(got, child.Name)
	}
	sort.Strings(got)
	sort.Strings(admitted)
	// The set, not the order: which names answer, not which line of the grammar
	// declares them.
	if strings.Join(got, " ") != strings.Join(admitted, " ") {
		missing, extra := []string{}, []string{}
		seen := map[string]bool{}
		for _, n := range admitted {
			seen[n] = true
		}
		for _, n := range got {
			if !seen[n] {
				extra = append(extra, n)
			}
			delete(seen, n)
		}
		for _, n := range admitted {
			if seen[n] {
				missing = append(missing, n)
			}
		}
		t.Errorf("the grammar admits a different set: missing %v, extra %v", missing, extra)
	}
}

// TestParseCrewSplitsLeadingEquals is the measurement that decides who parses
// what: kong splits the first word after a command name at `=`, so the capture
// is not the argv an arm may be handed. `crew inbox --since=abc` is a recipient
// that matches no message today; the split makes it a --since filter and then a
// refusal, which is a different command.
func TestParseCrewSplitsLeadingEquals(t *testing.T) {
	for _, tc := range []struct {
		args, captured []string
	}{
		{[]string{"inbox", "--since=abc"}, []string{"--since", "abc"}},
		{[]string{"roster", "--since=5"}, []string{"--since", "5"}},
		{[]string{"reap", "--idle=5"}, []string{"--idle", "5"}},
		// Everything else about a passthrough command's argv does arrive whole,
		// `--` and flag-shaped detail text included.
		{[]string{"status", "w1", "working", "--help"}, []string{"w1", "working", "--help"}},
		{[]string{"reap", "--", "--idle=5"}, []string{"--", "--idle=5"}},
		{[]string{"log", "--"}, []string{"--"}},
		{[]string{"roster", "-x"}, []string{"-x"}},
		{[]string{"roster", ""}, []string{""}},
	} {
		var (
			r       root
			capture passthroughCmd
		)
		p, err := newCrewParser(&r, &capture)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Parse(tc.args); err != nil {
			t.Fatalf("%q: %v", tc.args, err)
		}
		if got := fmt.Sprintf("%q", capture.Args); got != fmt.Sprintf("%q", tc.captured) {
			t.Errorf("crew %q captured %s, want %s", tc.args[1:], got, tc.captured)
		}
	}
}

// TestParseCrewRejectsWhatTheSwitchRejected is the other half: the input kong
// refuses is exactly the input the switch's default arm refused, so the grouped
// help on stderr and exit 1 stay the only answer.
func TestParseCrewRejectsWhatTheSwitchRejected(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"bogus"}, {""}, {"-x"}, {"--help"}, {"--"},
		// kong swallows a leading `--` and matches the word after it, so these
		// parse and are refused here, which is where the switch refused them.
		{"--", "id"}, {"--", "status", "w1", "working"}, {"--", "msg", "a", "b", "{}"}} {
		if _, ok := parseCrew(args); ok {
			t.Errorf("crew %q parsed, want a refusal", args)
		}
	}
	for _, args := range [][]string{{"id"}, {"id", "extra", "--whatever", "-x"}, {"new"}, {"engine-cmd", "--pane=x"},
		{"occupants", "--foo"}, {"roster"}, {"status", "w1", "working", "--help"}, {"where", "--crew=x", "nova"},
		{"identity", "--hash=x"}, {"hold", "add", "--help"}, {"pi-agent-dir", "--", "-x"}} {
		if _, ok := parseCrew(args); !ok {
			t.Errorf("crew %q refused, want it to reach its arm", args)
		}
	}
}

// TestRunTerminatorIsNoSubcommand pins the token the grammar accepts and no arm
// admits: kong swallows a leading `--` and matches the word after it, so
// `crew -- id` parses. The answer is the one an unknown name gets — grouped list
// on stderr, nothing on stdout, exit 1.
func TestRunTerminatorIsNoSubcommand(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}
	var stdout, stderr, want bytes.Buffer
	if code := run(context.Background(), []string{"bogus"}, &stdout, &want, e); code != exitFailure {
		t.Fatalf("an unknown subcommand exited %d, want %d", code, exitFailure)
	}
	if want.Len() == 0 {
		t.Fatal("an unknown subcommand printed no list")
	}
	for _, args := range [][]string{{"--"}, {"--", "id"}, {"--", "roster"},
		{"--", "status", "w1", "working"}, {"--", "msg", "a", "b", "{}"}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), args, &stdout, &stderr, e); code != exitFailure {
			t.Errorf("crew %q exited %d, want %d", args, code, exitFailure)
		}
		if stdout.String() != "" {
			t.Errorf("crew %q printed %q on stdout, want nothing", args, stdout.String())
		}
		if stderr.String() != want.String() {
			t.Errorf("crew %q printed a different refusal than an unknown subcommand", args)
		}
	}
}

// TestRunConvertedSubcommands is the acceptance table for the four subcommands
// kong now parses: success, every refusal, the exact bytes and the exit status,
// all of it as the base build answered them.
func TestRunConvertedSubcommands(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	const (
		engineCmdUsage = "crew: engine-cmd <pane_current_command>\n"
		occupantsUsage = "crew: occupants <worktree-path>\n"
	)
	for _, tc := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
		custom         bool // the answer is a minted id, checked below
	}{
		{name: "id", args: []string{"id"}, code: 0, stdout: "c1\n"},
		// The arm ignored argv entirely, so an argument kong would otherwise
		// refuse is still just ignored.
		{name: "id ignores every argument", args: []string{"id", "extra", "--whatever", "-x"}, code: 0, stdout: "c1\n"},
		{name: "id ignores a separator", args: []string{"id", "--", "--"}, code: 0, stdout: "c1\n"},
		{name: "new mints", args: []string{"new"}, code: 0, custom: true},
		{name: "new ignores its arguments", args: []string{"new", "ignored"}, code: 0, custom: true},
		{name: "engine-cmd needs a command", args: []string{"engine-cmd"}, code: exitFailure, stderr: engineCmdUsage},
		{name: "engine-cmd empty command", args: []string{"engine-cmd", ""}, code: exitFailure, stderr: engineCmdUsage},
		{name: "engine-cmd engine", args: []string{"engine-cmd", "node"}, code: 0},
		{name: "engine-cmd not an engine", args: []string{"engine-cmd", "zzz"}, code: exitFailure},
		// A flag-shaped word is the pane command the arm was asked about, and
		// splitting `--pane=x` at the `=` cannot change which of the two it is.
		{name: "engine-cmd flag-shaped", args: []string{"engine-cmd", "-x"}, code: exitFailure},
		{name: "engine-cmd equals-shaped", args: []string{"engine-cmd", "--pane=x"}, code: exitFailure},
		{name: "engine-cmd extra arguments", args: []string{"engine-cmd", "claude", "extra"}, code: 0},
		{name: "occupants needs a path", args: []string{"occupants"}, code: exitFailure, stderr: occupantsUsage},
		{name: "occupants empty path", args: []string{"occupants", ""}, code: exitFailure, stderr: occupantsUsage},
		{name: "occupants flag-shaped path", args: []string{"occupants", "--foo"}, code: 0, stdout: "[]\n"},
		{name: "occupants equals-shaped path", args: []string{"occupants", "--path=x"}, code: 0, stdout: "[]\n"},
		{name: "occupants unknown path", args: []string{"occupants", "/tmp/definitely-not-a-worktree", "extra"}, code: 0, stdout: "[]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, e)
			if code != tc.code {
				t.Fatalf("code %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			if stderr.String() != tc.stderr {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
			switch {
			case tc.custom:
				if !regexp.MustCompile(`^\d{10}-\d+\n$`).MatchString(stdout.String()) {
					t.Errorf("stdout %q, want the <unix seconds>-<pid> the arm mints", stdout.String())
				}
			default:
				if stdout.String() != tc.stdout {
					t.Errorf("stdout %q, want %q", stdout.String(), tc.stdout)
				}
			}
		})
	}
}

// TestRunPassthroughArgvReachesItsArm is the other half of the contract: a
// subcommand kong only names still parses its own words. Each row is an answer
// only the arm's own parsing produces, so a dropped, split or re-read token
// fails it — `--since=abc` as one word, `--idle=5` as an unknown flag rather
// than a value, `--help` as detail text.
func TestRunPassthroughArgvReachesItsArm(t *testing.T) {
	repo, _ := repoWithLog(t, &statusRow, 0o644)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	for _, tc := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
	}{
		{name: "inbox reads --since=abc as its recipient", args: []string{"inbox", "worker:feat/x#s1-1", "--since=abc"}},
		{name: "inbox reads --since with no value", args: []string{"inbox", "worker:feat/x#s1-1", "--since"},
			code: exitFailure, stderr: "crew: --since needs a value\n"},
		{name: "inbox reads --since and its value", args: []string{"inbox", "worker:feat/x#s1-1", "--since", "abc"},
			code: exitFailure, stderr: "crew: --since must be an integer ms timestamp\n"},
		{name: "roster reads an unknown flag as a crew", args: []string{"roster", "--since=5"}, stdout: "[]\n"},
		{name: "log reads a separator as a crew", args: []string{"log", "--", "--since"}},
		{name: "reap refuses --idle=5 as an unknown flag", args: []string{"reap", "--idle=5"}, code: exitFailure,
			stderr: "crew: reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH (got '--idle=5')\n"},
		{name: "reap keeps words after a separator together", args: []string{"reap", "--dry-run", "--idle=5"}, code: exitFailure,
			stderr: "crew: reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH (got '--idle=5')\n"},
		{name: "hold reads --crew with no value", args: []string{"hold", "list", "--crew"}, code: exitFailure,
			stderr: "crew: --crew needs a value\n"},
		{name: "status reads --help as an unknown argument", args: []string{"status", "w1", "working", "--help"},
			code: exitFailure, stderr: "crew: status: unknown arg '--help' (use -- before a detail that starts with dashes)\n"},
		{name: "where keeps its own flag grammar", args: []string{"where", "--crew=x", "nova"}, code: exitFailure,
			stderr: "crew: where: unknown flag '--crew=x' (usage: crew where <codename|branch|%id> [--crew ID])\n"},
		{name: "identity reads --hash=x as a branch", args: []string{"identity", "--hash=x"},
			stdout: `{"name":"rust","color":"rust","tmux":"colour166"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, e)
			if code != tc.code {
				t.Fatalf("code %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			if stdout.String() != tc.stdout {
				t.Errorf("stdout %q, want %q", stdout.String(), tc.stdout)
			}
			if stderr.String() != tc.stderr {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
		})
	}
}
