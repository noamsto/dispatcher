package probe

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func envOf(m map[string]string, stderr *bytes.Buffer) Env {
	e := Env{
		Getenv: func(k string) string { return m[k] },
		Pane:   "%7",
		CrewSH: "/nonexistent/crew.sh",
	}
	if stderr != nil {
		e.Stderr = stderr
	}
	return e
}

// shebang is the stub scripts' interpreter line: bash by absolute path, since
// the Nix build sandbox has no /usr/bin/env.
func shebang(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	return "#!" + bash + "\n"
}

// stubBin puts executable stubs first on PATH. Each stub logs its argv, one
// bracketed word per argument, to <name>.log and then runs its body.
func stubBin(t *testing.T, stubs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	sb := shebang(t)
	for name, body := range stubs {
		script := sb +
			"{ for a in \"$@\"; do printf '[%s]' \"$a\"; done; echo; } >>\"" + filepath.Join(dir, name+".log") + "\"\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestSampleSeam(t *testing.T) {
	cases := []struct {
		name, cmd string
		text      string
		alive     bool
	}{
		{"strips every trailing newline", `printf 'hi\n\n\n'`, "hi", true},
		{"keeps leading and inner newlines", `printf '\na\n\nb\n'`, "\na\n\nb", true},
		{"nonzero rc is not alive, output kept", `printf 'gone\n'; exit 3`, "gone", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Default(envOf(map[string]string{"CREW_STALL_SAMPLE_CMD": c.cmd}, nil))
			text, alive := p.Sample(context.Background())
			if text != c.text || alive != c.alive {
				t.Errorf("Sample = %q, %v; want %q, %v", text, alive, c.text, c.alive)
			}
		})
	}
}

func TestSeamStderrDiscarded(t *testing.T) {
	var stderr bytes.Buffer
	p := Default(envOf(map[string]string{"CREW_STALL_SAMPLE_CMD": `echo noise >&2; printf ok`}, &stderr))
	if text, _ := p.Sample(context.Background()); text != "ok" {
		t.Errorf("Sample = %q, want ok", text)
	}
	if stderr.Len() != 0 {
		t.Errorf("seam stderr leaked to Env.Stderr: %q", stderr.String())
	}
}

func TestSeamRunsUnderBashC(t *testing.T) {
	p := Default(envOf(map[string]string{"CREW_STALL_SAMPLE_CMD": `[[ -n $BASH_VERSION ]] && printf bash`}, nil))
	if text, alive := p.Sample(context.Background()); text != "bash" || !alive {
		t.Errorf("Sample = %q, %v; want bash, true", text, alive)
	}
}

func TestSampleColoredOrder(t *testing.T) {
	dir := stubBin(t, map[string]string{"tmux": `printf 'tmux-colored\n'`})
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"color cmd wins", map[string]string{"CREW_STALL_COLOR_CMD": `printf 'color\n'`, "CREW_STALL_SAMPLE_CMD": "printf sample"}, "color"},
		{"sample cmd next", map[string]string{"CREW_STALL_SAMPLE_CMD": `printf 'sample\n\n'`}, "sample"},
		{"tmux last", map[string]string{}, "tmux-colored"},
		{"failing color cmd is empty, no fallthrough", map[string]string{"CREW_STALL_COLOR_CMD": "printf partial; exit 1", "CREW_STALL_SAMPLE_CMD": "printf sample"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Default(envOf(c.env, nil))
			if got := p.SampleColored(context.Background()); got != c.want {
				t.Errorf("SampleColored = %q, want %q", got, c.want)
			}
		})
	}
	if got := readLog(t, dir, "tmux"); got != "[capture-pane][-e][-p][-t][%7]\n" {
		t.Errorf("tmux argv = %q", got)
	}
}

func TestSampleDefaultArgv(t *testing.T) {
	dir := stubBin(t, map[string]string{"tmux": `printf 'frame\n\n'`})
	text, alive := Default(envOf(nil, nil)).Sample(context.Background())
	if text != "frame" || !alive {
		t.Errorf("Sample = %q, %v; want frame, true", text, alive)
	}
	if got := readLog(t, dir, "tmux"); got != "[capture-pane][-p][-t][%7]\n" {
		t.Errorf("tmux argv = %q", got)
	}
}

func TestSampleDefaultFailureIsNotAlive(t *testing.T) {
	stubBin(t, map[string]string{"tmux": `echo "can't find pane" >&2; exit 1`})
	var stderr bytes.Buffer
	if text, alive := Default(envOf(nil, &stderr)).Sample(context.Background()); text != "" || alive {
		t.Errorf("Sample = %q, %v; want empty, false", text, alive)
	}
	if stderr.Len() != 0 {
		t.Errorf("tmux stderr leaked: %q", stderr.String())
	}
}

func TestPaneCmd(t *testing.T) {
	t.Run("seam", func(t *testing.T) {
		p := Default(envOf(map[string]string{"CREW_STALL_PROC_CMD": `printf 'claude\n\n'; exit 1`}, nil))
		if got := p.PaneCmd(context.Background()); got != "claude" {
			t.Errorf("PaneCmd = %q, want claude (output survives a nonzero rc)", got)
		}
	})
	t.Run("default picks the pane's row", func(t *testing.T) {
		dir := stubBin(t, map[string]string{"tmux": `printf '%%1\tfish\n%%7\tclaude\n%%70\tnvim\n'`})
		if got := Default(envOf(nil, nil)).PaneCmd(context.Background()); got != "claude" {
			t.Errorf("PaneCmd = %q, want claude", got)
		}
		if got := readLog(t, dir, "tmux"); got != "[list-panes][-a][-F][#{pane_id}\t#{pane_current_command}]\n" {
			t.Errorf("tmux argv = %q", got)
		}
	})
	t.Run("default without the pane is empty", func(t *testing.T) {
		stubBin(t, map[string]string{"tmux": `printf '%%1\tfish\n'`})
		if got := Default(envOf(nil, nil)).PaneCmd(context.Background()); got != "" {
			t.Errorf("PaneCmd = %q, want empty", got)
		}
	})
	t.Run("default on failure is empty", func(t *testing.T) {
		stubBin(t, map[string]string{"tmux": `printf '%%7\tclaude\n'; exit 1`})
		if got := Default(envOf(nil, nil)).PaneCmd(context.Background()); got != "" {
			t.Errorf("PaneCmd = %q, want empty", got)
		}
	})
}

func TestPaneModel(t *testing.T) {
	dir := stubBin(t, map[string]string{"tmux": `printf 'qwen3\n\n'`})
	if got := Default(envOf(nil, nil)).PaneModel(context.Background()); got != "qwen3" {
		t.Errorf("PaneModel = %q, want qwen3", got)
	}
	if got := readLog(t, dir, "tmux"); got != "[show-options][-pqv][-t][%7][@crew_model]\n" {
		t.Errorf("tmux argv = %q", got)
	}
	stubBin(t, map[string]string{"tmux": `printf 'x'; exit 1`})
	if got := Default(envOf(nil, nil)).PaneModel(context.Background()); got != "" {
		t.Errorf("PaneModel on failure = %q, want empty", got)
	}
}

func TestLoadSeam(t *testing.T) {
	p := Default(envOf(map[string]string{"CREW_STALL_LOAD_CMD": `printf '3.5 8\n\n'`}, nil))
	if got := p.Load(context.Background()); got != "3.5 8" {
		t.Errorf("Load = %q, want %q", got, "3.5 8")
	}
}

func TestLoadDefault(t *testing.T) {
	if _, err := os.Stat("/proc/loadavg"); err != nil {
		t.Skip("no /proc/loadavg")
	}
	dir := stubBin(t, map[string]string{"nproc": `echo 12`})
	p := Default(envOf(nil, nil))
	re := regexp.MustCompile(`^\d+\.\d+ 12$`)
	for range 2 {
		if got := p.Load(context.Background()); !re.MatchString(got) {
			t.Errorf("Load = %q, want <load1> 12", got)
		}
	}
	if got := readLog(t, dir, "nproc"); got != "\n" {
		t.Errorf("nproc ran %q, want exactly one bare call", got)
	}
}

func TestLoadDefaultNprocFailure(t *testing.T) {
	if _, err := os.Stat("/proc/loadavg"); err != nil {
		t.Skip("no /proc/loadavg")
	}
	stubBin(t, map[string]string{"nproc": `echo junk; exit 1`})
	if got := Default(envOf(nil, nil)).Load(context.Background()); !regexp.MustCompile(`^\d+\.\d+ 1$`).MatchString(got) {
		t.Errorf("Load = %q, want <load1> 1", got)
	}
}

func TestTopSeam(t *testing.T) {
	p := Default(envOf(map[string]string{"CREW_STALL_TOP_CMD": `printf 'a 1 9.0\nb 2 8.0\nc 3 7.0\n\n'`}, nil))
	if got := p.Top(context.Background()); got != "a 1 9.0\nb 2 8.0\nc 3 7.0" {
		t.Errorf("Top = %q, want the seam output with only trailing newlines stripped", got)
	}
}

func TestTopDefault(t *testing.T) {
	dir := stubBin(t, map[string]string{"ps": `cat <<'PS'
/home/u/git/repo/.worktrees/feat-x node 4242 99.0
/srv/a b/c/d claude 77 12.0
/ init 1  0.5
PS`})
	got := Default(envOf(nil, nil)).Top(context.Background())
	if want := ".worktrees/feat-x node 4242 99.0\nc/d claude 77 12.0"; got != want {
		t.Errorf("Top = %q, want %q", got, want)
	}
	if log := readLog(t, dir, "ps"); log != "[-eo][cwd=,comm=,pid=,pcpu=][--sort=-pcpu]\n" {
		t.Errorf("ps argv = %q", log)
	}
}

func TestTopDefaultKeepsUncollapsibleLines(t *testing.T) {
	stubBin(t, map[string]string{"ps": `printf '/ init 1  0.5\n/tmp bash 9 0.0\n'`})
	if got := Default(envOf(nil, nil)).Top(context.Background()); got != "/ init 1  0.5\n/tmp bash 9 0.0" {
		t.Errorf("Top = %q", got)
	}
}

func TestTopDefaultCollapseMatchesSed(t *testing.T) {
	sed, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("no sed")
	}
	lines := []string{
		"/home/u/git/repo/.worktrees/feat-x node 4242 99.0",
		"/ init 1  0.5",
		"/srv/a b/c/d claude 77 12.0",
		"/home/u/proj tsc 9  1.0",
		"/a/b/c/d/e x 1 2.0 ",
		"/a/b/c d/e f g ",
		"relative/x/y z 3 4.0",
		"/x//y z 3 4.0",
	}
	for _, line := range lines {
		cmd := exec.Command(sed, "-E", `s#.*/([^/]+/[^/]+) #\1 #`)
		cmd.Stdin = strings.NewReader(line + "\n")
		want, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		stubBin(t, map[string]string{"ps": "cat <<'PS'\n" + line + "\nPS"})
		if got := Default(envOf(nil, nil)).Top(context.Background()); got != strings.TrimSuffix(string(want), "\n") {
			t.Errorf("line %q: Top = %q, sed = %q", line, got, want)
		}
	}
}

func TestTopDefaultPsFailure(t *testing.T) {
	stubBin(t, map[string]string{"ps": `echo partial; exit 1`})
	if got := Default(envOf(nil, nil)).Top(context.Background()); got != "" {
		t.Errorf("Top = %q, want empty on ps failure", got)
	}
}

func TestRefreshBudgetSeam(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	var stderr bytes.Buffer
	p := Default(envOf(map[string]string{"CREW_BUDGET_REFRESH_CMD": `echo out; echo err >&2; echo ran >` + marker + `; exit 5`}, &stderr))
	p.RefreshBudget(context.Background())
	if b, _ := os.ReadFile(marker); string(b) != "ran\n" {
		t.Errorf("refresh seam did not run, marker = %q", b)
	}
	if stderr.Len() != 0 {
		t.Errorf("refresh stderr leaked: %q", stderr.String())
	}
}

func TestRefreshBudgetDefault(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	dir := stubBin(t, map[string]string{"refresh-budget": `echo "w=${CREW_WORKER_ID-unset} id=${CREW_ID-unset} keep=${KEEP-unset}" >` + out + `; echo noise; echo err >&2`})
	t.Setenv("CREW_WORKER_ID", "w1")
	t.Setenv("CREW_ID", "c1")
	t.Setenv("KEEP", "yes")
	var stderr bytes.Buffer
	Default(envOf(nil, &stderr)).RefreshBudget(context.Background())
	if b, _ := os.ReadFile(out); string(b) != "w=unset id=unset keep=yes\n" {
		t.Errorf("refresh-budget env = %q", b)
	}
	if got := readLog(t, dir, "refresh-budget"); got != "\n" {
		t.Errorf("refresh-budget argv = %q, want none", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("refresh-budget stderr leaked: %q", stderr.String())
	}
}

func TestRefreshBudgetNoCommandIsNoop(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	Default(envOf(nil, nil)).RefreshBudget(context.Background())
}

func TestSetPaneOption(t *testing.T) {
	t.Run("argv, errors ignored", func(t *testing.T) {
		var stderr bytes.Buffer
		dir := stubBin(t, map[string]string{"tmux": `echo no >&2; exit 1`})
		Default(envOf(nil, &stderr)).SetPaneOption(context.Background(), "@crew_detail", "two words")
		if got := readLog(t, dir, "tmux"); got != "[set-option][-p][-t][%7][@crew_detail][two words]\n" {
			t.Errorf("tmux argv = %q", got)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr leaked: %q", stderr.String())
		}
	})
	t.Run("no tmux is a noop", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		Default(envOf(nil, nil)).SetPaneOption(context.Background(), "@crew_state", "idle")
	})
}

// shScript prints its argv, its shell options and its cwd, warns on stderr and
// exits 3.
const shScript = `printf '%s\n' "$*"; printf 'opts=%s\n' "$SHELLOPTS"; pwd; printf '\n'; echo warn >&2; exit 3`

// A raw crew.sh source is not executable: it runs under bash with crew.sh's
// writeShellApplication options, from the repo's git common dir.
func TestSh(t *testing.T) {
	crewSH := filepath.Join(t.TempDir(), "crew.sh")
	if err := os.WriteFile(crewSH, []byte(shScript), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	e := envOf(nil, &stderr)
	e.CrewSH = crewSH
	e.Dir = t.TempDir()
	out, rc := Default(e).Sh(context.Background(), "nudge", "%7", "claude", "")
	if rc != 3 {
		t.Errorf("rc = %d, want 3", rc)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[0] != "stall-watch --sh nudge %7 claude " || lines[2] != e.Dir {
		t.Fatalf("out = %q, want the argv, opts and cwd lines, trailing newlines stripped", out)
	}
	for _, opt := range []string{"errexit", "nounset", "pipefail"} {
		if !strings.Contains(lines[1], opt) {
			t.Errorf("bash options %q lack %s", lines[1], opt)
		}
	}
	if stderr.String() != "warn\n" {
		t.Errorf("stderr = %q, want warn", stderr.String())
	}
}

// The Nix-built crew is executable and carries its own pinned interpreter and
// options, so it runs as is: no bash wrapper adds errexit here.
func TestShExecutable(t *testing.T) {
	crewSH := filepath.Join(t.TempDir(), "crew")
	if err := os.WriteFile(crewSH, []byte(shebang(t)+shScript), 0o755); err != nil {
		t.Fatal(err)
	}
	e := envOf(nil, nil)
	e.CrewSH = crewSH
	e.Dir = t.TempDir()
	t.Setenv("PATH", t.TempDir())
	out, rc := Default(e).Sh(context.Background(), "release", "feat/x")
	lines := strings.Split(out, "\n")
	if rc != 3 || len(lines) != 3 || lines[0] != "stall-watch --sh release feat/x" || lines[2] != e.Dir {
		t.Fatalf("Sh = %q, %d; want the argv and cwd lines, 3", out, rc)
	}
	if strings.Contains(lines[1], "errexit") {
		t.Errorf("options %q: ran under a bash -e wrapper", lines[1])
	}
}

func TestShUnstartable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, rc := Default(envOf(nil, nil)).Sh(context.Background(), "x"); rc != -1 {
		t.Errorf("rc = %d, want -1 for a child that never started", rc)
	}
}

func TestShKilledBySignal(t *testing.T) {
	crewSH := filepath.Join(t.TempDir(), "crew.sh")
	if err := os.WriteFile(crewSH, []byte(`kill -KILL $$`), 0o644); err != nil {
		t.Fatal(err)
	}
	e := envOf(nil, nil)
	e.CrewSH = crewSH
	if _, rc := Default(e).Sh(context.Background(), "x"); rc != -1 {
		t.Errorf("rc = %d, want -1 for a killed child", rc)
	}
}

func TestBusRows(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(log, []byte("{\"a\":1}\n{\"b\":\"x\"}\n{\"torn\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Default(envOf(nil, nil))
	rows, ok := p.BusRows(log)
	if !ok || len(rows) != 2 {
		t.Fatalf("BusRows = %d rows, ok=%v; want the 2-row well-formed prefix, true", len(rows), ok)
	}
	v, _ := rows[1].Get("b")
	if s, _ := v.AsString(); s != "x" {
		t.Errorf("row 1 field b = %q, want x", s)
	}
	if _, ok := p.BusRows(dir); ok {
		t.Error("a directory reported ok")
	}
	if _, ok := p.BusRows(filepath.Join(dir, "missing")); ok {
		t.Error("a missing path reported ok")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rows, ok := p.BusRows(empty); !ok || len(rows) != 0 {
		t.Errorf("empty log = %d rows, ok=%v; want 0, true", len(rows), ok)
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := Default(envOf(map[string]string{"CREW_BUDGET_REFRESH_CMD": `sleep 30 & echo $! >` + pidFile + `; wait`}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.RefreshBudget(ctx)
		close(done)
	}()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 {
		if time.Now().After(deadline) {
			t.Fatal("seam never wrote the sleep pid")
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("RefreshBudget still blocked 1s after cancel")
	}

	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild sleep %d survived the cancel", pid)
		}
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := Default(envOf(map[string]string{"CREW_STALL_SAMPLE_CMD": "printf x"}, nil))
	if text, alive := p.Sample(ctx); text != "" || alive {
		t.Errorf("Sample on a cancelled ctx = %q, %v; want empty, false", text, alive)
	}
}
