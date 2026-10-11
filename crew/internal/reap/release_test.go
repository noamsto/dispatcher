package reap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

const (
	branch = "feat/x"
	// statusTS is the status row's ts in ms; fakeNow is "now" for the probes.
	statusTS = int64(1_700_000_000_000)
	fakeNow  = int64(1_800_000_000)
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func idleFrame(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../frame/testdata/frames/done_idle.txt")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimRight(string(b), "\n")
	if why, idle := frame.PaneIdleReason(text, text, false); !idle {
		t.Fatalf("fixture is not an idle claude frame: %s", why)
	}
	return text
}

// rel is one release scenario: a real repo and linked worktree for the
// worktree lookup, and fake tmux, captures, clock and bus.
type rel struct {
	t        *testing.T
	common   string
	wt       string
	log      string
	wins     string // list-windows (occupants)
	panes    string // list-panes
	wlist    string // list-windows (activity)
	plain    map[string][]string
	colored  string
	mu       sync.Mutex
	calls    []string
	sleeps   []time.Duration
	appended []string
	appendEr error
	says     []string
	notes    []string
	capSeen  map[string]int
}

func newRel(t *testing.T) *rel {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("CREW_RELEASE_GAP", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "main")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, main, "init", "-q", "-b", "main")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, "wt")
	git(t, main, "worktree", "add", "-q", "-b", branch, wt)

	idle := idleFrame(t)
	r := &rel{
		t:       t,
		common:  filepath.Join(main, ".git"),
		wt:      wt,
		log:     filepath.Join(root, "bus.log"),
		wins:    "@5\tworker\t" + wt,
		panes:   "@5\t%5\tclaude",
		plain:   map[string][]string{"%5": {idle}},
		colored: idle,
		capSeen: map[string]int{},
	}
	// The claude config dir is empty unless a test plants a transcript.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	return r
}

func (r *rel) probes() Probes {
	return Probes{
		Tmux: func(_ context.Context, args ...string) string {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "tmux "+strings.Join(args, " "))
			switch args[0] {
			case "list-windows":
				if strings.Contains(args[3], "window_active") {
					return r.wlist
				}
				return r.wins
			case "list-panes":
				return r.panes
			}
			return ""
		},
		TmuxRun: func(_ context.Context, args ...string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "tmux "+strings.Join(args, " "))
		},
		Capture: func(_ context.Context, pane string, colored bool) string {
			r.mu.Lock()
			defer r.mu.Unlock()
			kind := "plain"
			if colored {
				kind = "colored"
			}
			r.calls = append(r.calls, "capture "+kind+" "+pane)
			if colored {
				return r.colored
			}
			seq := r.plain[pane]
			if len(seq) == 0 {
				return ""
			}
			i := min(r.capSeen[pane], len(seq)-1)
			r.capSeen[pane]++
			return seq[i]
		},
		Sleep: func(d time.Duration) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "sleep")
			r.sleeps = append(r.sleeps, d)
		},
		Now: func() time.Time { return time.Unix(fakeNow, 123_000_000) },
		Append: func(path, line string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.appendEr != nil {
				return r.appendEr
			}
			r.appended = append(r.appended, path+"\t"+line)
			return nil
		},
	}
}

func (r *rel) release(state string, grace int64, dry bool) (int, error) {
	r.t.Helper()
	d := Deps{
		Common: r.common, Log: r.log, P: r.probes(),
		Say:  func(s string) { r.says = append(r.says, s) },
		Note: func(s string) { r.notes = append(r.notes, s) },
	}
	return ReleaseWindows(context.Background(), d, branch, "sess-1", state, statusTS, grace, dry)
}

func (r *rel) kills() []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "tmux kill-window") {
			out = append(out, c)
		}
	}
	return out
}

func (r *rel) wantKept(rc int, err error, note string) {
	r.t.Helper()
	if err != nil || rc != 3 {
		r.t.Fatalf("rc=%d err=%v, want 3", rc, err)
	}
	if !slices.Equal(r.notes, []string{note}) {
		r.t.Fatalf("notes %q, want %q", r.notes, note)
	}
	if len(r.kills()) != 0 || len(r.appended) != 0 || len(r.says) != 0 {
		r.t.Fatalf("kept but acted: calls %q appended %q says %q", r.calls, r.appended, r.says)
	}
}

func TestReleaseNoWorktreeDoesNothing(t *testing.T) {
	r := newRel(t)
	d := Deps{Common: r.common, Log: r.log, P: r.probes(), Say: func(string) {}, Note: func(string) {}}
	rc, err := ReleaseWindows(context.Background(), d, "feat/unknown", "s", "done", statusTS, 300, false)
	if rc != 0 || err != nil || len(r.calls) != 0 {
		t.Fatalf("rc=%d err=%v calls=%q", rc, err, r.calls)
	}
}

func TestReleaseWorktreeDirGoneDoesNothing(t *testing.T) {
	r := newRel(t)
	if err := os.RemoveAll(r.wt); err != nil {
		t.Fatal(err)
	}
	rc, err := r.release("done", 300, false)
	if rc != 0 || err != nil || len(r.calls) != 0 {
		t.Fatalf("rc=%d err=%v calls=%q", rc, err, r.calls)
	}
}

func TestReleaseNoOccupantsDoesNothing(t *testing.T) {
	r := newRel(t)
	r.wins = "@5\tworker\t/elsewhere"
	rc, err := r.release("done", 300, false)
	if rc != 0 || err != nil || len(r.kills()) != 0 || len(r.appended) != 0 {
		t.Fatalf("rc=%d err=%v calls=%q", rc, err, r.calls)
	}
}

func TestReleaseExitedWithEngineKeeps(t *testing.T) {
	r := newRel(t)
	rc, err := r.release("exited", 300, false)
	if rc != 0 || err != nil {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	if !slices.Equal(r.notes, []string{"keeping feat/x — exited but an engine is still running there"}) {
		t.Fatalf("notes %q", r.notes)
	}
	if len(r.kills()) != 0 || len(r.appended) != 0 {
		t.Fatalf("acted: %q %q", r.calls, r.appended)
	}
}

func TestReleaseExitedWithoutEngineReleases(t *testing.T) {
	r := newRel(t)
	r.panes = "@5\t%5\tfish"
	rc, err := r.release("exited", 300, false)
	if rc != 0 || err != nil || len(r.kills()) != 1 {
		t.Fatalf("rc=%d err=%v calls=%q", rc, err, r.calls)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "capture") || c == "sleep" {
			t.Fatalf("sampled a pane with no engine: %q", r.calls)
		}
	}
}

func TestReleaseAttachedVisibleKeeps(t *testing.T) {
	r := newRel(t)
	r.wlist = "@9\t1\t1\t0\n@5\t1\t2\t0"
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — human present: @5 is visible in an attached tmux client")
	wantCall := "tmux list-windows -a -F #{window_id}\t#{window_active}\t#{session_attached}\t#{window_activity}"
	if !slices.Contains(r.calls, wantCall) {
		t.Fatalf("calls %q lack %q", r.calls, wantCall)
	}
}

func TestReleaseVisibleNeedsActiveAndAttached(t *testing.T) {
	for _, row := range []string{"@5\t0\t3\t0", "@5\t1\t0\t0", "@5\t1\t\t0", "@5\t1\tx\t0", "@9\t1\t1\t0"} {
		r := newRel(t)
		r.wlist = row
		if rc, err := r.release("done", 300, false); rc != 0 || err != nil || len(r.kills()) != 1 {
			t.Fatalf("%q: rc=%d err=%v calls=%q", row, rc, err, r.calls)
		}
	}
}

func TestReleaseRecentActivityKeeps(t *testing.T) {
	r := newRel(t)
	r.wlist = fmt.Sprintf("@5\t0\t0\t%d", fakeNow-10)
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — human present: @5 had activity within the grace")
}

func TestReleaseOldActivityReleases(t *testing.T) {
	r := newRel(t)
	r.wlist = fmt.Sprintf("@5\t0\t0\t%d\n@6\t0\t0\t%d", fakeNow-1000, fakeNow)
	rc, err := r.release("done", 300, false)
	if rc != 0 || err != nil || len(r.kills()) != 1 {
		t.Fatalf("rc=%d err=%v calls=%q", rc, err, r.calls)
	}
}

func plantTranscript(t *testing.T, wt string, files map[string][]string, mtimes map[string]time.Time) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects",
		strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return '-'
		}, wt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, lines := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if mt, ok := mtimes[name]; ok {
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func stamp(deltaMS int64) string {
	return time.UnixMilli(statusTS + deltaMS).UTC().Format("2006-01-02T15:04:05.000Z")
}

func TestReleaseTranscriptUserTurn(t *testing.T) {
	text := func(s string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":%q}}`, stamp(5000), s)
	}
	cases := []struct {
		name string
		line string
		keep bool
	}{
		{"newer typed turn", text("hello"), true},
		{"newer block text", fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"text","text":"hi"}]}}`, stamp(5000)), true},
		{"older turn", fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":"hello"}}`, stamp(-5000)), false},
		{"tool result", fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","content":"x"}]}}`, stamp(5000)), false},
		{"isMeta", fmt.Sprintf(`{"type":"user","isMeta":true,"timestamp":%q,"message":{"content":"hello"}}`, stamp(5000)), false},
		{"task notification", text("<task-notification>done</task-notification>"), false},
		{"indented bash-stdout", text("  <bash-stdout>x</bash-stdout>"), false},
		{"assistant row", fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":"hello"}}`, stamp(5000)), false},
		{"no timestamp", `{"type":"user","message":{"content":"hello"}}`, false},
		{"empty text", text(""), false},
		// jq -R ran the filter per line: an erroring row was reported and
		// skipped, so a later typed turn still keeps the window.
		{"numeric timestamp row before a typed turn", `{"type":"user","timestamp":5,"message":{"content":"hello"}}` + "\n" + text("hello"), true},
		{"string message row before a typed turn", fmt.Sprintf(`{"type":"user","timestamp":%q,"message":"hello"}`, stamp(5000)) + "\n" + text("hello"), true},
		{"erroring row alone", `{"type":"user","timestamp":5,"message":{"content":"hello"}}`, false},
		{"line over 64 KiB before a typed turn", fmt.Sprintf(`{"type":"assistant","message":{"content":%q}}`, strings.Repeat("a", 100<<10)) + "\n" + text("hello"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRel(t)
			plantTranscript(t, r.wt, map[string][]string{"a.jsonl": {"not json", c.line, ""}}, nil)
			rc, err := r.release("done", 300, false)
			if c.keep {
				r.wantKept(rc, err, "keeping feat/x — human present: the engine transcript has a user turn newer than the status")
				return
			}
			if rc != 0 || err != nil || len(r.kills()) != 1 {
				t.Fatalf("rc=%d err=%v notes=%q", rc, err, r.notes)
			}
		})
	}
}

func TestReleaseTranscriptNewestFileWins(t *testing.T) {
	r := newRel(t)
	newer := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":"hi"}}`, stamp(5000))
	older := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":"hi"}}`, stamp(-5000))
	now := time.Now()
	plantTranscript(t, r.wt,
		map[string][]string{"old.jsonl": {newer}, "new.jsonl": {older}},
		map[string]time.Time{"old.jsonl": now.Add(-time.Hour), "new.jsonl": now})
	if rc, err := r.release("done", 300, false); rc != 0 || err != nil || len(r.kills()) != 1 {
		t.Fatalf("rc=%d err=%v notes=%q", rc, err, r.notes)
	}
}

func TestReleasePaneChangesBetweenSamplesKeeps(t *testing.T) {
	r := newRel(t)
	idle := r.plain["%5"][0]
	r.plain["%5"] = []string{idle, idle + "\nmore output"}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %5 is still changing")
	if !slices.Equal(r.sleeps, []time.Duration{time.Second}) {
		t.Fatalf("sleeps %v", r.sleeps)
	}
}

func TestReleaseCaptureOrder(t *testing.T) {
	r := newRel(t)
	r.wins += "\n@6\tworker2\t" + r.wt
	r.panes += "\n@6\t%6\tclaude"
	r.plain["%6"] = r.plain["%5"]
	if rc, err := r.release("done", 300, false); rc != 0 || err != nil {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	var got []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "capture") || c == "sleep" {
			got = append(got, c)
		}
	}
	want := []string{"capture plain %5", "capture colored %5", "capture plain %6", "capture colored %6", "sleep", "capture plain %5", "capture plain %6"}
	if !slices.Equal(got, want) {
		t.Fatalf("order:\n got %q\nwant %q", got, want)
	}
}

func TestReleaseUnreadablePaneKeeps(t *testing.T) {
	r := newRel(t)
	r.plain["%5"] = []string{""}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %5 is unreadable")
	if len(r.sleeps) != 0 {
		t.Fatalf("slept before the unreadable verdict: %v", r.sleeps)
	}
}

func TestReleaseClaudeNotIdleKeeps(t *testing.T) {
	r := newRel(t)
	r.plain["%5"] = []string{"✻ Working… (3s · ↓ 1.2k tokens)"}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %5 is not idle (live turn, unsent input, or no idle input box)")
}

func TestReleaseNixWrappedClaudeUsesIdleCheck(t *testing.T) {
	r := newRel(t)
	r.panes = "@5\t%5\t.claude-wrapped"
	r.plain["%5"] = []string{"just some text"}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %5 is not idle (live turn, unsent input, or no idle input box)")
}

func TestReleaseNonClaudePromptKeeps(t *testing.T) {
	r := newRel(t)
	r.panes = "@5\t%5\tpi"
	r.plain["%5"] = []string{"Pick one\n 1. a\n 2. b\nEnter to select"}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %5 shows a prompt")
}

func TestReleaseNonClaudeStablePaneReleases(t *testing.T) {
	r := newRel(t)
	r.panes = "@5\t%5\tcodex"
	r.plain["%5"] = []string{"working on it"}
	if rc, err := r.release("done", 300, false); rc != 0 || err != nil || len(r.kills()) != 1 {
		t.Fatalf("rc=%d err=%v notes=%q", rc, err, r.notes)
	}
}

func TestReleaseWritesRowAndKills(t *testing.T) {
	r := newRel(t)
	r.wins += "\n@6\tworker2\t" + r.wt
	r.panes = "@5\t%5\tfish\n@6\t%6\tfish"
	rc, err := r.release("done", 300, false)
	if rc != 0 || err != nil {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	if want := []string{"tmux kill-window -t @5", "tmux kill-window -t @6"}; !slices.Equal(r.kills(), want) {
		t.Fatalf("kills %q", r.kills())
	}
	wantSays := []string{
		fmt.Sprintf("released @5 at %s (feat/x done)", r.wt),
		fmt.Sprintf("released @6 at %s (feat/x done)", r.wt),
	}
	if !slices.Equal(r.says, wantSays) || len(r.notes) != 0 {
		t.Fatalf("says %q notes %q", r.says, r.notes)
	}
	if len(r.appended) != 1 {
		t.Fatalf("appended %q", r.appended)
	}
	path, line, _ := strings.Cut(r.appended[0], "\t")
	if path != r.log {
		t.Fatalf("log %q", path)
	}
	got, err := jsonv.ParseOne(line)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := jsonv.ParseOne(fmt.Sprintf(
		`{"ts":%d,"kind":"release","branch":"feat/x","session":"sess-1","state":"done","windows":["@5","@6"]}`,
		fakeNow*1000+123))
	if g, w := string(jsonv.Append(nil, got, jsonv.Options{})), string(jsonv.Append(nil, want, jsonv.Options{})); g != w {
		t.Fatalf("row:\n got %s\nwant %s", g, w)
	}
	if strings.ContainsAny(line, "\n ") {
		t.Fatalf("row is not compact: %q", line)
	}
}

func TestReleaseDryRunTouchesNothing(t *testing.T) {
	r := newRel(t)
	rc, err := r.release("failed", 300, true)
	if rc != 0 || err != nil {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	if len(r.kills()) != 0 || len(r.appended) != 0 {
		t.Fatalf("dry run acted: %q %q", r.calls, r.appended)
	}
	if want := []string{fmt.Sprintf("would release @5 at %s (feat/x failed)", r.wt)}; !slices.Equal(r.says, want) {
		t.Fatalf("says %q", r.says)
	}
}

func TestReleaseAppendErrorIsReturned(t *testing.T) {
	r := newRel(t)
	r.appendEr = errors.New("disk full")
	rc, err := r.release("done", 300, false)
	if rc != 0 || err == nil || err.Error() != "disk full" {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	if len(r.kills()) != 1 {
		t.Fatalf("window not killed before the row write: %q", r.calls)
	}
}

func TestReleaseGap(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", time.Second},
		{"0", 0},
		{"0.25", 250 * time.Millisecond},
		{"2", 2 * time.Second},
		{"abc", time.Second},
		{"-1", time.Second},
		{"nan", time.Second},
	} {
		r := newRel(t)
		t.Setenv("CREW_RELEASE_GAP", c.env)
		if rc, err := r.release("done", 300, false); rc != 0 || err != nil {
			t.Fatalf("%q: rc=%d err=%v", c.env, rc, err)
		}
		if !slices.Equal(r.sleeps, []time.Duration{c.want}) {
			t.Fatalf("%q: sleeps %v, want %v", c.env, r.sleeps, c.want)
		}
	}
}
