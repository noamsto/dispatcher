package rolewatch

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/spill"
)

// fakeTmux answers reads from a table, keeps a pane alive for paneIDs
// `#{pane_id}` reads, serves captures in order (the last repeats) and records
// every argv joined by spaces.
type fakeTmux struct {
	reads    map[string]string
	paneIDs  int
	captures []string
	calls    []string
	inputs   []string
	onRead   func(line string) bool // true answers empty, as a cancelled exec would
}

func (f *fakeTmux) Run(_ context.Context, args ...string) (string, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if f.onRead != nil && f.onRead(line) {
		return "", nil
	}
	switch {
	case line == "display-message -p -t %6 #{pane_id}":
		if f.paneIDs == 0 {
			return "", errors.New("can't find pane")
		}
		f.paneIDs--
		return "%6", nil
	case args[0] == "capture-pane":
		out := f.captures[0]
		if len(f.captures) > 1 {
			f.captures = f.captures[1:]
		}
		return out, nil
	}
	return f.reads[line], nil
}

func (f *fakeTmux) RunInput(ctx context.Context, input string, args ...string) (string, error) {
	f.inputs = append(f.inputs, input)
	return f.Run(ctx, args...)
}

const (
	readStartStamp = "display-message -p -t %6 #{@crew_role}|#{window_id}|#{@crew_delivery}"
	readStamp      = "display-message -p -t %6 #{@crew_role}|#{window_id}"
	readBranch     = "show-options -wqv -t @3 @crew_branch"
	readCrew       = "show-options -wqv -t @3 @crew_id"
	readExited     = "display-message -p -t %6 #{@crew_exited}"
)

func stampedPane() map[string]string {
	return map[string]string{
		readStartStamp: "reviewer|@3|",
		readStamp:      "reviewer|@3",
		readBranch:     "feat/9-x",
		readCrew:       "c1",
	}
}

func frameFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "frame", "testdata", "frames", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

func TestClockSeam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clock")
	c := NewClock(func(k string) string {
		if k == "DISPATCH_ROLE_WATCH_CLOCK" {
			return path
		}
		return ""
	})
	c.Now = func() time.Time { return time.Unix(1700000000, 0) }
	if got := c.Seconds(); got != 1700000000 {
		t.Fatalf("seeded Seconds = %d", got)
	}
	readClock := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := readClock(); got != "1700000000" {
		t.Fatalf("seeded file = %q", got)
	}
	if err := os.WriteFile(path, []byte("1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.Seconds(); got != 1000 {
		t.Fatalf("Seconds = %d, want 1000", got)
	}
	for _, step := range []struct{ interval, want string }{{"0.2", "1001"}, {"2", "1003"}} {
		if err := c.SleepCtx(context.Background(), step.interval, os.Stderr); err != nil {
			t.Fatal(err)
		}
		if got := readClock(); got != step.want {
			t.Fatalf("after sleep %s: clock = %q, want %s", step.interval, got, step.want)
		}
	}

	wall := NewClock(func(string) string { return "" })
	if wall.CrewClock != "" {
		t.Fatalf("unset env gave CrewClock %q", wall.CrewClock)
	}
	if d := wall.Seconds() - time.Now().Unix(); d < -1 || d > 1 {
		t.Fatalf("wall Seconds off by %d", d)
	}
}

func TestStamped(t *testing.T) {
	tests := []struct {
		name, key, value string
		want             bool
	}{
		{"all match", "", "", true},
		{"role re-stamped", readStamp, "plan-critic|@3", false},
		{"branch re-stamped", readBranch, "feat/8-other", false},
		{"crew re-stamped", readCrew, "c2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := stampedPane()
			if tt.key != "" {
				reads[tt.key] = tt.value
			}
			w := &watcher{ctx: context.Background(), o: Options{Tmux: &fakeTmux{reads: reads}},
				role: "reviewer", pane: "%6", branch: "feat/9-x", wCrew: "c1"}
			if got := w.stamped(); got != tt.want {
				t.Fatalf("stamped() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartupRefusals(t *testing.T) {
	ok := Request{Role: "reviewer", Pane: "%6", Branch: "feat/9-x", Engine: "claude", Interval: "2", DeferNotice: "60", SubmitRetries: "3"}
	tests := []struct {
		name  string
		edit  func(*Request)
		reads map[string]string
		want  string
		code  int // 0 is 1
		opts  func(*Options)
	}{
		{"no role", func(r *Request) { r.Role = "" }, nil, "dispatch: --role-watch needs a role name", 0, nil},
		{"no pane", func(r *Request) { r.Pane = "" }, nil, "dispatch: --role-watch needs --pane <id>", 0, nil},
		{"submit retries", func(r *Request) { r.SubmitRetries = "-1" }, nil,
			"dispatch: --role-watch: --submit-retries must be a non-negative integer, got -1", 0, nil},
		{"interval", func(r *Request) { r.Interval = "1s" }, nil,
			"dispatch: --role-watch: --interval must be a non-negative number, got 1s", 0, nil},
		{"defer notice", func(r *Request) { r.DeferNotice = "0.5" }, nil,
			"dispatch: --role-watch: --defer-notice must be a non-negative integer, got 0.5", 0, nil},
		{"pane id", func(r *Request) { r.Pane = "6" }, nil,
			"dispatch: --role-watch: --pane must be a pane id (%N), got 6", 0, nil},
		{"pane id punctuation", func(r *Request) { r.Pane = "@1.{bottom-right}" }, nil,
			"dispatch: --role-watch: --pane must be a pane id (%N), got @1.{bottom-right}", 0, nil},
		{"lead", func(r *Request) { r.Role = "lead" }, nil, "dispatch: --role-watch: role 'lead' is never watched", 0, nil},
		{"F22 lead pane", nil, map[string]string{readStartStamp: "lead|@3|"},
			"dispatch: --role-watch: pane %6's @crew_role (lead) does not match --role reviewer", 0, nil},
		{"F22 role", nil, map[string]string{readStartStamp: "plan-critic|@3|"},
			"dispatch: --role-watch: pane %6's @crew_role (plan-critic) does not match --role reviewer", 0, nil},
		{"F22 branch", nil, map[string]string{readBranch: "feat/8-other"},
			"dispatch: --role-watch: window @3's @crew_branch (feat/8-other) does not match --branch feat/9-x", 0, nil},
		{name: "git branch fails", edit: func(r *Request) { r.Branch = "" },
			want: "dispatch: --role-watch: cannot read the current branch: git: not a repository", code: 128,
			opts: func(o *Options) {
				o.CurrentBranch = func() (string, error) { return "", errors.New("git: not a repository") }
			}},
		{name: "no bus", want: "dispatch: --role-watch: no crew bus: not in a git repository", code: 128,
			opts: func(o *Options) { o.LogErr = errors.New("not a repo") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ok
			if tt.edit != nil {
				tt.edit(&req)
			}
			reads := stampedPane()
			for k, v := range tt.reads {
				reads[k] = v
			}
			tm := &fakeTmux{reads: reads, paneIDs: 1}
			var stderr strings.Builder
			opts := Options{
				Tmux: tm, Stderr: &stderr, CrewID: func() string { return "" },
				CurrentBranch: func() (string, error) { return "feat/9-x", nil },
			}
			if tt.opts != nil {
				tt.opts(&opts)
			}
			wantCode := cmp.Or(tt.code, 1)
			code := Run(context.Background(), req, opts)
			if code != wantCode || stderr.String() != tt.want+"\n" {
				t.Fatalf("exit %d, stderr %q; want %d, %q", code, stderr.String(), wantCode, tt.want)
			}
			if i := slices.IndexFunc(tm.calls, func(c string) bool { return strings.HasPrefix(c, "set-option") }); i >= 0 {
				t.Fatalf("refusal wrote tmux: %v", tm.calls)
			}
		})
	}
}

func TestDeliverIdleClaude(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	row := `{"kind":"msg","ts":9999999999999,"from":"worker:feat/9-x#s1-1","to":"role:feat/9-x:reviewer","body":"go"}` + "\n"
	if err := os.WriteFile(log, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	clockFile := filepath.Join(dir, "clock")
	tm := &fakeTmux{
		reads:    stampedPane(),
		paneIDs:  2,
		captures: []string{frameFile(t, "rw_idle.txt"), frameFile(t, "rw_claude_held.txt")},
	}
	var posts []string
	code := Run(context.Background(),
		Request{Role: "reviewer", Pane: "%6", Branch: "feat/9-x", Engine: "claude", Interval: "0.2", DeferNotice: "60", SubmitRetries: "3"},
		Options{
			Tmux:   tm,
			Clock:  NewClock(func(string) string { return clockFile }),
			Stderr: os.Stderr,
			Log:    log,
			Post: func(_ context.Context, crew, from, to, body string) {
				posts = append(posts, to+" "+body)
			},
			CrewID: func() string { return "" },
			PID:    42,
			Rand:   func() int { return 7 },
		})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := []string{
		readStartStamp, readBranch, readCrew, readExited, "set-option -p -t %6 @crew_state idle",
		"display-message -p -t %6 #{pane_id}", readExited,
		readExited, "set-option -p -t %6 @crew_state working",
		readExited, readStamp, readBranch, readCrew,
		"capture-pane -e -p -t %6",
		"load-buffer -b rw-assign-42-7 -",
		"paste-buffer -p -d -r -b rw-assign-42-7 -t %6",
		"capture-pane -e -p -t %6",
		"send-keys -t %6 Enter",
		"display-message -p -t %6 #{pane_id}", readExited,
		"display-message -p -t %6 #{pane_id}",
	}
	if !slices.Equal(tm.calls, want) {
		t.Fatalf("argv log:\n%s\nwant:\n%s", strings.Join(tm.calls, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(tm.inputs, []string{"Assignment: go"}) {
		t.Fatalf("load-buffer input %q", tm.inputs)
	}
	if len(posts) != 0 {
		t.Fatalf("unexpected posts %q", posts)
	}
}

// spilledWatcher is a typed-delivery watcher on crew c1 whose bus lives in dir,
// with the posts it makes collected as "to body".
func spilledWatcher(dir string, posts *[]string) *watcher {
	return &watcher{
		ctx:  context.Background(),
		role: "reviewer", pane: "%6", branch: "feat/9-x", wCrew: "c1", crew: "c1",
		roleID: "role:feat/9-x:reviewer", delivery: "typed",
		o: Options{
			Tmux: &fakeTmux{reads: stampedPane()},
			Log:  filepath.Join(dir, "events.jsonl"),
			Post: func(_ context.Context, _, _, to, body string) { *posts = append(*posts, to+" "+body) },
		},
	}
}

const spillFrom = "worker:feat/9-x#s1-1"

func TestEnqueueExpandsASpilledBodyFromTheWatchersOwnCrew(t *testing.T) {
	dir := t.TempDir()
	paths := bus.Paths{Dir: dir}
	full := strings.Repeat("the whole assignment ", 400)
	// The row names another crew; reading that crew's directory would be a leak.
	rel, err := spill.Write(paths.CrewDir("c1"), 1, full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spill.Write(paths.CrewDir("other"), 1, "someone else's body"); err != nil {
		t.Fatal(err)
	}
	var posts []string
	w := spilledWatcher(dir, &posts)
	w.enqueue(map[string]any{"from": spillFrom, "to": w.roleID, "crew_id": "other", "body": "head", "body_file": rel}, 5)
	if len(w.pending) != 1 || w.pending[0].body != full {
		t.Fatalf("pending = %+v, want the %d-byte body from crew c1", w.pending, len(full))
	}
	if len(posts) != 0 {
		t.Fatalf("unexpected posts %q", posts)
	}
}

// A window with no @crew_id stamp leaves wCrew empty; the watcher's crew is then
// the checkout's, where the sender wrote the spill.
func TestEnqueueExpandsWithTheFallbackCrewWhenTheWindowHasNoStamp(t *testing.T) {
	dir := t.TempDir()
	full := strings.Repeat("the whole assignment ", 400)
	rel, err := spill.Write(bus.Paths{Dir: dir}.CrewDir("c1"), 1, full)
	if err != nil {
		t.Fatal(err)
	}
	var posts []string
	w := spilledWatcher(dir, &posts)
	w.wCrew = ""
	w.enqueue(map[string]any{"from": spillFrom, "to": w.roleID, "body": "head", "body_file": rel}, 5)
	if len(w.pending) != 1 || w.pending[0].body != full || len(posts) != 0 {
		t.Fatalf("pending = %+v, posts = %q, want the whole body and no notice", w.pending, posts)
	}
}

func TestEnqueueDeliversHeadAndNoteWhenExpansionFailsAndTellsTheLead(t *testing.T) {
	dir := t.TempDir()
	var posts []string
	w := spilledWatcher(dir, &posts)
	w.enqueue(map[string]any{"from": spillFrom, "to": w.roleID, "crew_id": "c1", "body": "head", "body_file": "msg-bodies/gone.txt"}, 5)
	if len(w.pending) != 1 || !strings.HasPrefix(w.pending[0].body, "head [full body unavailable") ||
		!strings.Contains(w.pending[0].body, "body is the head only]") || !AssignmentSafe(w.pending[0].body) {
		t.Fatalf("pending = %+v, want the head plus a typeable body_note", w.pending)
	}
	if len(posts) != 1 || !strings.HasPrefix(posts[0], spillFrom+` {"role":"reviewer","event":"assignment_truncated"`) {
		t.Fatalf("posts = %q, want one assignment_truncated notice to the lead", posts)
	}
}

func TestDeliverCancelledDuringStampSkipsOrphanNotice(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	row := `{"kind":"msg","ts":9999999999999,"from":"worker:feat/9-x#s1-1","to":"role:feat/9-x:reviewer","body":"go"}` + "\n"
	if err := os.WriteFile(log, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := &fakeTmux{
		reads:    stampedPane(),
		paneIDs:  5,
		captures: []string{frameFile(t, "rw_idle.txt")},
		onRead: func(line string) bool {
			if line != readStamp {
				return false
			}
			cancel()
			return true
		},
	}
	posts := 0
	code := Run(ctx,
		Request{Role: "reviewer", Pane: "%6", Branch: "feat/9-x", Engine: "claude", Interval: "0.2", DeferNotice: "60", SubmitRetries: "3"},
		Options{
			Tmux:   tm,
			Clock:  NewClock(func(string) string { return filepath.Join(dir, "clock") }),
			Stderr: os.Stderr,
			Log:    log,
			Post:   func(context.Context, string, string, string, string) { posts++ },
			CrewID: func() string { return "" },
			PID:    42,
			Rand:   func() int { return 7 },
		})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if posts != 0 {
		t.Fatalf("posted %d orphan notices after cancel", posts)
	}
}
