package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

const (
	crewsTableHeader = "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n"
	invalidIDMsg     = "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n"
	noCrewIDMsg      = "crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one\n"
	noCrewRegMsg     = "crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one\n"
	crewNewHelperEnv = "CREW_NEW_HELPER"
	ghStub           = `#!/bin/sh
printf '%s\n' "$*" >>"$GH_LOG"
case "$*" in
"pr list "*) [ -z "${PR_OPEN_BRANCH:-}" ] || printf '%s\n' "$PR_OPEN_BRANCH" ;;
esac
exit 0
`
	tmuxStub = `#!/bin/sh
printf '%s\n' "$*" >>"$TMUX_LOG"
case "$1 $2" in
"list-windows -a") [ -z "${TMUX_WINDOWS:-}" ] || printf '%s\n' "$TMUX_WINDOWS" ;;
esac
exit 0
`
)

// crewsCLI drives run() in the process, with gh and tmux stubbed on PATH and
// the process cwd in the repo, where adopt and reap run git.
type crewsCLI struct {
	*holdCLI
	proc  proc.Table
	ghLog string
}

func unsetenv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newCrewsCLI(t *testing.T) *crewsCLI {
	t.Helper()
	h := newHoldCLI(t)
	// reap would otherwise exec os.Executable, the test binary.
	t.Setenv("CREW_RATE_AUTOSWEEP", "0")
	unsetenv(t, "TMUX", "TMUX_PANE", "PR_OPEN_BRANCH", "TMUX_WINDOWS")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Chdir(h.repo)

	stubs := t.TempDir()
	c := &crewsCLI{
		holdCLI: h,
		proc:    proc.OS{},
		ghLog:   filepath.Join(stubs, "gh.log"),
	}
	t.Setenv("GH_LOG", c.ghLog)
	t.Setenv("TMUX_LOG", filepath.Join(stubs, "tmux.log"))
	writeStub(t, stubs, "gh", ghStub)
	writeStub(t, stubs, "tmux", tmuxStub)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	return c
}

func (c *crewsCLI) run(args ...string) (string, string, int) {
	c.t.Helper()
	e := env{getwd: func() (string, error) { return c.repo, nil }, proc: c.proc}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

func (c *crewsCLI) mustRun(args ...string) string {
	c.t.Helper()
	out, errOut, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
	}
	return out
}

// expect runs args and requires the exit code, returning stdout and stderr.
func (c *crewsCLI) expect(want int, args ...string) (string, string) {
	c.t.Helper()
	out, errOut, code := c.run(args...)
	if code != want {
		c.t.Fatalf("%v: code %d, want %d (stdout %q, stderr %q)", args, code, want, out, errOut)
	}
	return out, errOut
}

// as sets the caller's CREW_ID; "" leaves it unset.
func (c *crewsCLI) as(crew string) {
	c.t.Helper()
	unsetenv(c.t, "CREW_ID")
	if crew != "" {
		c.t.Setenv("CREW_ID", crew)
	}
}

func (c *crewsCLI) busDir() string { return filepath.Join(c.repo, ".git", "crew") }

func (c *crewsCLI) crewDir(id string) string { return filepath.Join(c.busDir(), "crews", id) }

func (c *crewsCLI) pidOf(id string) string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.crewDir(id), "pid"))
	if err != nil {
		c.t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func (c *crewsCLI) pidfileLog() string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.busDir(), "pidfile.log"))
	if err != nil {
		c.t.Fatal(err)
	}
	return string(data)
}

// stubLog is a stub's recorded argv lines; "" when the stub never ran.
func stubLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (c *crewsCLI) registerAs(crew, pid string) {
	c.t.Helper()
	c.as(crew)
	c.mustRun("register", pid)
}

func (c *crewsCLI) appendLog(s string) {
	c.t.Helper()
	if err := os.MkdirAll(c.busDir(), 0o755); err != nil {
		c.t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(c.busDir(), "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		c.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		c.t.Fatal(err)
	}
}

func (c *crewsCLI) seedRow(row map[string]any) {
	c.t.Helper()
	line, err := json.Marshal(row)
	if err != nil {
		c.t.Fatal(err)
	}
	c.appendLog(string(line) + "\n")
}

// seedEvent appends a status row carrying crew straight to the log, the way a
// hostile id arrives through `dispatch --crew-id`.
func (c *crewsCLI) seedEvent(crew string) {
	c.t.Helper()
	c.seedRow(map[string]any{"ts": time.Now().UnixMilli(), "kind": "status", "crew_id": crew, "from": "worker:feat/x#s1", "state": "working"})
}

// seedClaim's issue is any so a numeric issue stays a JSON number.
func (c *crewsCLI) seedClaim(crew string, issue any, branch string, ts int) {
	c.t.Helper()
	c.seedRow(map[string]any{"ts": ts, "crew_id": crew, "kind": "claim-issue", "issue": issue, "branch": branch})
}

func (c *crewsCLI) git(args ...string) {
	c.t.Helper()
	full := append([]string{"-C", c.repo, "-c", "user.email=t@e", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		c.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// occupy puts a sage window on a worktree of branch, as tmux reports it.
func (c *crewsCLI) occupy(branch string) {
	c.t.Helper()
	wt := filepath.Join(c.repo, ".worktrees", strings.ReplaceAll(branch, "/", "-"))
	c.git("commit", "--allow-empty", "-qm", "init")
	c.git("branch", branch)
	c.git("worktree", "add", "-q", wt, branch)
	c.t.Setenv("TMUX_WINDOWS", "@1\tsage\t"+wt)
}

func livePid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// crewsRow is the table row whose first field is id, or nil.
func crewsRow(out, id string) []string {
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Split(line, "\t"); f[0] == id {
			return f
		}
	}
	return nil
}

func wantRow(t *testing.T, out, id string) []string {
	t.Helper()
	row := crewsRow(out, id)
	if len(row) != 6 {
		t.Fatalf("row for %s = %q in %q", id, row, out)
	}
	return row
}

func eq(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func contains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s = %q, want it to contain %q", what, got, w)
		}
	}
}

func lacks(t *testing.T, what, got string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(got, n) {
			t.Errorf("%s = %q, want it not to contain %q", what, got, n)
		}
	}
}

func TestCrewsTable(t *testing.T) {
	t.Run("an events-only crew (never registered) appears with no pid", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("c-events")
		c.mustRun("status", "worker:feat/x#s1", "working")
		row := wantRow(t, c.mustRun("crews"), "c-events")
		eq(t, "workers", row[3], "1")
		eq(t, "pid", row[4], "—")
		eq(t, "alive", row[5], "—")
	})

	t.Run("a crew-dir-only crew (no events) has zero workers and no timestamps", func(t *testing.T) {
		c := newCrewsCLI(t)
		dead := strconv.Itoa(deadPid(t))
		c.registerAs("c-dir", dead)
		row := wantRow(t, c.mustRun("crews"), "c-dir")
		eq(t, "last_event_s", row[1], "—")
		eq(t, "first_event_s", row[2], "—")
		eq(t, "workers", row[3], "0")
		eq(t, "pid", row[4], dead)
		eq(t, "alive", row[5], "no")
	})

	t.Run("a crew present in both sources appears exactly once", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-both", strconv.Itoa(os.Getppid()))
		c.mustRun("status", "worker:feat/x#s1", "working")
		n := 0
		for line := range strings.SplitSeq(c.mustRun("crews"), "\n") {
			if strings.HasPrefix(line, "c-both\t") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("c-both rows = %d, want 1", n)
		}
	})

	t.Run("workers counts distinct worker branches under one crew", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("c-workers")
		c.mustRun("status", "worker:feat/a#s1", "working")
		c.mustRun("status", "worker:feat/b#s1", "working")
		c.mustRun("status", "worker:feat/b#s1", "done")
		eq(t, "workers", wantRow(t, c.mustRun("crews"), "c-workers")[3], "2")
	})

	t.Run("a live registered pid reads alive yes", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		row := wantRow(t, c.mustRun("crews"), "c-live")
		eq(t, "pid", row[4], pid)
		eq(t, "alive", row[5], "yes")
	})

	t.Run("a seeded crew-id-less reap event adds no phantom row", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.seedRow(map[string]any{"ts": time.Now().UnixMilli(), "kind": "reap", "branch": "feat/x", "pr": "", "pr_state": "", "worktree": ""})
		eq(t, "stdout", c.mustRun("crews"), crewsTableHeader)
	})

	t.Run("register then deregister leaves header-only output, no '*' row", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c1", strconv.Itoa(os.Getppid()))
		c.mustRun("deregister")
		eq(t, "stdout", c.mustRun("crews"), crewsTableHeader)
	})

	t.Run("a pid file of 0 reads as not alive", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-zero", "0")
		row := wantRow(t, c.mustRun("crews"), "c-zero")
		eq(t, "pid", row[4], "0")
		eq(t, "alive", row[5], "no")
	})

	t.Run("a torn trailing line still lists the well-formed crews", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("c-intact")
		c.mustRun("status", "worker:feat/x#s1", "working")
		c.appendLog(`{"ts":1785951264000,"crew_id":"c-to`)
		wantRow(t, c.mustRun("crews"), "c-intact")
	})

	t.Run("no crew dir at all still exits 0 with just the header", func(t *testing.T) {
		c := newCrewsCLI(t)
		eq(t, "stdout", c.mustRun("crews"), crewsTableHeader)
	})

	t.Run("2500 event-carrying crews survive the old --argjson stats argv cliff (#838)", func(t *testing.T) {
		c := newCrewsCLI(t)
		var log strings.Builder
		for i := 1; i <= 2500; i++ {
			fmt.Fprintf(&log, `{"ts":1785951264000,"kind":"status","crew_id":"c%d","from":"worker:feat/x#s1","state":"working"}`+"\n", i)
		}
		c.appendLog(log.String())
		out := c.mustRun("crews")
		if n := strings.Count(out, "\n"); n != 2501 {
			t.Errorf("lines = %d, want 2501 (header + one row per crew)", n)
		}
		row := wantRow(t, out, "c1234")
		eq(t, "workers", row[3], "1")
		eq(t, "pid", row[4], "—")
	})
}

func TestCrewsMine(t *testing.T) {
	t.Run("lists the crew whose pid is an ancestor of the caller", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-mine", strconv.Itoa(os.Getppid()))
		eq(t, "stdout", c.mustRun("crews", "--mine"), "c-mine\n")
	})

	t.Run("omits a crew owned by a live child and one with a dead pid", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-mine", strconv.Itoa(os.Getppid()))
		c.registerAs("c-child", strconv.Itoa(livePid(t)))
		c.registerAs("c-dead", strconv.Itoa(deadPid(t)))
		eq(t, "stdout", c.mustRun("crews", "--mine"), "c-mine\n")
	})

	t.Run("a crew with no pid file and a crew with only log traffic are never ours", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("c-events")
		c.mustRun("status", "worker:feat/x#s1", "working")
		if err := os.MkdirAll(c.crewDir("c-nopid"), 0o755); err != nil {
			t.Fatal(err)
		}
		eq(t, "stdout", c.mustRun("crews", "--mine"), "")
	})

	t.Run("no crews at all prints nothing and exits 0", func(t *testing.T) {
		c := newCrewsCLI(t)
		eq(t, "stdout", c.mustRun("crews", "--mine"), "")
	})

	t.Run("an unknown flag is a usage error, not the full table", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c1", strconv.Itoa(os.Getppid()))
		out, errOut := c.expect(64, "crews", "--bogus")
		eq(t, "stdout", out, "")
		eq(t, "stderr", errOut, "crew: crews: unknown arg '--bogus'\n")
	})

	t.Run("--mine=yes is --mine, not the full table", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c1", strconv.Itoa(os.Getppid()))
		mine := c.mustRun("crews", "--mine")
		eq(t, "--mine=yes", c.mustRun("crews", "--mine=yes"), mine)
	})
}

func TestCrewID(t *testing.T) {
	t.Run("two consecutive calls with CREW_ID unset both refuse, no id is minted", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("")
		for range 2 {
			out, errOut := c.expect(1, "id")
			eq(t, "stdout", out, "")
			eq(t, "stderr", errOut, noCrewIDMsg)
		}
	})

	t.Run("the refusal names crew crews, crew adopt, and crew new", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("")
		_, errOut := c.expect(1, "id")
		contains(t, "stderr", errOut, "crew crews", "crew adopt", "crew new")
	})

	t.Run("resolves from WORKER_TASK.md when CREW_ID is unset", func(t *testing.T) {
		c := newCrewsCLI(t)
		if err := os.WriteFile(filepath.Join(c.repo, "WORKER_TASK.md"), []byte("crew_id: 1785381776-999\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.as("")
		eq(t, "stdout", c.mustRun("id"), "1785381776-999\n")
	})
}

var crewNewOut = regexp.MustCompile(`^[0-9]+-[0-9]+\n$`)

// TestCrewNewHelper is the child process TestCrewNew spawns; the mint embeds
// the pid, which in-process is shared by every call.
func TestCrewNewHelper(*testing.T) {
	if os.Getenv(crewNewHelperEnv) == "" {
		return
	}
	os.Exit(run(context.Background(), []string{"new"}, os.Stdout, os.Stderr, env{getwd: os.Getwd}))
}

func TestCrewNew(t *testing.T) {
	t.Run("mints <epoch>-<pid>", func(t *testing.T) {
		c := newCrewsCLI(t)
		if out := c.mustRun("new"); !crewNewOut.MatchString(out) {
			t.Errorf("stdout = %q, want <epoch>-<pid>", out)
		}
	})

	t.Run("two consecutive calls differ", func(t *testing.T) {
		mint := func() string {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrewNewHelper$")
			cmd.Env = append(os.Environ(), crewNewHelperEnv+"=1")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if !crewNewOut.Match(out) {
				t.Fatalf("stdout = %q, want <epoch>-<pid>", out)
			}
			return string(out)
		}
		if first, second := mint(), mint(); first == second {
			t.Errorf("both calls minted %q", first)
		}
	})

	t.Run("works outside a git repo", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"new"}, &stdout, &stderr, env{getwd: func() (string, error) { return "/", nil }})
		if code != 0 || !crewNewOut.MatchString(stdout.String()) {
			t.Errorf("code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
		}
	})
}

func TestAdopt(t *testing.T) {
	ppid := strconv.Itoa(os.Getppid())

	t.Run("refuses an unknown id", func(t *testing.T) {
		c := newCrewsCLI(t)
		_, errOut := c.expect(1, "adopt", "nope-1234")
		eq(t, "stderr", errOut, "crew: no crew 'nope-1234' in this repo — run 'crew crews' to list them, or 'crew new' to start one\n")
	})

	t.Run("refuses a crew whose registered pid is alive, without leaking the pid", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		out, errOut := c.expect(1, "adopt", "c-live")
		eq(t, "stderr", errOut, "crew: crew 'c-live' still has a live dispatcher — 'crew new' starts your own; '--force' overrides if that process is a stale pid reuse\n")
		lacks(t, "stdout", out, pid)
		lacks(t, "stderr", errOut, pid)
	})

	t.Run("re-adopting this session's own live crew succeeds", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-mine", ppid)
		out, _ := c.expect(0, "adopt", "c-mine", ppid)
		eq(t, "stdout", out, "c-mine\n")
	})

	t.Run("a registered pid of 0 does not read as a live dispatcher", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-zero", "0")
		out, _ := c.expect(0, "adopt", "c-zero", ppid)
		eq(t, "stdout", out, "c-zero\n")
	})

	t.Run("prints the bare id, with no export prefix", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("c-bare")
		c.mustRun("status", "worker:feat/x#s1", "working")
		out, _ := c.expect(0, "adopt", "c-bare", ppid)
		eq(t, "stdout", out, "c-bare\n")
		if strings.HasPrefix(out, "export") {
			t.Errorf("stdout = %q, want no export prefix", out)
		}
	})

	t.Run("refuses an injection-shaped id even when the bus log carries it", func(t *testing.T) {
		c := newCrewsCLI(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		id := "x; touch " + marker
		c.seedEvent(id)
		_, errOut := c.expect(1, "adopt", id, ppid)
		eq(t, "stderr", errOut, invalidIDMsg)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("marker file exists (stat err %v)", err)
		}
	})

	t.Run("refuses a traversal id and writes nothing outside the crew dir", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.seedEvent("../../escaped")
		_, errOut := c.expect(1, "adopt", "../../escaped", ppid)
		eq(t, "stderr", errOut, invalidIDMsg)
		for _, p := range []string{filepath.Join(c.repo, ".git", "escaped"), filepath.Join(c.busDir(), "escaped")} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s exists (stat err %v)", p, err)
			}
		}
	})

	t.Run("--force overrides a live-pid refusal", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-live", strconv.Itoa(livePid(t)))
		// Not ppid, which the owner walk would also land on: the pid argument
		// has to be what gets written.
		self := strconv.Itoa(os.Getpid())
		out, _ := c.expect(0, "adopt", "c-live", "--force", self)
		eq(t, "stdout", out, "c-live\n")
		eq(t, "pid file", c.pidOf("c-live"), self)
	})

	t.Run("--force after the id does not write the literal --force as the pid", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-live", strconv.Itoa(livePid(t)))
		c.expect(0, "adopt", "c-live", "--force")
		if got := c.pidOf("c-live"); !regexp.MustCompile(`^[0-9]+$`).MatchString(got) {
			t.Errorf("pid file = %q, want digits", got)
		}
	})

	t.Run("an events-only crew is adoptable", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("c-events")
		c.mustRun("status", "worker:feat/x#s1", "working")
		out, _ := c.expect(0, "adopt", "c-events", ppid)
		eq(t, "stdout", out, "c-events\n")
	})

	t.Run("a dead-pid registered crew is adoptable without --force", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-dead", strconv.Itoa(deadPid(t)))
		out, _ := c.expect(0, "adopt", "c-dead", ppid)
		eq(t, "stdout", out, "c-dead\n")
	})

	t.Run("missing id is a usage error", func(t *testing.T) {
		c := newCrewsCLI(t)
		_, errOut := c.expect(1, "adopt")
		eq(t, "stderr", errOut, "crew: adopt [--force] <id> [pid]\n")
	})
}

func TestAdoptReleasesClaims(t *testing.T) {
	ppid := strconv.Itoa(os.Getppid())
	const release = "issue edit %s --remove-label dispatched"

	adoptDead := func(c *crewsCLI) {
		c.t.Helper()
		c.registerAs("c-dead", strconv.Itoa(deadPid(c.t)))
	}
	adopt := func(c *crewsCLI, id string, extra ...string) string {
		c.t.Helper()
		out, errOut := c.expect(0, append([]string{"adopt", id}, append(extra, ppid)...)...)
		eq(c.t, "stdout", out, id+"\n")
		return errOut
	}

	t.Run("releases a dead crew's recorded claim", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", "83", "feat/83-foo", 100)
		errOut := adopt(c, "c-dead")
		contains(t, "stderr", errOut, "released the dispatched label on #83")
		contains(t, "gh.log", stubLog(t, c.ghLog), fmt.Sprintf(release, "83"))
	})

	t.Run("releases every claim of a bundled branch (#615)", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", "42", "feat/42-bundle", 100)
		c.seedClaim("c-dead", "43", "feat/42-bundle", 100)
		adopt(c, "c-dead")
		contains(t, "gh.log", stubLog(t, c.ghLog), fmt.Sprintf(release, "42"), fmt.Sprintf(release, "43"))
	})

	t.Run("leaves another crew's claim alone", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", "50", "feat/50-a", 100)
		c.seedClaim("c-other", "99", "feat/99-b", 100)
		adopt(c, "c-dead")
		log := stubLog(t, c.ghLog)
		contains(t, "gh.log", log, fmt.Sprintf(release, "50"))
		lacks(t, "gh.log", log, "99")
	})

	t.Run("leaves alone an issue whose claim was later re-taken by another crew", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", "77", "feat/77-old", 100)
		c.seedClaim("c-live-other", "77", "feat/77-new", 200)
		adopt(c, "c-dead")
		lacks(t, "gh.log", stubLog(t, c.ghLog), "issue edit 77")
	})

	t.Run("leaves alone a branch that heads an open PR", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", "61", "feat/61-x", 100)
		t.Setenv("PR_OPEN_BRANCH", "feat/61-x")
		errOut := adopt(c, "c-dead")
		contains(t, "stderr", errOut, "keeping #61", "heads an open PR")
		lacks(t, "gh.log", stubLog(t, c.ghLog), "issue edit 61")
	})

	t.Run("leaves alone a branch with a live worker occupant", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.occupy("feat/72-y")
		c.seedClaim("c-dead", "72", "feat/72-y", 100)
		errOut := adopt(c, "c-dead")
		contains(t, "stderr", errOut, "keeping #72", "still occupies feat/72-y")
		lacks(t, "gh.log", stubLog(t, c.ghLog), "issue edit 72")
	})

	t.Run("--force over a live pid releases nothing", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-live", strconv.Itoa(livePid(t)))
		c.seedClaim("c-live", "55", "feat/55-z", 100)
		adopt(c, "c-live", "--force")
		eq(t, "gh.log", stubLog(t, c.ghLog), "")
	})

	t.Run("with no claim-issue rows, adopt makes no gh calls", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		adopt(c, "c-dead")
		eq(t, "gh.log", stubLog(t, c.ghLog), "")
	})

	t.Run("keeps an issue whose other claimed branch heads an open PR", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-live-other", "83", "feat/83-b", 50)
		c.seedClaim("c-dead", "83", "feat/83-a", 100)
		t.Setenv("PR_OPEN_BRANCH", "feat/83-b")
		errOut := adopt(c, "c-dead")
		contains(t, "stderr", errOut, "keeping #83", "feat/83-b heads an open PR")
		lacks(t, "gh.log", stubLog(t, c.ghLog), "issue edit 83")
	})

	t.Run("keeps an issue whose other claimed branch has a live occupant", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.occupy("feat/84-b")
		c.seedClaim("c-live-other", "84", "feat/84-b", 50)
		c.seedClaim("c-dead", "84", "feat/84-a", 100)
		errOut := adopt(c, "c-dead")
		contains(t, "stderr", errOut, "keeping #84", "still occupies feat/84-b")
		lacks(t, "gh.log", stubLog(t, c.ghLog), "issue edit 84")
	})

	t.Run("a numeric and a string issue are one issue for newest-claim-wins", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", 73, "feat/73-old", 100)
		c.seedClaim("c-live-other", "73", "feat/73-new", 200)
		adopt(c, "c-dead")
		lacks(t, "gh.log", stubLog(t, c.ghLog), "issue edit 73")
	})

	t.Run("a claim row with a malformed issue is skipped, never passed to gh", func(t *testing.T) {
		c := newCrewsCLI(t)
		adoptDead(c)
		c.seedClaim("c-dead", "90; rm -rf .", "feat/bad", 100)
		c.seedClaim("c-dead", "91", "feat/91-ok", 100)
		errOut := adopt(c, "c-dead")
		contains(t, "stderr", errOut, "not a number")
		log := stubLog(t, c.ghLog)
		lacks(t, "gh.log", log, "rm -rf")
		contains(t, "gh.log", log, fmt.Sprintf(release, "91"))
	})
}

func TestRegisterRefusals(t *testing.T) {
	t.Run("the refusal names the discovery commands", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("")
		_, errOut := c.expect(1, "register")
		eq(t, "stderr", errOut, noCrewRegMsg)
		contains(t, "stderr", errOut, "crew crews", "crew adopt", "crew new")
	})

	t.Run("id and register abort messages name adopt with no pid", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.as("")
		for _, sub := range []string{"id", "register"} {
			_, errOut := c.expect(1, sub)
			contains(t, sub+" stderr", errOut, "crew adopt <id>")
			lacks(t, sub+" stderr", errOut, "$PPID")
		}
	})
}

func TestAdoptRegisterNoPidRecordOwner(t *testing.T) {
	// The caller's parent is a shell under a non-shell engine process; the
	// owner walk must climb to it.
	underEngine := func(c *crewsCLI) {
		ppid := os.Getppid()
		c.proc = &proctest.Table{Procs: map[int]proctest.Proc{
			ppid:   {Parent: 777001, Comm: "bash"},
			777001: {Comm: "claude"},
		}}
	}

	t.Run("adopt: with no pid records the non-shell ancestor, not the calling subshell", func(t *testing.T) {
		c := newCrewsCLI(t)
		underEngine(c)
		c.registerAs("c-np", "0")
		c.expect(0, "adopt", "c-np")
		eq(t, "pid file", c.pidOf("c-np"), "777001")
	})

	t.Run("register: with no pid records the non-shell ancestor, not the calling subshell", func(t *testing.T) {
		c := newCrewsCLI(t)
		underEngine(c)
		c.as("c-rg")
		c.mustRun("register")
		eq(t, "pid file", c.pidOf("c-rg"), "777001")
	})
}

func TestPidFileSurvivesForeignCallers(t *testing.T) {
	ppid := strconv.Itoa(os.Getppid())

	// epermProbes keeps ps from vouching for pid 1, so only the kill(2) EPERM
	// answer can prove it live.
	epermCLI := func(t *testing.T) *crewsCLI {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("root can signal pid 1")
		}
		if err := syscall.Kill(1, 0); err != syscall.EPERM {
			t.Skipf("kill(1, 0) = %v, want EPERM", err)
		}
		c := newCrewsCLI(t)
		dir := t.TempDir()
		writeStub(t, dir, "ps", "#!/bin/sh\nexit 1\n")
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		return c
	}

	t.Run("deregister: a live non-ancestor pid keeps its crew dir and pid, exit 0", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		_, errOut := c.expect(0, "deregister")
		eq(t, "pid file", c.pidOf("c-live"), pid)
		eq(t, "stderr", errOut, "crew: crew 'c-live' still has a live dispatcher (pid "+pid+") that is not this caller's parent or owner — left in place\n")
	})

	t.Run("register: a different pid over a live one is refused and writes nothing", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		_, errOut := c.expect(1, "register")
		eq(t, "stderr", errOut, "crew: crew 'c-live' still has a live dispatcher (pid "+pid+") — 'crew new' starts your own crew; if that pid is a stale reuse, recover with 'crew adopt --force c-live'\n")
		contains(t, "stderr", errOut, "c-live", pid, "crew new", "crew adopt --force")
		c.expect(1, "register", "4242")
		eq(t, "pid file", c.pidOf("c-live"), pid)
	})

	t.Run("register: the same live pid again is idempotent", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		c.mustRun("register", pid)
		eq(t, "pid file", c.pidOf("c-live"), pid)
	})

	t.Run("register: a live ancestor pid is not replaced by a different pid", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-mine", ppid)
		c.expect(1, "register", "4242")
		eq(t, "pid file", c.pidOf("c-mine"), ppid)
	})

	t.Run("register: a live pid on another uid (kill EPERM) is still refused (#450)", func(t *testing.T) {
		c := epermCLI(t)
		c.registerAs("c-eperm", "1")
		_, errOut := c.expect(1, "register")
		contains(t, "stderr", errOut, "pid 1")
		eq(t, "pid file", c.pidOf("c-eperm"), "1")
	})

	t.Run("register: a recorded pid recycled by a later process is replaced, not stranded (#450)", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-rec", strconv.Itoa(livePid(t)))
		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(c.crewDir("c-rec"), "pid"), old, old); err != nil {
			t.Fatal(err)
		}
		c.mustRun("register", "4242")
		eq(t, "pid file", c.pidOf("c-rec"), "4242")
	})

	t.Run("crews: a live foreign-uid pid (kill EPERM) still lists as alive (#450)", func(t *testing.T) {
		c := epermCLI(t)
		c.registerAs("c-eperm", "1")
		row := wantRow(t, c.mustRun("crews"), "c-eperm")
		eq(t, "pid", row[4], "1")
		eq(t, "alive", row[5], "yes")
	})

	t.Run("worker worktree: WORKER_TASK.md crew_id cannot deregister or re-register a live crew", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		if err := os.WriteFile(filepath.Join(c.repo, "WORKER_TASK.md"), []byte("crew_id: c-live\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.as("")
		c.expect(0, "deregister")
		c.expect(1, "register")
		eq(t, "pid file", c.pidOf("c-live"), pid)
	})

	t.Run("reap and other crews' register/deregister/adopt leave a live pid alone", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		c.seedEvent("c-live")
		c.expect(0, "reap", "--dry-run")
		c.expect(0, "reap", "--idle", "0")
		c.registerAs("c-other", strconv.Itoa(deadPid(t)))
		c.expect(0, "adopt", "c-other", ppid)
		c.mustRun("deregister")
		eq(t, "pid file", c.pidOf("c-live"), pid)
	})

	t.Run("deregister: removes the crew when the recorded pid is the caller's parent or dead", func(t *testing.T) {
		c := newCrewsCLI(t)
		c.registerAs("c-ppid", ppid)
		c.mustRun("deregister")
		if _, err := os.Stat(c.crewDir("c-ppid")); !os.IsNotExist(err) {
			t.Errorf("c-ppid dir still there (stat err %v)", err)
		}
		c.registerAs("c-dead", strconv.Itoa(deadPid(t)))
		c.mustRun("deregister")
		if _, err := os.Stat(c.crewDir("c-dead")); !os.IsNotExist(err) {
			t.Errorf("c-dead dir still there (stat err %v)", err)
		}
	})

	t.Run("deregister: a live ancestor that is not the caller's parent or owner is refused", func(t *testing.T) {
		c := newCrewsCLI(t)
		ppidN := os.Getppid()
		// The pid is an ancestor above the caller's parent, which the real
		// process tree cannot supply in-process.
		c.proc = &proctest.Table{Procs: map[int]proctest.Proc{
			777002:      {},
			os.Getpid(): {Parent: ppidN},
			ppidN:       {Parent: 777002, Comm: "go"},
		}}
		c.registerAs("c-mine", "777002")
		c.mustRun("deregister")
		eq(t, "pid file", c.pidOf("c-mine"), "777002")
	})

	t.Run("register/deregister: an invalid crew id exits 1 and touches nothing", func(t *testing.T) {
		c := newCrewsCLI(t)
		pid := strconv.Itoa(livePid(t))
		c.registerAs("c-live", pid)
		c.as("..")
		c.expect(1, "deregister")
		c.as("a/../x")
		c.expect(1, "register")
		if st, err := os.Stat(filepath.Join(c.busDir(), "crews")); err != nil || !st.IsDir() {
			t.Errorf("crews dir missing (err %v)", err)
		}
		eq(t, "pid file", c.pidOf("c-live"), pid)
	})

	t.Run("pidfile.log: removal, refusal and adopt name the action, crew and caller", func(t *testing.T) {
		c := newCrewsCLI(t)
		live := strconv.Itoa(livePid(t))
		c.registerAs("c-live", live)
		c.expect(1, "register", "4242")
		c.mustRun("deregister")
		c.expect(0, "adopt", "c-live", "--force", ppid)
		c.mustRun("deregister")
		dead := strconv.Itoa(deadPid(t))
		c.registerAs("c-gone", dead)
		c.mustRun("deregister")
		contains(t, "pidfile.log", c.pidfileLog(),
			"register ok crew=c-live",
			"register refused crew=c-live old="+live+" new=4242",
			"deregister refused crew=c-live",
			"adopt forced crew=c-live old="+live+" new="+ppid,
			"deregister removed crew=c-gone old="+dead,
			"cwd="+c.repo,
			"by=")
	})
}
