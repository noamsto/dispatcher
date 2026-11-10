package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

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
			e.Setenv("FRAMES", filepath.Join(pkgDir, "internal", "frame", "testdata", "frames"))
			return nil
		},
	})
}

// The fake tmux keeps its state under $FAKE_TMUX_DIR and logs every argv, one
// space-joined line, to $FAKE_TMUX_LOG.
//
//	state              key=value lines: role window delivery exited branch crew
//	                   width zoomed layout min_cols pane lifetime
//	                   load_buffer_fail paste_buffer_fail (non-empty: that
//	                   command exits 1; an after-<ev>-<n> file clears it)
//	frames/<n>         the nth capture-pane; the last one repeats
//	after-<ev>-<n>     key=value lines merged into state once the nth <ev> has
//	                   answered; ev is alive (a #{pane_id} read), capture or
//	                   enter (send-keys -t <pane> Enter)
//	append-<ev>-<n>    rows appended to .git/crew/events.jsonl (in the working
//	                   directory) once the nth <ev> has answered
//	n.<ev>             the event counters
//	buffer             stdin of the last load-buffer
//	loads              one line per load-buffer: the captures answered so far, a
//	                   space, then its stdin
//
// $FRAMES is the shared classifier fixture directory, for `cp $FRAMES/<f> tmux/frames/<n>`.
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
		if s.get("load_buffer_fail", "") != "" {
			return "", false
		}
		data, _ := io.ReadAll(stdin)
		n, _ := os.ReadFile(filepath.Join(s.dir, "n.capture"))
		f, err := os.OpenFile(filepath.Join(s.dir, "loads"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return "", false
		}
		_, _ = fmt.Fprintf(f, "%s %s\n", strings.TrimSpace(string(n)), data)
		_ = f.Close()
		return "", os.WriteFile(filepath.Join(s.dir, "buffer"), data, 0o644) == nil
	case "paste-buffer":
		return "", len(args) > 1 && s.get("paste_buffer_fail", "") == ""
	case "delete-buffer", "set-window-option", "select-layout":
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
