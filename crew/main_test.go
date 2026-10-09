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

	t.Run("an unknown subcommand is still the usage exit", func(t *testing.T) {
		repo, _ := repoWithLog(t, nil, 0)
		var stdout, stderr bytes.Buffer
		e := env{getwd: func() (string, error) { return repo, nil }}
		if code := run(context.Background(), []string{"logs"}, &stdout, &stderr, e); code != exitUsage {
			t.Fatalf("code %d, want %d", code, exitUsage)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "crew-go: usage") {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
	})
}

// The arm has two halves: the msgs it prints, and the delivered-marks file it
// leaves for `crew await` and the bash `_unread_scan` to read. Both are wired
// through bus.Paths, so the marks land under the repo's crew dir next to the bus.
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
