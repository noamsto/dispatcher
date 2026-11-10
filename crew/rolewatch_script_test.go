package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/rogpeppe/go-internal/txtar"

	"github.com/noamsto/dispatcher/crew/internal/drift"
	"github.com/noamsto/dispatcher/crew/internal/proc"
)

// crewMain is the real binary's entry, as testscript's `crew` command.
func crewMain() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, env{
		getwd:  os.Getwd,
		probes: probes,
		proc:   proc.OS{},
		stdin:  os.Stdin,
	}))
}

func TestRoleWatchScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/rolewatch",
		Setup: func(e *testscript.Env) error {
			e.Setenv("FAKE_TMUX_DIR", filepath.Join(e.WorkDir, "tmux"))
			e.Setenv("FAKE_TMUX_LOG", filepath.Join(e.WorkDir, "tmux.log"))
			return nil
		},
	})
}

// The fake tmux keeps its state under $FAKE_TMUX_DIR and logs every argv, one
// space-joined line, to $FAKE_TMUX_LOG.
//
//	state              key=value lines: role window delivery exited branch crew
//	                   width zoomed layout min_cols pane lifetime
//	frames/<n>         the nth capture-pane; the last one repeats
//	after-<ev>-<n>     key=value lines merged into state once the nth <ev> has
//	                   answered; ev is alive (a #{pane_id} read), capture or
//	                   enter (send-keys -t <pane> Enter)
//	append-<ev>-<n>    rows appended to .git/crew/events.jsonl (in the working
//	                   directory) once the nth <ev> has answered
//	n.<ev>             the event counters
//	buffer             stdin of the last load-buffer
//
// After `lifetime` #{pane_id} reads (default 100) the pane is gone, which ends
// a watcher. Anything the watchers do not issue exits 1.
func fakeTmux() {
	os.Exit(fakeTmuxRun(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

var reAssignBuffer = regexp.MustCompile(`rw-assign-[0-9]+-[0-9]+`)

func fakeTmuxRun(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir := os.Getenv("FAKE_TMUX_DIR")
	line := reAssignBuffer.ReplaceAllString(strings.Join(args, " "), "rw-assign-N")
	f, err := os.OpenFile(os.Getenv("FAKE_TMUX_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "fake tmux:", err)
		return 2
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()

	ft := &fakeTmuxState{dir: dir, kv: map[string]string{}}
	ft.load()
	out, ok := ft.answer(args, stdin)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "fake tmux: unsupported argv: %s\n", line)
		return 1
	}
	if out != "" {
		_, _ = fmt.Fprintln(stdout, out)
	}
	return 0
}

type fakeTmuxState struct {
	dir string
	kv  map[string]string
}

func (s *fakeTmuxState) load() {
	data, _ := os.ReadFile(filepath.Join(s.dir, "state"))
	s.merge(string(data))
}

func (s *fakeTmuxState) merge(text string) {
	for _, l := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			s.kv[k] = v
		}
	}
}

// event counts one event and applies the after-<ev>-<n> override, if any.
func (s *fakeTmuxState) event(ev string) int {
	path := filepath.Join(s.dir, "n."+ev)
	data, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)+"\n"), 0o644)
	if over, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("after-%s-%d", ev, n))); err == nil {
		s.merge(string(over))
		var keys []string
		for k := range s.kv {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, s.kv[k])
		}
		_ = os.WriteFile(filepath.Join(s.dir, "state"), []byte(b.String()), 0o644)
	}
	if rows, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("append-%s-%d", ev, n))); err == nil {
		if f, err := os.OpenFile(".git/crew/events.jsonl", os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.Write(rows)
			_ = f.Close()
		}
	}
	return n
}

func (s *fakeTmuxState) get(k, def string) string {
	if v, ok := s.kv[k]; ok {
		return v
	}
	return def
}

// frame is the nth capture, or the last one before it.
func (s *fakeTmuxState) frame(n int) string {
	for ; n > 0; n-- {
		if data, err := os.ReadFile(filepath.Join(s.dir, "frames", strconv.Itoa(n))); err == nil {
			return strings.TrimSuffix(string(data), "\n")
		}
	}
	return ""
}

func (s *fakeTmuxState) answer(args []string, stdin io.Reader) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	switch args[0] {
	case "display-message":
		if len(args) != 5 || args[1] != "-p" || args[2] != "-t" {
			return "", false
		}
		return s.display(args[4])
	case "show-options":
		switch {
		case len(args) == 5 && args[1] == "-wqv" && args[2] == "-t" && args[4] == "@crew_branch":
			return s.get("branch", ""), true
		case len(args) == 5 && args[1] == "-wqv" && args[2] == "-t" && args[4] == "@crew_id":
			return s.get("crew", ""), true
		case len(args) == 7 && slices.Equal(args[1:4], []string{"-w", "-v", "-q"}) && args[4] == "-t" && args[6] == "@grid_refit_min_role_cols":
			return s.get("min_cols", ""), true
		}
	case "set-option":
		return "", len(args) == 6 && args[1] == "-p" && args[2] == "-t" && args[4] == "@crew_state"
	case "capture-pane":
		if len(args) != 5 || args[1] != "-e" || args[2] != "-p" || args[3] != "-t" {
			return "", false
		}
		n := s.event("capture")
		return s.frame(n), true
	case "send-keys":
		if len(args) == 4 && args[1] == "-t" && args[3] == "Enter" {
			s.event("enter")
		}
		return "", len(args) > 1
	case "load-buffer":
		if len(args) != 4 || args[1] != "-b" || args[3] != "-" {
			return "", false
		}
		data, _ := io.ReadAll(stdin)
		return "", os.WriteFile(filepath.Join(s.dir, "buffer"), data, 0o644) == nil
	case "paste-buffer", "delete-buffer", "set-window-option", "select-layout":
		return "", len(args) > 1
	case "list-panes":
		switch {
		case len(args) > 1 && args[1] == "-a":
			return "", true
		case len(args) == 5 && args[1] == "-t" && args[3] == "-F":
			return fmt.Sprintf("%s|%s|%s", s.get("pane", "%6"), s.get("role", ""), s.get("width", "")), true
		}
	}
	return "", false
}

func (s *fakeTmuxState) display(format string) (string, bool) {
	switch format {
	case "#{pane_id}":
		n := s.event("alive")
		lifetime, _ := strconv.Atoi(s.get("lifetime", "100"))
		if n > lifetime {
			return "", true
		}
		return s.get("pane", "%6"), true
	case "#{@crew_exited}":
		return s.get("exited", "0"), true
	case "#{@crew_role}|#{window_id}|#{@crew_delivery}":
		return s.get("role", "") + "|" + s.get("window", "@1") + "|" + s.get("delivery", ""), true
	case "#{@crew_role}|#{window_id}":
		return s.get("role", "") + "|" + s.get("window", "@1"), true
	case "#{window_zoomed_flag}":
		return s.get("zoomed", "0"), true
	case "#{pane_width}":
		return s.get("width", ""), true
	case "#{window_layout}":
		return s.get("layout", ""), true
	}
	return "", false
}

// differentialCases are the scripts whose argv log is the bash arm's: the
// differential test runs the arm and `crew role-watch` against each script's
// fake state, and the script's golden.log is the arm's log.
var differentialCases = []struct {
	name    string
	notices int // role-posted msg rows each run must leave
}{
	{"deliver_idle_claude", 0}, {"held_resend", 2}, {"stamp_flip_after_enter", 2},
	{"pi_retype", 2}, {"reguard_width", 2}, {"reguard_zoomed", 2}, {"unknown_escalation", 2},
}

// TestRoleWatchDifferential runs the dispatch.sh --role-watch arm and the Go
// watcher against identical fake state and compares their tmux argv and the
// notices they post. ROLEWATCH_GOLDEN_DIR=<dir> also writes the arm's argv log
// to <dir>/<case>.log, to be pasted as the script's golden.log.
func TestRoleWatchDifferential(t *testing.T) {
	root := filepath.Join(pkgDir, "..")
	arm := filepath.Join(root, "adapters", "core", "dispatch.sh")
	for _, tool := range []string{"bash", "jq", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			drift.Missing(t, tool, err)
		}
	}
	if _, err := os.Stat(arm); err != nil {
		drift.Missing(t, arm, err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"crew", "tmux"} {
		if err := os.Symlink(self, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range differentialCases {
		name := c.name
		t.Run(name, func(t *testing.T) {
			ar, err := txtar.ParseFile(filepath.Join(pkgDir, "testdata", "rolewatch", name+".txtar"))
			if err != nil {
				t.Fatal(err)
			}
			args := watcherArgs(t, string(ar.Comment))
			goLog, goNotices := runWatcher(t, ar, bin, root, nil, args)
			bashLog, bashNotices := runWatcher(t, ar, bin, root, []string{"bash", arm, "--role-watch"}, args)
			if dir := os.Getenv("ROLEWATCH_GOLDEN_DIR"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, name+".log"), []byte(bashLog), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if goLog != bashLog {
				t.Errorf("tmux argv differs\n--- bash arm\n%s--- crew role-watch\n%s", bashLog, goLog)
			}
			if len(bashNotices) != c.notices {
				t.Errorf("the arm posted %d notices, want %d: %v", len(bashNotices), c.notices, bashNotices)
			}
			if !reflect.DeepEqual(goNotices, bashNotices) {
				t.Errorf("notices differ\n--- bash arm\n%v\n--- crew role-watch\n%v", bashNotices, goNotices)
			}
		})
	}
}

// watcherArgs is the `exec crew role-watch …` line of a script's comment, so
// the script and the differential run one command line.
func watcherArgs(t *testing.T, comment string) []string {
	t.Helper()
	for _, l := range strings.Split(comment, "\n") {
		if rest, ok := strings.CutPrefix(l, "exec crew role-watch "); ok {
			return strings.Fields(rest)
		}
	}
	t.Fatal("no `exec crew role-watch` line in the script")
	return nil
}

type notice struct {
	From, To string
	Body     any
}

// runWatcher lays the archive out in a fresh repo and runs one watcher in it:
// `crew role-watch args` when prefix is nil, else prefix+args (the bash arm).
// It returns the tmux log and the role-posted msg rows.
func runWatcher(t *testing.T, ar *txtar.Archive, bin, root string, prefix, args []string) (string, []notice) {
	t.Helper()
	work := t.TempDir()
	for _, f := range ar.Files {
		if f.Name == "golden.log" {
			continue
		}
		path := filepath.Join(work, f.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runIn := func(name string, a ...string) {
		cmd := exec.Command(name, a...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, a, err, out)
		}
	}
	runIn("git", "init", "-q")
	runIn("mkdir", ".git/crew")
	runIn("cp", "events.jsonl", ".git/crew/events.jsonl")

	cmdline := append([]string{"crew", "role-watch"}, args...)
	if prefix != nil {
		cmdline = append(slices.Clone(prefix), args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmdline[0], cmdline[1:]...)
	cmd.Dir = work
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + work,
		"XDG_CONFIG_HOME=" + filepath.Join(work, "xdg-config"),
		"XDG_CACHE_HOME=" + filepath.Join(work, "xdg-cache"),
		"FAKE_TMUX_DIR=" + filepath.Join(work, "tmux"),
		"FAKE_TMUX_LOG=" + filepath.Join(work, "tmux.log"),
		"DISPATCH_ROLE_WATCH_CLOCK=" + filepath.Join(work, "clock"),
		"GRANT_CHECK_LIB=" + filepath.Join(root, "adapters/core/grant-check.sh"),
		"LAUNCHER_COMMON_LIB=" + filepath.Join(root, "adapters/core/launcher-common.sh"),
		"WORKTREE_GIT_LIB=" + filepath.Join(root, "adapters/core/worktree-git.sh"),
		"LOCAL_MODELS_LIB=" + filepath.Join(root, "adapters/core/local-models.sh"),
		"BUDGET_GATE_LIB=" + filepath.Join(root, "adapters/core/budget-gate.sh"),
		"OPENROUTER_KEY_LIB=" + filepath.Join(root, "adapters/core/openrouter-key.sh"),
		"MODEL_RESOLVE_BIN=" + filepath.Join(root, "adapters/core/model-resolve.sh"),
		"DISPATCH_CONFIG_BIN=" + filepath.Join(root, "adapters/core/dispatch-config.sh"),
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\n%s", cmdline, err, stderr.String())
	}
	log, _ := os.ReadFile(filepath.Join(work, "tmux.log"))
	return string(log), roleNotices(t, filepath.Join(work, ".git/crew/events.jsonl"))
}

// roleNotices are the msg rows the watcher posted, with the body parsed as
// JSON when it is, so a key-order or spacing difference is not a difference.
func roleNotices(t *testing.T, path string) []notice {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []notice
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row struct {
			Kind, From, To string
			Body           string
		}
		if json.Unmarshal([]byte(l), &row) != nil || row.Kind != "msg" || !strings.HasPrefix(row.From, "role:") {
			continue
		}
		n := notice{From: row.From, To: row.To}
		if json.Unmarshal([]byte(row.Body), &n.Body) != nil {
			n.Body = row.Body
		}
		out = append(out, n)
	}
	return out
}
