package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/nudge"
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
		code := run(context.Background(), []string{"crews", "--bogus=yes"}, &stdout, &stderr, e)
		if code != 64 || stdout.Len() != 0 || stderr.String() != "crew: crews: unknown arg '--bogus'\n" {
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

	for _, spelling := range []string{"--mine", "--mine=yes", "--mine=true"} {
		t.Run(spelling+" lists the live ancestor through the probes", func(t *testing.T) {
			testCrewsMine(t, spelling)
		})
	}
}

func testCrewsMine(t *testing.T, spelling string) {
	t.Helper()
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
	if code := run(context.Background(), []string{"crews", spelling}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d", code)
	}
	if stdout.String() != "c-anc\n" || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
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
	// A typed subcommand's bad argv is refused after the repo check, not before.
	for _, tc := range refusalRows {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != exitFailure || stdout.Len() != 0 || stderr.String() != "crew: not in a git repo\n" {
			t.Errorf("%s: code %d stdout %q stderr %q", tc.name, code, stdout.String(), stderr.String())
		}
	}
}

// The fleet and rate --sweep-all refusals print before the repo check, because
// those modes run outside any repo.
func TestPreLocateRefusalsPrintOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	e := env{getwd: func() (string, error) { return dir, nil }}
	for _, tc := range preLocateRefusalRows {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != tc.code || stdout.Len() != 0 || stderr.String() != tc.msg+"\n" {
			t.Errorf("%s: code %d stdout %q stderr %q, want %d and %q", tc.name, code, stdout.String(), stderr.String(), tc.code, tc.msg)
		}
	}
	// kong reads `--root --sweep-all` as a bad line, where the bash glob saw the
	// mode and the arm took --sweep-all as the directory.
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"rate", "--root", "--sweep-all"}, &stdout, &stderr, e)
	if code != exitFailure || stdout.Len() != 0 || stderr.String() != rateUsage+"\n" {
		t.Errorf("rate --root --sweep-all: code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
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
	for _, name := range []string{"engine-cmd", "id", "new", "occupants",
		"where", "identity", "resolve-target", "crews", "register", "deregister", "log", "sessions", "inbox",
		"roster", "report", "retro", "rate", "fleet", "stack", "roster-render", "pi-agent-dir",
		"status", "msg", "reply", "hold", "nudge", "await", "watch"} {
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
// is not the argv an unconverted arm may be handed. `crew adopt --crew=x` is a
// word the arm reads whole; the split makes it `--crew x`, a different
// command. The typed subcommands want the split (TestFlagSpellingsAreIdentical).
func TestParseCrewSplitsLeadingEquals(t *testing.T) {
	for _, tc := range []struct {
		args, captured []string
	}{
		{[]string{"adopt", "--crew=x"}, []string{"--crew", "x"}},
		{[]string{"pr-watch", "--since=5"}, []string{"--since", "5"}},
		{[]string{"reap", "--idle=5"}, []string{"--idle", "5"}},
		// Everything else about a passthrough command's argv does arrive whole,
		// `--` and flag-shaped detail text included.
		{[]string{"pr-watch", "w1", "working", "--help"}, []string{"w1", "working", "--help"}},
		{[]string{"reap", "--", "--idle=5"}, []string{"--", "--idle=5"}},
		{[]string{"adopt", "--"}, []string{"--"}},
		{[]string{"adopt", "-x"}, []string{"-x"}},
		{[]string{"adopt", ""}, []string{""}},
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
		// A typed subcommand's bad argv is that subcommand's refusal, not an unknown name.
		{"where", "--bogus"}, {"inbox", "--since"}, {"crews", "extra"},
		{"roster", "--bogus"}, {"rate", "--root"}, {"fleet", "extra"}, {"stack"}, {"roster-render", "--once=nope"},
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
// fails it — `--idle=5` as an unknown flag rather than a value, `--help` as
// detail text.
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
		{name: "reap refuses --idle=5 as an unknown flag", args: []string{"reap", "--idle=5"}, code: exitFailure,
			stderr: "crew: reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH (got '--idle=5')\n"},
		{name: "reap keeps words after a separator together", args: []string{"reap", "--dry-run", "--idle=5"}, code: exitFailure,
			stderr: "crew: reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH (got '--idle=5')\n"},
		{name: "adopt reads a flag-shaped id whole", args: []string{"adopt", "--bogus"}, code: exitFailure,
			stderr: "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n"},
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

// refusalRows is each typed subcommand's grammar and semantic refusals: the
// stderr line and exit status the old argv loop of that arm printed, plus the
// kong-shaped ones for input the loops ignored. Every row of the same kind
// (unknown flag, extra positional, missing value, empty value) is here for
// each subcommand that has one.
var refusalRows = []struct {
	name string
	args []string
	msg  string
	code int
}{
	{"where unknown flag", []string{"where", "--bogus", "nova"}, "crew: where: unknown flag '--bogus' (" + whereUsage + ")", 1},
	{"where extra positional", []string{"where", "nova", "iris"}, "crew: where: one target only (" + whereUsage + ")", 1},
	{"where missing value", []string{"where", "nova", "--crew"}, "crew: where: --crew needs an id (" + whereUsage + ")", 1},
	{"where empty value", []string{"where", "--crew", "", "nova"}, "crew: where: --crew needs an id (" + whereUsage + ")", 1},
	{"where empty value equals", []string{"where", "--crew=", "nova"}, "crew: where: --crew needs an id (" + whereUsage + ")", 1},
	{"where no target", []string{"where"}, "crew: where: " + whereUsage, 1},

	{"resolve-target unknown flag", []string{"resolve-target", "--bogus", "sage"}, "crew: resolve-target: unknown flag '--bogus'", 1},
	{"resolve-target extra positional", []string{"resolve-target", "sage", "again"}, "crew: resolve-target: one target only", 1},
	{"resolve-target missing value", []string{"resolve-target", "sage", "--crew"}, "crew: resolve-target: --crew needs an id", 1},
	{"resolve-target empty value", []string{"resolve-target", "--crew", "", "sage"}, "crew: resolve-target: --crew needs an id", 1},
	{"resolve-target no target", []string{"resolve-target"}, "usage: crew resolve-target <target> [--crew ID]", 1},

	{"identity unknown flag", []string{"identity", "--bogus"}, "crew: identity: unknown flag '--bogus' (usage: crew identity <branch> [crew])", 1},
	{"identity extra positional", []string{"identity", "a", "b", "c"}, "crew: identity <branch> [crew]", 1},
	{"identity missing value", []string{"identity", "--hash"}, "crew: identity --hash <name>", 1},
	{"identity empty value", []string{"identity", "--hash", ""}, "crew: identity --hash <name>", 1},
	{"identity no branch", []string{"identity"}, "crew: identity <branch> [crew]", 1},
	{"identity empty branch", []string{"identity", ""}, "crew: identity <branch> [crew]", 1},

	{"crews unknown flag", []string{"crews", "--bogus"}, "crew: crews: unknown arg '--bogus'", 64},
	{"crews empty positional", []string{"crews", ""}, "crew: crews: unknown arg ''", 64},
	{"crews extra positional", []string{"crews", "x"}, "crew: crews: unknown arg 'x'", 64},
	{"crews bad bool", []string{"crews", "--mine=nope"}, `crew: crews: --mine: bool value must be true, 1, yes, false, 0 or no but got "nope"`, 64},

	{"register unknown flag", []string{"register", "--bogus"}, "crew: register: unknown flag '--bogus'", 1},
	{"register extra positional", []string{"register", "1", "2"}, "crew: register: unexpected argument '2'", 1},
	{"deregister unknown flag", []string{"deregister", "--bogus"}, "crew: deregister: unknown flag '--bogus'", 1},
	{"deregister extra positional", []string{"deregister", "1"}, "crew: deregister: unexpected argument '1'", 1},
	{"log unknown flag", []string{"log", "--bogus"}, "crew: log: unknown flag '--bogus'", 1},
	{"log extra positional", []string{"log", "c1", "c2"}, "crew: log: unexpected argument 'c2'", 1},

	{"sessions unknown flag", []string{"sessions", "feat/x", "--bogus"}, sessionsUsage, 1},
	{"sessions extra positional", []string{"sessions", "feat/x", "more"}, sessionsUsage, 1},
	{"sessions missing value", []string{"sessions", "feat/x", "--crew"}, "crew: --crew needs a value", 1},
	{"sessions empty value", []string{"sessions", "feat/x", "--crew", ""}, "crew: --crew needs a value", 1},
	{"sessions no branch", []string{"sessions"}, sessionsUsage, 1},
	{"sessions empty branch", []string{"sessions", ""}, sessionsUsage, 1},

	{"inbox unknown flag", []string{"inbox", "--bogus"}, "crew: inbox: unknown flag '--bogus'", 1},
	{"inbox extra positional", []string{"inbox", "me", "c1", "c2"}, "crew: inbox: unexpected argument 'c2'", 1},
	{"inbox since missing value", []string{"inbox", "me", "--since"}, "crew: --since needs a value", 1},
	{"inbox since empty value", []string{"inbox", "me", "--since", ""}, "crew: --since needs a value", 1},
	{"inbox from missing value", []string{"inbox", "me", "--from"}, "crew: --from needs a value", 1},
	{"inbox from empty value", []string{"inbox", "me", "--from="}, "crew: --from needs a value", 1},
	{"inbox bad bool", []string{"inbox", "me", "--undelivered=x"}, `crew: inbox: --undelivered: bool value must be true, 1, yes, false, 0 or no but got "x"`, 1},
	{"inbox branch-only worker id", []string{"inbox", "worker:feat/x", "--since"},
		"crew: inbox: 'worker:feat/x' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message", 1},
	{"inbox undelivered needs a session", []string{"inbox", "dispatcher:c1", "--undelivered"},
		"crew: inbox: --undelivered needs a worker session id ($CREW_WORKER_ID)", 1},

	{"roster unknown flag", []string{"roster", "--bogus"}, "crew: roster: unknown flag '--bogus'", 1},
	{"roster extra positional", []string{"roster", "c1", "c2"}, "crew: roster: unexpected argument 'c2'", 1},
	{"report unknown flag", []string{"report", "--bogus"}, "crew: report: unknown flag '--bogus'", 1},
	{"report extra positional", []string{"report", "c1", "c2"}, "crew: report: unexpected argument 'c2'", 1},

	{"retro unknown flag", []string{"retro", "--bogus"}, retroUsage, 1},
	{"retro extra positional", []string{"retro", "c1"}, retroUsage, 1},
	{"retro bad bool", []string{"retro", "--report=nope"}, retroUsage, 1},

	{"rate unknown flag", []string{"rate", "--bogus"}, rateUsage, 1},
	{"rate extra positional", []string{"rate", "x"}, rateUsage, 1},
	{"rate bad bool", []string{"rate", "--report=nope"}, rateUsage, 1},
	{"rate root missing value", []string{"rate", "--root"}, rateRootNeedsDir, 1},
	{"rate root empty value", []string{"rate", "--root", ""}, rateRootNeedsDir, 1},
	{"rate root empty value equals", []string{"rate", "--root="}, rateRootNeedsDir, 1},
	{"rate root empty among others", []string{"rate", "--root", "/x", "--root", ""}, rateRootNeedsDir, 1},

	{"stack no subcommand", []string{"stack"}, stackUsage, 1},
	{"stack unknown subcommand", []string{"stack", "nonsense"}, stackUsage, 1},
	{"stack unknown flag", []string{"stack", "cut", "--bogus", "a"}, stackUsage, 1},
	{"stack cut no child", []string{"stack", "cut"}, stackUsage, 1},
	{"stack cut empty child", []string{"stack", "cut", ""}, stackUsage, 1},
	{"stack cut record no child", []string{"stack", "cut", "--record"}, stackUsage, 1},
	{"stack cut oid without record", []string{"stack", "cut", "a", "b"}, stackUsage, 1},
	{"stack cut extra positional", []string{"stack", "cut", "--record", "a", "b", "c"}, stackUsage, 1},
	{"stack cut bad bool", []string{"stack", "cut", "--record=nope", "a"}, stackUsage, 1},
	{"stack align no top", []string{"stack", "align"}, stackUsage, 1},
	{"stack align empty top", []string{"stack", "align", ""}, stackUsage, 1},
	{"stack align extra positional", []string{"stack", "align", "3", "4"}, stackUsage, 1},
	{"stack align record", []string{"stack", "align", "--record", "3"}, stackUsage, 1},

	{"status unknown flag", []string{"status", "w1", "blocked", "--bogus"},
		"crew: status: unknown arg '--bogus' (use -- before a detail that starts with dashes)", 1},
	{"status single-dash detail", []string{"status", "w1", "blocked", "-x"},
		"crew: status: unknown arg '-x' (use -- before a detail that starts with dashes)", 1},
	{"status extra positional", []string{"status", "w1", "blocked", "why", "url", "extra"}, "crew: status: unexpected argument 'extra'", 1},
	{"status bad bool", []string{"status", "w1", "blocked", "--restamp=nope"},
		`crew: status: --restamp: bool value must be true, 1, yes, false, 0 or no but got "nope"`, 1},

	{"reply unknown flag", []string{"reply", "--bogus", "worker:x", "go"}, "crew: reply: unknown flag '--bogus'", 1},
	{"reply single-dash body", []string{"reply", "worker:x", "-5"}, "crew: reply: unknown flag '-5'", 1},
	{"reply extra positional", []string{"reply", "worker:x", "go", "more"}, "crew: reply: unexpected argument 'more'", 1},
	{"reply crew missing value", []string{"reply", "worker:x", "go", "--crew"}, "crew: --crew needs a value", 1},
	{"reply crew empty value", []string{"reply", "worker:x", "go", "--crew", ""}, "crew: --crew needs a value", 1},
	{"reply crew empty value equals", []string{"reply", "--crew=", "worker:x", "go"}, "crew: --crew needs a value", 1},

	{"hold no action", []string{"hold"}, holdUsage, 1},
	{"hold unknown action", []string{"hold", "bogus"}, holdUsage, 1},
	{"hold flag as action", []string{"hold", "--bogus"}, holdUsage, 1},
	{"hold list unknown flag", []string{"hold", "list", "--bogus"}, "crew: hold list [--crew ID] [--json]", 1},
	{"hold list extra positional", []string{"hold", "list", "x"}, "crew: hold list [--crew ID] [--json]", 1},
	{"hold list bad bool", []string{"hold", "list", "--json=nope"}, "crew: hold list [--crew ID] [--json]", 1},
	{"hold list crew missing value", []string{"hold", "list", "--json", "--crew"}, "crew: --crew needs a value", 1},
	{"hold list crew empty value", []string{"hold", "list", "--crew", ""}, "crew: --crew needs a value", 1},
	{"hold list crew empty value equals", []string{"hold", "list", "--crew="}, "crew: --crew needs a value", 1},
	{"hold due unknown flag", []string{"hold", "due", "--bogus"}, "crew: hold due [--crew ID] [--json]", 1},
	{"hold due extra positional", []string{"hold", "due", "x"}, "crew: hold due [--crew ID] [--json]", 1},
	{"hold due crew missing value", []string{"hold", "due", "--crew"}, "crew: --crew needs a value", 1},
	{"hold due crew empty value", []string{"hold", "due", "--crew", ""}, "crew: --crew needs a value", 1},
	{"hold park json is unknown", []string{"hold", "park", "60", "--json"}, "crew: hold park <default> [--crew ID]", 1},
	{"hold park unknown flag", []string{"hold", "park", "60", "--bogus"}, "crew: hold park <default> [--crew ID]", 1},
	{"hold park extra positional", []string{"hold", "park", "60", "90"}, "crew: hold park <default> [--crew ID]", 1},
	{"hold park crew missing value", []string{"hold", "park", "60", "--crew"}, "crew: --crew needs a value", 1},
	{"hold park crew empty value", []string{"hold", "park", "60", "--crew", ""}, "crew: --crew needs a value", 1},
	{"hold release no id", []string{"hold", "release"}, "crew: hold release <id> [--crew ID]", 1},
	{"hold release empty id", []string{"hold", "release", ""}, "crew: hold release <id> [--crew ID]", 1},
	{"hold release json is unknown", []string{"hold", "release", "h1", "--json"}, "crew: hold release <id> [--crew ID]", 1},
	{"hold release extra positional", []string{"hold", "release", "h1", "h2"}, "crew: hold release <id> [--crew ID]", 1},
	{"hold release crew missing value", []string{"hold", "release", "h1", "--crew"}, "crew: --crew needs a value", 1},
	{"hold release crew empty value", []string{"hold", "release", "h1", "--crew", ""}, "crew: --crew needs a value", 1},
	{"hold add unknown flag", []string{"hold", "add", "--bogus"}, "crew: hold add: unknown arg '--bogus'", 1},
	{"hold add single-dash flag", []string{"hold", "add", "-x"}, "crew: hold add: unknown arg '-x'", 1},
	{"hold add unknown flag after values", []string{"hold", "add", "--engine", "e", "--bogus", "t"}, "crew: hold add: unknown arg '--bogus'", 1},
	{"hold add bad bool", []string{"hold", "add", "--draft=nope"},
		`crew: hold add: --draft: bool value must be true, 1, yes, false, 0 or no but got "nope"`, 1},
	{"hold add engine missing value", []string{"hold", "add", "--engine"}, "crew: --engine needs a value", 1},
	{"hold add window empty value", []string{"hold", "add", "--window", ""}, "crew: --window needs a value", 1},
	{"hold add resets-at empty value equals", []string{"hold", "add", "--resets-at="}, "crew: --resets-at needs a value", 1},
	{"hold add spec missing value", []string{"hold", "add", "--engine", "e", "--spec"}, "crew: --spec needs a value", 1},
	{"hold add crew empty value", []string{"hold", "add", "--crew", "", "t"}, "crew: --crew needs a value", 1},
	// The flag table's order decides which empty value is named, not argv order.
	{"hold add two empty values", []string{"hold", "add", "--model", "", "--window", ""}, "crew: --window needs a value", 1},

	{"nudge unknown flag", []string{"nudge", "--bogus", "nova"}, "crew: nudge: unknown flag '--bogus' (usage: " + nudge.Usage + ")", 1},
	{"nudge extra positional", []string{"nudge", "nova", "iris"}, "crew: nudge: one target only (usage: " + nudge.Usage + ")", 1},
	{"nudge crew missing value", []string{"nudge", "nova", "--crew"}, "crew: nudge: --crew needs an id (usage: " + nudge.Usage + ")", 1},
	{"nudge crew empty value", []string{"nudge", "--crew", "", "nova"}, "crew: nudge: --crew needs an id (usage: " + nudge.Usage + ")", 1},
	{"nudge crew empty value equals", []string{"nudge", "--crew=", "nova"}, "crew: nudge: --crew needs an id (usage: " + nudge.Usage + ")", 1},
	// `--wait abc` after a target is a second target, as the arm answered.
	{"nudge wait word after a target", []string{"nudge", "nova", "--wait", "abc"}, "crew: nudge: one target only (usage: " + nudge.Usage + ")", 1},
	{"nudge wait word before a target", []string{"nudge", "--wait", "abc", "nova"}, "crew: nudge: one target only (usage: " + nudge.Usage + ")", 1},

	{"await unknown flag", []string{"await", "worker:x#s1-1", "--bogus"}, "crew: await: unknown arg '--bogus'", 1},
	{"await extra positional", []string{"await", "worker:x#s1-1", "extra"}, "crew: await: unknown arg 'extra'", 1},
	{"await from missing value", []string{"await", "worker:x#s1-1", "--from"}, "crew: --from needs a value", 1},
	{"await timeout empty value", []string{"await", "worker:x#s1-1", "--timeout", ""}, "crew: --timeout needs a value", 1},
	{"await interval empty value equals", []string{"await", "worker:x#s1-1", "--interval="}, "crew: --interval needs a value", 1},

	{"watch unknown flag", []string{"watch", "--bogus"}, "crew: watch: unknown arg '--bogus'", 1},
	{"watch extra positional", []string{"watch", "x"}, "crew: watch: unknown arg 'x'", 1},
	{"watch since missing value", []string{"watch", "--since"}, "crew: --since needs a value", 1},
	{"watch states empty value", []string{"watch", "--states", ""}, "crew: --states needs a value", 1},
	{"watch timeout missing value", []string{"watch", "--timeout"}, "crew: --timeout needs a value", 1},
	{"watch interval empty value equals", []string{"watch", "--interval="}, "crew: --interval needs a value", 1},
	{"watch crew empty value", []string{"watch", "--crew", ""}, "crew: --crew needs a value", 1},

	{"roster-render unknown flag", []string{"roster-render", "--bogus"}, "crew: roster-render: unknown arg '--bogus'", 64},
	{"roster-render extra positional", []string{"roster-render", "--crew", "c1", "x"}, "crew: roster-render: unknown arg 'x'", 64},
	{"roster-render crew missing value", []string{"roster-render", "--crew"}, "crew: --crew needs a value", 64},
	{"roster-render crew empty value", []string{"roster-render", "--crew", ""}, "crew: --crew needs a value", 64},
	{"roster-render pane empty value equals", []string{"roster-render", "--pane="}, "crew: --pane needs a value", 64},
	{"roster-render interval missing value", []string{"roster-render", "--crew", "c1", "--interval"}, "crew: --interval needs a value", 64},
	{"roster-render quiet empty value", []string{"roster-render", "--quiet", ""}, "crew: --quiet needs a value", 64},
	{"roster-render bad bool", []string{"roster-render", "--once=nope"},
		`crew: roster-render: --once: bool value must be true, 1, yes, false, 0 or no but got "nope"`, 64},
	// An unknown argument outranks a bad interval, as it did in the arm's loop.
	{"roster-render unknown beats interval", []string{"roster-render", "--bogus", "--interval", "x"}, "crew: roster-render: unknown arg '--bogus'", 64},
}

// preLocateRefusalRows are the refusals of the two modes that run outside any
// repo: they print before the repo check, so they are not in refusalRows.
var preLocateRefusalRows = []struct {
	name string
	args []string
	msg  string
	code int
}{
	{"fleet unknown flag", []string{"fleet", "--bogus"}, fleetUsage, 1},
	{"fleet extra positional", []string{"fleet", "extra"}, fleetUsage, 1},
	{"fleet crew missing value", []string{"fleet", "--crew"}, fleetUsage, 1},
	{"fleet crew empty value", []string{"fleet", "--crew", ""}, fleetUsage, 1},
	{"fleet crew empty value equals", []string{"fleet", "--crew="}, fleetUsage, 1},
	{"fleet bad bool", []string{"fleet", "--all=nope"}, fleetUsage, 1},
	{"fleet stream behind a flag", []string{"fleet", "--json", "stream"}, fleetUsage, 1},
	{"rate sweep-all unknown flag", []string{"rate", "--sweep-all", "--bogus"}, rateUsage, 1},
	{"rate sweep-all root missing value", []string{"rate", "--sweep-all", "--root"}, rateRootNeedsDir, 1},
	{"rate sweep-all root empty value", []string{"rate", "--sweep-all", "--root", ""}, rateRootNeedsDir, 1},
	{"rate sweep-all root empty value equals", []string{"rate", "--sweep-all", "--root="}, rateRootNeedsDir, 1},
}

const (
	holdUsage        = "crew: hold add|list|due|park|release"
	retroUsage       = "crew: retro takes --report and --json"
	rateUsage        = "crew: rate takes --report, --json, --pooled, and --sweep-all/--root"
	fleetUsage       = "crew: fleet [--all] [--crew ID] [--json]"
	stackUsage       = "usage: crew stack cut [--record] <child-branch|PR#> [<oid>]\n       crew stack align <top-PR#|branch>"
	rateRootNeedsDir = "crew: rate --root needs a directory"
	whereUsage       = "usage: crew where <codename|branch|%id> [--crew ID]"
)

// TestTypedRefusals pins each typed subcommand's refusals at the parse: the
// line and status are the old arms', and every one is a refusal on a parse that
// still selected the subcommand.
func TestTypedRefusals(t *testing.T) {
	for _, tc := range slices.Concat(refusalRows, preLocateRefusalRows) {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := parseCrew(tc.args)
			if !ok || parsed.sub != tc.args[0] {
				t.Fatalf("crew %q did not select its subcommand (ok %v, sub %q)", tc.args, ok, parsed.sub)
			}
			if parsed.refusal == nil {
				t.Fatalf("crew %q parsed clean, want %q", tc.args, tc.msg)
			}
			if parsed.refusal.msg != tc.msg || parsed.refusal.code != tc.code {
				t.Errorf("crew %q refused %q (%d), want %q (%d)", tc.args, parsed.refusal.msg, parsed.refusal.code, tc.msg, tc.code)
			}
		})
	}
}

// TestKongErrorClassification pins the two kong error prefixes kongRefusal maps
// by text, so a kong bump that rewords them fails here rather than turning an
// unknown flag into the generic fallback line.
func TestKongErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		prefix string
	}{
		{[]string{"where", "--bogus"}, "unknown flag --bogus"},
		{[]string{"where", "-x"}, "unknown flag -x"},
		{[]string{"where", "a", "b"}, "unexpected argument b"},
		{[]string{"deregister", "x"}, "unexpected argument x"},
		{[]string{"crews", "--mine=nope"}, "--mine: bool value must be"},
		{[]string{"roster", "--bogus"}, "unknown flag --bogus"},
		{[]string{"roster", "a", "b"}, "unexpected argument b"},
		{[]string{"rate", "--root"}, "--root: missing value"},
		{[]string{"roster-render", "--once=nope"}, "--once: bool value must be"},
		{[]string{"status", "w1", "blocked", "--bogus"}, "unknown flag --bogus"},
		{[]string{"status", "w1", "blocked", "-x"}, "unknown flag -x"},
		{[]string{"status", "w1", "blocked", "a", "b", "c"}, "unexpected argument c"},
		{[]string{"status", "--restamp=nope"}, "--restamp: bool value must be"},
		{[]string{"reply", "--bogus", "a", "b"}, "unknown flag --bogus"},
		{[]string{"reply", "a", "-5"}, "unknown flag -5"},
		{[]string{"reply", "a", "b", "c"}, "unexpected argument c"},
		{[]string{"hold", "--bogus"}, "unknown flag --bogus"},
		{[]string{"hold", "bogus"}, "unexpected argument bogus"},
		{[]string{"hold", "list", "--bogus"}, "unknown flag --bogus"},
		{[]string{"hold", "list", "x"}, "unexpected argument x"},
		{[]string{"hold", "list", "--json=nope"}, "--json: bool value must be"},
		{[]string{"hold", "park", "1", "2"}, "unexpected argument 2"},
		{[]string{"hold", "release", "a", "b"}, "unexpected argument b"},
		{[]string{"hold", "add", "-x"}, "unknown flag -x"},
		{[]string{"hold", "add", "--draft=nope"}, "--draft: bool value must be"},
		{[]string{"nudge", "--bogus"}, "unknown flag --bogus"},
		{[]string{"nudge", "a", "b"}, "unexpected argument b"},
		{[]string{"await", "--bogus"}, "unknown flag --bogus"},
		{[]string{"await", "a", "b"}, "unexpected argument b"},
		{[]string{"watch", "--bogus"}, "unknown flag --bogus"},
		{[]string{"watch", "x"}, "unexpected argument x"},
	} {
		var (
			r       root
			capture passthroughCmd
		)
		p, err := newCrewParser(&r, &capture)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Parse(tc.args)
		if err == nil || !strings.HasPrefix(err.Error(), tc.prefix) {
			t.Errorf("crew %q: kong said %v, want it to start %q", tc.args, err, tc.prefix)
		}
	}
}

// TestRunConvertedFlagSubcommands runs the nine through run(): the refusals
// before any bus read, and the answers an arm gives once it has typed values.
func TestRunConvertedFlagSubcommands(t *testing.T) {
	log := logRowC1 + "\n" + dispatch + "\n"
	repo, _ := repoWithLog(t, &log, 0o644)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes(), procs: crewsProbes(nil, nil)}

	for _, tc := range slices.Concat(refusalRows, preLocateRefusalRows) {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, e)
			if code != tc.code || stdout.Len() != 0 || stderr.String() != tc.msg+"\n" {
				t.Errorf("code %d stdout %q stderr %q, want %d and %q", code, stdout.String(), stderr.String(), tc.code, tc.msg)
			}
		})
	}

	for _, tc := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
	}{
		{name: "identity hash wins over a branch", args: []string{"identity", "--hash", "x", "a-branch"},
			stdout: `{"name":"ember","color":"orange","tmux":"colour130"}` + "\n"},
		{name: "identity hash equals", args: []string{"identity", "--hash=x"},
			stdout: `{"name":"ember","color":"orange","tmux":"colour130"}` + "\n"},
		{name: "resolve-target a branch", args: []string{"resolve-target", "feat/x", "--crew", "c1"}, stdout: "feat/x\t\t\tc1\n"},
		{name: "resolve-target a crew filter", args: []string{"resolve-target", "--crew=c9", "feat/x"}, code: exitFailure,
			stderr: "crew: resolve-target: no worker matches 'feat/x'\n"},
		{name: "sessions flag after the branch", args: []string{"sessions", "feat/x", "--crew=c9"}, stdout: "[]\n\n"},
		{name: "sessions flag before the branch", args: []string{"sessions", "--crew", "c9", "feat/x"}, stdout: "[]\n\n"},
		{name: "crews mine bool value", args: []string{"crews", "--mine=yes"}},
		{name: "log a crew", args: []string{"log", "c9"}},
		{name: "inbox equals flags", args: []string{"inbox", "worker:feat/x#s1-1", "c1", "--since=0", "--from=dispatcher:c1"}},
		{name: "roster a crew", args: []string{"roster", "c9"}, stdout: "[]\n"},
		{name: "retro json needs report", args: []string{"retro", "--json"}, code: exitFailure, stderr: retroUsage + "\n"},
		{name: "retro report json equals", args: []string{"retro", "--report=true", "--json=true"},
			stdout: "{\n  \"rows\": [],\n  \"tags\": [],\n  \"unknown\": []\n}\n"},
		{name: "rate json needs report", args: []string{"rate", "--json"}, code: exitFailure, stderr: rateUsage + "\n"},
		{name: "rate root needs sweep-all", args: []string{"rate", "--root", "/x"}, code: exitFailure,
			stderr: "crew: rate --root needs --sweep-all\n"},
		{name: "rate sweep-all and report", args: []string{"rate", "--sweep-all", "--report"}, code: exitFailure, stderr: rateUsage + "\n"},
		{name: "stack cut refuses a short oid", args: []string{"stack", "cut", "--record", "feat/x", "abc"}, code: exitFailure,
			stderr: "crew: 'abc' is not a full lowercase hex commit oid\n"},
		{name: "stack cut oid after the child and flag", args: []string{"stack", "cut", "feat/x", "abc", "--record"}, code: exitFailure,
			stderr: "crew: 'abc' is not a full lowercase hex commit oid\n"},
		// roster-render refuses before it touches the bus, a lock or tmux.
		{name: "roster-render interval zero", args: []string{"roster-render", "--crew", "c1", "--interval", "0"}, code: exitUsage,
			stderr: "crew: --interval must be a positive integer number of seconds\n"},
		{name: "roster-render interval negative", args: []string{"roster-render", "--crew", "c1", "--interval", "-1"}, code: exitUsage,
			stderr: "crew: --interval must be a positive integer number of seconds\n"},
		{name: "roster-render no crew", args: []string{"roster-render"}, code: exitUsage,
			stderr: "crew: roster-render: --crew is required\n"},
		{name: "roster-render once and detach", args: []string{"roster-render", "--crew=c1", "--once", "--detach=true"}, code: exitUsage,
			stderr: "crew: roster-render: --once and --detach are mutually exclusive\n"},
		{name: "roster-render dash-led crew value", args: []string{"roster-render", "--crew", "--once"}, code: exitUsage,
			stderr: "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n"},
		{name: "inbox since not an integer", args: []string{"inbox", "worker:feat/x#s1-1", "--since=abc"}, code: exitFailure,
			stderr: "crew: --since must be an integer ms timestamp\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, e)
			if code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
				t.Errorf("code %d stdout %q stderr %q, want %d %q %q", code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
			}
		})
	}
}

// TestRunRosterAndReportFallBackToTheAmbientCrew pins the empty-crew fallback
// the typed Crew keeps: no word, and an empty one, both read this repo's crew.
func TestRunRosterAndReportFallBackToTheAmbientCrew(t *testing.T) {
	log := logRowC1 + "\n" + dispatch + "\n"
	repo, _ := repoWithLog(t, &log, 0o644)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes(), procs: crewsProbes(nil, nil)}
	for _, tc := range []struct {
		args []string
		want string // a row only c1's bus holds
	}{
		{[]string{"roster"}, "feat/x"}, {[]string{"roster", ""}, "feat/x"}, {[]string{"roster", "c1"}, "feat/x"},
		{[]string{"report"}, "claude\tsonnet"}, {[]string{"report", ""}, "claude\tsonnet"}, {[]string{"report", "c1"}, "claude\tsonnet"},
	} {
		args := tc.args
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr, e); code != 0 || !strings.Contains(stdout.String(), tc.want) || stderr.Len() != 0 {
			t.Errorf("crew %q: code %d stdout %q stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
}

// TestFlagSpellingsAreIdentical is the decision of #862: `--flag value` and
// `--flag=value` are one command, whatever the subcommand answers.
func TestFlagSpellingsAreIdentical(t *testing.T) {
	const me = "worker:feat/x#s1-1"
	log := logRowC1 + "\n" + dispatch + "\n"
	repo, _ := repoWithLog(t, &log, 0o644)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes(), procs: crewsProbes(nil, nil)}

	// value flags: the spaced form, then the same words with the flag joined.
	for _, tc := range []struct {
		name         string
		before, flag []string
		value        string
		after        []string
		refused      bool
	}{
		{name: "where --crew", before: []string{"where"}, value: "c1", after: []string{"nova"}},
		{name: "where --crew empty", before: []string{"where"}, value: "", after: []string{"nova"}, refused: true},
		{name: "resolve-target --crew", before: []string{"resolve-target"}, value: "c1", after: []string{"feat/x"}},
		{name: "resolve-target --crew empty", before: []string{"resolve-target"}, value: "", after: []string{"feat/x"}, refused: true},
		{name: "sessions --crew", before: []string{"sessions", "feat/x"}, value: "c1"},
		{name: "sessions --crew empty", before: []string{"sessions", "feat/x"}, value: "", refused: true},
		{name: "identity --hash", before: []string{"identity"}, value: "reviewer"},
		{name: "identity --hash empty", before: []string{"identity"}, value: "", refused: true},
		{name: "inbox --since", before: []string{"inbox", me, "c1"}, value: "0"},
		{name: "inbox --since bad", before: []string{"inbox", me, "c1"}, value: "abc"},
		{name: "inbox --since empty", before: []string{"inbox", me, "c1"}, value: "", refused: true},
		{name: "inbox --from", before: []string{"inbox", me, "c1"}, value: "dispatcher:c1"},
		{name: "inbox --from empty", before: []string{"inbox", me, "c1"}, value: "", refused: true},
	} {
		flag := tc.name[strings.LastIndex(tc.name, "--"):]
		flag, _, _ = strings.Cut(flag, " ")
		t.Run(tc.name, func(t *testing.T) {
			spaced := append(append(slices.Clone(tc.before), flag, tc.value), tc.after...)
			joined := append(append(slices.Clone(tc.before), flag+"="+tc.value), tc.after...)
			assertSameRun(t, e, spaced, joined, tc.refused)
		})
	}

	// bool flags take no separate value: the bare flag and `=true` are one.
	for _, tc := range []struct {
		name         string
		bare, joined []string
	}{
		{"crews --mine", []string{"crews", "--mine"}, []string{"crews", "--mine=true"}},
		{"inbox --undelivered", []string{"inbox", me, "c1", "--undelivered"}, []string{"inbox", me, "c1", "--undelivered=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSameRun(t, e, tc.bare, tc.joined, false) })
	}

	// The rest of the converted subcommands, where running both spellings is safe:
	// roster-render only through argv it refuses, rate and retro through reads.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, tc := range []struct {
		name         string
		bare, joined []string
		refused      bool
	}{
		{"retro --report", []string{"retro", "--report"}, []string{"retro", "--report=true"}, false},
		{"retro --report --json", []string{"retro", "--report", "--json"}, []string{"retro", "--report=true", "--json=true"}, false},
		{"rate --report", []string{"rate", "--report"}, []string{"rate", "--report=true"}, false},
		{"rate --report --json --pooled", []string{"rate", "--report", "--json", "--pooled"},
			[]string{"rate", "--report=true", "--json=true", "--pooled=true"}, false},
		{"rate --root", []string{"rate", "--root", "/x"}, []string{"rate", "--root=/x"}, true},
		{"rate --root empty", []string{"rate", "--root", ""}, []string{"rate", "--root="}, true},
		{"roster-render --crew empty", []string{"roster-render", "--crew", ""}, []string{"roster-render", "--crew="}, true},
		{"roster-render --pane empty", []string{"roster-render", "--pane", ""}, []string{"roster-render", "--pane="}, true},
		{"roster-render --interval", []string{"roster-render", "--crew", "c1", "--interval", "0"},
			[]string{"roster-render", "--crew=c1", "--interval=0"}, true},
		{"roster-render --quiet", []string{"roster-render", "--crew", "c1", "--quiet", "x"},
			[]string{"roster-render", "--crew=c1", "--quiet=x"}, true},
		{"roster-render --once --detach", []string{"roster-render", "--crew", "c1", "--once", "--detach"},
			[]string{"roster-render", "--crew", "c1", "--once=true", "--detach=true"}, true},
		// refused once the sender check passes: --restamp needs state blocked.
		{"hold list --json --crew", []string{"hold", "list", "--json", "--crew", "c1"}, []string{"hold", "list", "--json=true", "--crew=c1"}, false},
		{"hold due --json --crew", []string{"hold", "due", "--json", "--crew", "c1"}, []string{"hold", "due", "--json=true", "--crew=c1"}, false},
		{"hold park --crew", []string{"hold", "park", "60", "--crew", "c1"}, []string{"hold", "park", "60", "--crew=c1"}, false},
		{"hold list --crew empty", []string{"hold", "list", "--crew", ""}, []string{"hold", "list", "--crew="}, true},
		{"hold due --crew empty", []string{"hold", "due", "--crew", ""}, []string{"hold", "due", "--crew="}, true},
		{"hold park --crew empty", []string{"hold", "park", "60", "--crew", ""}, []string{"hold", "park", "60", "--crew="}, true},
		{"hold release --crew empty", []string{"hold", "release", "h1", "--crew", ""}, []string{"hold", "release", "h1", "--crew="}, true},
		{"reply --crew empty", []string{"reply", "--crew", "", "worker:feat/x", "go"}, []string{"reply", "--crew=", "worker:feat/x", "go"}, true},
		{"reply --crew", []string{"reply", "--crew", "c9", "role:a", "go"}, []string{"reply", "--crew=c9", "role:a", "go"}, true},
		{"nudge --crew empty", []string{"nudge", "--crew", "", "nova"}, []string{"nudge", "--crew=", "nova"}, true},
		{"nudge --wait word", []string{"nudge", "nova", "--wait", "abc"}, []string{"nudge", "nova", "--wait=abc"}, true},
		{"await --from empty", []string{"await", me, "--from", ""}, []string{"await", me, "--from="}, true},
		{"await --timeout empty", []string{"await", me, "--timeout", ""}, []string{"await", me, "--timeout="}, true},
		{"await --interval empty", []string{"await", me, "--interval", ""}, []string{"await", me, "--interval="}, true},
		{"await --timeout bad", []string{"await", me, "--timeout", "abc"}, []string{"await", me, "--timeout=abc"}, true},
		{"watch --since empty", []string{"watch", "--since", ""}, []string{"watch", "--since="}, true},
		{"watch --states empty", []string{"watch", "--states", ""}, []string{"watch", "--states="}, true},
		{"watch --timeout empty", []string{"watch", "--timeout", ""}, []string{"watch", "--timeout="}, true},
		{"watch --interval empty", []string{"watch", "--interval", ""}, []string{"watch", "--interval="}, true},
		{"watch --crew empty", []string{"watch", "--crew", ""}, []string{"watch", "--crew="}, true},
		{"watch --timeout zero", []string{"watch", "--timeout", "0"}, []string{"watch", "--timeout=0"}, true},
		{"watch --states only commas", []string{"watch", "--states", ","}, []string{"watch", "--states=,"}, true},
		{"watch --since bad", []string{"watch", "--since", "x"}, []string{"watch", "--since=x"}, true},
		{"status --restamp", []string{"status", me, "working", "--restamp"}, []string{"status", me, "working", "--restamp=true"}, true},
		{"stack cut --record", []string{"stack", "cut", "--record", "feat/x", "abc"},
			[]string{"stack", "cut", "--record=true", "feat/x", "abc"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSameRun(t, e, tc.bare, tc.joined, tc.refused) })
	}

	// fleet runs outside any repo; only its refusals are run, since its table
	// carries ages.
	for _, tc := range []struct {
		name         string
		bare, joined []string
	}{
		{"fleet --crew empty", []string{"fleet", "--crew", ""}, []string{"fleet", "--crew="}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSameRun(t, e, tc.bare, tc.joined, true) })
	}
}

// TestParseSpellingsAreIdentical is the same decision at the parse, for every
// converted flag: the spaced and the joined spelling bind the same values and
// earn the same refusal. It reaches the flags a run could not safely drive.
func TestParseSpellingsAreIdentical(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a, b     []string
		refusing bool
	}{
		{"rate --root", []string{"rate", "--sweep-all", "--root", "/x"}, []string{"rate", "--sweep-all", "--root=/x"}, false},
		{"rate --root repeated", []string{"rate", "--sweep-all", "--root", "/x", "--root", "/y"},
			[]string{"rate", "--sweep-all", "--root=/x", "--root=/y"}, false},
		{"rate --root first word", []string{"rate", "--root", "/x", "--sweep-all"}, []string{"rate", "--root=/x", "--sweep-all"}, false},
		{"rate --root commas", []string{"rate", "--root", "/a,b"}, []string{"rate", "--root=/a,b"}, false},
		{"rate --root empty", []string{"rate", "--root", ""}, []string{"rate", "--root="}, true},
		{"fleet --crew", []string{"fleet", "--crew", "c1"}, []string{"fleet", "--crew=c1"}, false},
		{"fleet --crew empty", []string{"fleet", "--crew", ""}, []string{"fleet", "--crew="}, true},
		{"roster-render --crew", []string{"roster-render", "--crew", "c1"}, []string{"roster-render", "--crew=c1"}, false},
		{"roster-render --pane", []string{"roster-render", "--crew", "c1", "--pane", "%3"}, []string{"roster-render", "--crew=c1", "--pane=%3"}, false},
		{"roster-render --interval", []string{"roster-render", "--crew", "c1", "--interval", "5"},
			[]string{"roster-render", "--crew", "c1", "--interval=5"}, false},
		{"roster-render --interval negative", []string{"roster-render", "--interval", "-1"}, []string{"roster-render", "--interval=-1"}, false},
		{"roster-render --quiet", []string{"roster-render", "--crew", "c1", "--quiet", "0"},
			[]string{"roster-render", "--crew", "c1", "--quiet=0"}, false},
		{"roster-render --quiet empty", []string{"roster-render", "--quiet", ""}, []string{"roster-render", "--quiet="}, true},
		// bool flags: bare and `=true` are one.
		{"retro bools", []string{"retro", "--report", "--json"}, []string{"retro", "--report=true", "--json=true"}, false},
		{"rate bools", []string{"rate", "--report", "--json", "--pooled"}, []string{"rate", "--report=true", "--json=true", "--pooled=true"}, false},
		{"rate --sweep-all", []string{"rate", "--sweep-all"}, []string{"rate", "--sweep-all=true"}, false},
		{"fleet bools", []string{"fleet", "--all", "--json"}, []string{"fleet", "--all=true", "--json=true"}, false},
		{"stack cut --record", []string{"stack", "cut", "--record", "feat/x"}, []string{"stack", "cut", "--record=true", "feat/x"}, false},
		{"status --restamp", []string{"status", "w1", "blocked", "why", "--restamp"}, []string{"status", "w1", "blocked", "why", "--restamp=true"}, false},
		{"status --restamp first", []string{"status", "--restamp", "w1", "blocked"}, []string{"status", "--restamp=true", "w1", "blocked"}, false},
		{"reply --crew", []string{"reply", "--crew", "c1", "worker:x", "go"}, []string{"reply", "--crew=c1", "worker:x", "go"}, false},
		{"reply --crew after body", []string{"reply", "worker:x", "go", "--crew", "c1"}, []string{"reply", "worker:x", "go", "--crew=c1"}, false},
		{"hold list flags", []string{"hold", "list", "--json", "--crew", "c1"}, []string{"hold", "list", "--json=true", "--crew=c1"}, false},
		{"hold due flags", []string{"hold", "due", "--json", "--crew", "c1"}, []string{"hold", "due", "--json=true", "--crew=c1"}, false},
		{"hold park --crew", []string{"hold", "park", "60", "--crew", "c1"}, []string{"hold", "park", "60", "--crew=c1"}, false},
		{"hold park --crew first", []string{"hold", "park", "--crew", "c1", "60"}, []string{"hold", "park", "--crew=c1", "60"}, false},
		{"hold release --crew", []string{"hold", "release", "h1", "--crew", "c1"}, []string{"hold", "release", "h1", "--crew=c1"}, false},
		{"hold add --draft", []string{"hold", "add", "--draft", "t"}, []string{"hold", "add", "--draft=true", "t"}, false},
		{"hold add all value flags", holdAddSpaced("v"), holdAddJoined("v"), false},
		{"hold add all value flags empty", holdAddSpaced(""), holdAddJoined(""), true},
		{"nudge --crew", []string{"nudge", "--crew", "c1", "nova"}, []string{"nudge", "--crew=c1", "nova"}, false},
		{"nudge --crew after target", []string{"nudge", "nova", "--crew", "c1"}, []string{"nudge", "nova", "--crew=c1"}, false},
		{"nudge --crew empty", []string{"nudge", "--crew", "", "nova"}, []string{"nudge", "--crew=", "nova"}, true},
		{"nudge --wait", []string{"nudge", "nova", "--wait", "5"}, []string{"nudge", "nova", "--wait=5"}, false},
		{"nudge --wait first", []string{"nudge", "--wait", "5", "nova"}, []string{"nudge", "--wait=5", "nova"}, false},
		{"nudge --wait word", []string{"nudge", "--wait", "nova"}, []string{"nudge", "--wait=nova"}, false},
		{"nudge --wait word and target", []string{"nudge", "nova", "--wait", "abc"}, []string{"nudge", "nova", "--wait=abc"}, true},
		{"await --from", []string{"await", "worker:x#s1-1", "--from", "dispatcher:c1"}, []string{"await", "worker:x#s1-1", "--from=dispatcher:c1"}, false},
		{"await --timeout", []string{"await", "worker:x#s1-1", "--timeout", "5"}, []string{"await", "worker:x#s1-1", "--timeout=5"}, false},
		{"await --interval", []string{"await", "worker:x#s1-1", "--interval", "3"}, []string{"await", "worker:x#s1-1", "--interval=3"}, false},
		{"await flags first", []string{"await", "--from", "d", "--timeout", "5", "--interval", "3", "worker:x#s1-1"},
			[]string{"await", "--from=d", "--timeout=5", "--interval=3", "worker:x#s1-1"}, false},
		{"await --timeout empty", []string{"await", "worker:x#s1-1", "--timeout", ""}, []string{"await", "worker:x#s1-1", "--timeout="}, true},
		{"watch --since", []string{"watch", "--since", "5"}, []string{"watch", "--since=5"}, false},
		{"watch --states", []string{"watch", "--states", "blocked,done"}, []string{"watch", "--states=blocked,done"}, false},
		{"watch --timeout", []string{"watch", "--timeout", "30"}, []string{"watch", "--timeout=30"}, false},
		{"watch --interval", []string{"watch", "--interval", "1"}, []string{"watch", "--interval=1"}, false},
		{"watch --crew", []string{"watch", "--crew", "c1"}, []string{"watch", "--crew=c1"}, false},
		{"watch all flags", []string{"watch", "--since", "5", "--states", "done", "--timeout", "30", "--interval", "1", "--crew", "c1"},
			[]string{"watch", "--since=5", "--states=done", "--timeout=30", "--interval=1", "--crew=c1"}, false},
		{"watch --since empty", []string{"watch", "--since", ""}, []string{"watch", "--since="}, true},
		{"roster-render bools", []string{"roster-render", "--no-open", "--once", "--detach"},
			[]string{"roster-render", "--no-open=true", "--once=true", "--detach=true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, okA := parseCrew(tc.a)
			b, okB := parseCrew(tc.b)
			if !okA || !okB {
				t.Fatalf("crew %q or %q selected no subcommand", tc.a, tc.b)
			}
			if !reflect.DeepEqual(a.typed, b.typed) || !reflect.DeepEqual(a.refusal, b.refusal) {
				t.Errorf("crew %q and crew %q parse differently:\n %+v %v\n %+v %v", tc.a, tc.b, a.typed, a.refusal, b.typed, b.refusal)
			}
			if (a.refusal != nil) != tc.refusing {
				t.Errorf("crew %q refusal = %v, want refused %v", tc.a, a.refusal, tc.refusing)
			}
		})
	}
}

// holdFlags are `hold add`'s fourteen value flags.
var holdFlags = []string{"engine", "window", "resets-at", "agent", "ref", "branch", "tier", "model",
	"effort", "plan", "mcp", "shape", "spec", "crew"}

func holdAddSpaced(v string) []string {
	args := []string{"hold", "add"}
	for _, f := range holdFlags {
		args = append(args, "--"+f, v)
	}
	return append(args, "t")
}

func holdAddJoined(v string) []string {
	args := []string{"hold", "add"}
	for _, f := range holdFlags {
		args = append(args, "--"+f+"="+v)
	}
	return append(args, "t")
}

// TestHoldAddEachValueFlagSpellsTheSame: one flag at a time, so a flag the table
// loses or mis-binds is named by the row, not hidden behind the other thirteen.
func TestHoldAddEachValueFlagSpellsTheSame(t *testing.T) {
	for _, f := range holdFlags {
		for _, v := range []string{"v", ""} {
			t.Run(f+"/"+v, func(t *testing.T) {
				a, okA := parseCrew([]string{"hold", "add", "--" + f, v, "t"})
				b, okB := parseCrew([]string{"hold", "add", "--" + f + "=" + v, "t"})
				if !okA || !okB {
					t.Fatal("hold add selected no subcommand")
				}
				if !reflect.DeepEqual(a.typed, b.typed) || !reflect.DeepEqual(a.refusal, b.refusal) {
					t.Errorf("spellings differ:\n %+v %v\n %+v %v", a.typed, a.refusal, b.typed, b.refusal)
				}
				if wantRefused := v == ""; (a.refusal != nil) != wantRefused {
					t.Errorf("refusal = %v, want refused %v", a.refusal, wantRefused)
				}
			})
		}
	}
}

// TestHoldAddTitleIsVerbatimAfterTheFirstWord pins add's grammar: flags up to
// the first title word, then every word is title text.
func TestHoldAddTitleIsVerbatimAfterTheFirstWord(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		draft bool
		title []string
	}{
		{[]string{"hold", "add", "my", "--draft", "title"}, false, []string{"my", "--draft", "title"}},
		{[]string{"hold", "add", "--draft", "my", "--engine", "x"}, true, []string{"my", "--engine", "x"}},
		{[]string{"hold", "add", "a", "-x"}, false, []string{"a", "-x"}},
	} {
		parsed, ok := parseCrew(tc.args)
		got := parsed.typed.Hold.Add
		if !ok || parsed.refusal != nil || got.Draft != tc.draft || !slices.Equal(got.Title, tc.title) || got.Engine != nil {
			t.Errorf("crew %q: ok %v refusal %v draft %v title %q", tc.args, ok, parsed.refusal, got.Draft, got.Title)
		}
	}
}

func assertSameRun(t *testing.T, e env, a, b []string, refused bool) {
	t.Helper()
	var out, errOut [2]bytes.Buffer
	codes := [2]int{
		run(context.Background(), a, &out[0], &errOut[0], e),
		run(context.Background(), b, &out[1], &errOut[1], e),
	}
	if codes[0] != codes[1] || out[0].String() != out[1].String() || errOut[0].String() != errOut[1].String() {
		t.Errorf("crew %q and crew %q differ:\n %d %q %q\n %d %q %q",
			a, b, codes[0], out[0].String(), errOut[0].String(), codes[1], out[1].String(), errOut[1].String())
	}
	if refused && codes[0] == 0 {
		t.Errorf("crew %q succeeded, want a refusal", a)
	}
}

// TestMsgWordsAreVerbatim pins msg's grammar: every word is data, flag-shaped
// ones included, and only a leading `--a=b` is split by kong.
func TestMsgWordsAreVerbatim(t *testing.T) {
	for _, tc := range []struct{ args, words []string }{
		{[]string{"msg", "a", "b", "-1"}, []string{"a", "b", "-1"}},
		{[]string{"msg", "a", "b", "--help"}, []string{"a", "b", "--help"}},
		{[]string{"msg", "a", "b", "x=y", "extra"}, []string{"a", "b", "x=y", "extra"}},
		{[]string{"msg", "a", "b", "--restamp=true"}, []string{"a", "b", "--restamp=true"}},
		{[]string{"msg", "--a=b", "to", "body"}, []string{"--a", "b", "to", "body"}},
	} {
		parsed, ok := parseCrew(tc.args)
		if !ok || parsed.refusal != nil || !slices.Equal(parsed.typed.Msg.Args, tc.words) {
			t.Errorf("crew %q: ok %v refusal %v words %q, want %q", tc.args, ok, parsed.refusal, parsed.typed.Msg.Args, tc.words)
		}
	}
}

// TestRunSenderCheckSurvivesTyping: a caller that is not the dispatcher cannot
// post as dispatcher:c1, however the grammar spells the sender or the crew.
func TestRunSenderCheckSurvivesTyping(t *testing.T) {
	const worker = "worker:feat/x#s1-1"
	log := logRowC1 + "\n" + dispatch + "\n"
	repo, logPath := repoWithLog(t, &log, 0o644)
	t.Setenv("CREW_ID", "c1")
	t.Setenv("CREW_WORKER_ID", worker)
	t.Setenv("TMUX_PANE", "")
	if err := os.Unsetenv("TMUX_PANE"); err != nil {
		t.Fatal(err)
	}
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes(), procs: crewsProbes(nil, nil)}
	resets := strconv.FormatInt(time.Now().Unix()+3600, 10)
	add := func(crew ...string) []string {
		return append(append([]string{"hold", "add"}, crew...),
			"--engine", "claude", "--window", "w", "--resets-at", resets, "--agent", "a", "--ref", "r",
			"--branch", "feat/x", "--tier", "standard", "--model", "m", "--effort", "medium", "a", "title")
	}
	for _, tc := range []struct {
		name string
		sub  string
		args []string
	}{
		{"msg", "msg", []string{"msg", "dispatcher:c1", "x", "y"}},
		{"msg from a worker env", "msg", []string{"msg", "dispatcher:c1", worker, "hi"}},
		{"reply crew equals", "reply", []string{"reply", worker, "go", "--crew=c1"}},
		{"reply crew spaced", "reply", []string{"reply", "--crew", "c1", worker, "go"}},
		{"reply crew after body", "reply", []string{"reply", worker, "go", "--crew", "c1"}},
		{"hold add crew equals", "hold add", add("--crew=c1")},
		{"hold add crew spaced", "hold add", add("--crew", "c1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, e)
			want := "crew: " + tc.sub + ": refusing sender 'dispatcher:c1' — "
			if code != exitFailure || !strings.HasPrefix(stderr.String(), want) {
				t.Fatalf("code %d stderr %q, want exit 1 and prefix %q", code, stderr.String(), want)
			}
			after, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("the log changed: %q", after)
			}
		})
	}
}

// TestStreamChildWatchArgv pins the argv `crew stream` re-enters `crew watch`
// with (internal/stream/stream.go park): it must keep parsing clean under the
// typed grammar, to the request it was built from.
func TestStreamChildWatchArgv(t *testing.T) {
	parsed, ok := parseCrew([]string{"watch", "--crew", "c1", "--timeout", "300",
		"--states", "blocked,done", "--interval", "2"})
	if !ok || parsed.refusal != nil {
		t.Fatalf("stream's child argv refused: ok %v refusal %v", ok, parsed.refusal)
	}
	w := parsed.typed.Watch
	if w.Since != nil || str(w.Crew) != "c1" || str(w.Timeout) != "300" ||
		str(w.States) != "blocked,done" || str(w.Interval) != "2" {
		t.Errorf("stream's child argv bound %+v", w)
	}
}

// TestNudgeWaitBinding pins what the typed Wait and Target make of the
// positions `--wait` can take.
func TestNudgeWaitBinding(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want nudge.Request
	}{
		{[]string{"nudge", "nova"}, nudge.Request{Target: "nova"}},
		{[]string{"nudge", "nova", "--wait"}, nudge.Request{Target: "nova", Wait: "1800"}},
		{[]string{"nudge", "nova", "--wait", "5"}, nudge.Request{Target: "nova", Wait: "5"}},
		{[]string{"nudge", "--wait=5", "nova", "--crew=c1"}, nudge.Request{Crew: "c1", Target: "nova", Wait: "5"}},
		{[]string{"nudge", "--wait", "nova"}, nudge.Request{Target: "nova", Wait: "1800"}},
	} {
		parsed, ok := parseCrew(tc.args)
		if !ok || parsed.refusal != nil {
			t.Errorf("crew %q refused: %v", tc.args, parsed.refusal)
			continue
		}
		n := parsed.typed.Nudge
		got, _ := nudge.NewRequest(str(n.Crew), n.Target, n.Wait)
		if got != tc.want {
			t.Errorf("crew %q = %+v, want %+v", tc.args, got, tc.want)
		}
	}
}
