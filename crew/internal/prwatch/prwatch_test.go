package prwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

const event = `{"pr":42,"changed":["head_sha"],"state":"open"}`

// The child side of TestStopSignal: this binary, re-execed under the name
// `pr-watch`, announces itself, prints the event and parks forever — so the only
// way the test's Run returns is the signal it forwards.
const (
	childEnv  = "PRWATCH_CHILD"
	markerEnv = "PRWATCH_CHILD_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		_ = os.WriteFile(os.Getenv(markerEnv), []byte("x"), 0o644)
		fmt.Println(event)
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

// tree is the bus the arm writes to; paths is what main.go would hand Run.
type tree struct {
	t     *testing.T
	root  string
	paths bus.Paths
}

func newTree(t *testing.T) tree {
	t.Helper()
	root := t.TempDir()
	tr := tree{t: t, root: root, paths: bus.Paths{
		Dir: filepath.Join(root, ".dispatcher", "crew", "c1"),
		Log: filepath.Join(root, ".dispatcher", "crew", "c1", "events.jsonl"),
	}}
	return tr
}

// rows are the bus rows, parsed.
func (tr tree) rows() []string {
	tr.t.Helper()
	b, err := os.ReadFile(tr.paths.Log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		tr.t.Fatalf("read the bus: %v", err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func fixedClock() clock.Clock {
	return clock.Clock{Now: func() time.Time { return time.Unix(1700000000, 5e8) }}
}

// stub is the park without a binary: it prints out when it starts (the way the
// real child's stdout is captured by `$(…)`), exits Wait with code, and records
// the signals forwarded to it.
type stub struct {
	out  string
	code int
	// wait, when set, holds Wait until the test closes it.
	wait chan struct{}

	mu   sync.Mutex
	sigs []os.Signal
	runs int
}

func (s *stub) Signal(sig os.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sigs = append(s.sigs, sig)
	return nil
}

func (s *stub) seen() []os.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]os.Signal(nil), s.sigs...)
}

func (s *stub) Wait() int {
	if s.wait != nil {
		<-s.wait
	}
	return s.code
}

func park(s *stub) Start {
	return func(_ context.Context, _ []string, stdout, _ io.Writer) (Child, error) {
		s.mu.Lock()
		s.runs++
		s.mu.Unlock()
		_, _ = io.WriteString(stdout, s.out)
		return s, nil
	}
}

func (s *stub) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs
}

func TestNoCrewID(t *testing.T) {
	tr := newTree(t)
	s := &stub{out: event + "\n", code: 0}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"42"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "" }, Clock: fixedClock(), Start: park(s)}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if errOut.String() != "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if s.started() != 0 {
		t.Errorf("the child ran %d times, want never: the arm resolves the crew id first", s.started())
	}
	if len(tr.rows()) != 0 {
		t.Error("a refused park still posted")
	}
}

func TestChildFailure(t *testing.T) {
	tr := newTree(t)
	s := &stub{out: "partial output\n" + event + "\n", code: 3}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"42"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock(), Start: park(s)}); code != 3 {
		t.Fatalf("exit = %d, want the child's 3", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty: set -e skips the post and the print", out.String())
	}
	if len(tr.rows()) != 0 {
		t.Error("a failed park posted")
	}
}

func TestTimeoutMarker(t *testing.T) {
	tr := newTree(t)
	// `$(…)` strips trailing newlines, so a bare newline is the timeout marker.
	s := &stub{out: "\n", code: 0}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"42"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock(), Start: park(s)}); code != 0 {
		t.Fatalf("exit = %d, want 0 (a park that timed out is not a failure)", code)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q, want both empty", out.String(), errOut.String())
	}
	if len(tr.rows()) != 0 {
		t.Error("the timeout marker was posted")
	}
}

func TestEvent(t *testing.T) {
	tr := newTree(t)
	s := &stub{out: event + "\n", code: 0}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"42", "--repo", "o/r"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock(), Start: park(s)}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != event+"\n" {
		t.Errorf("stdout = %q, want the event plus a newline", out.String())
	}
	rows := tr.rows()
	if len(rows) != 1 {
		t.Fatalf("bus has %d rows, want 1", len(rows))
	}
	// The row is value-equal, so compare that way; the body is the event text.
	want := `{"body":"` + strings.ReplaceAll(event, `"`, `\"`) +
		`","crew_id":"c1","from":"pr-watch:42","kind":"msg","to":"dispatcher:c1","ts":1700000000500}`
	if got := testjson.Compact(testjson.MustParse(t, rows[0])); got != want {
		t.Errorf("bus row = %s, want %s", got, want)
	}
}

func TestFromIsTheFirstArgumentEvenWhenAFlag(t *testing.T) {
	tr := newTree(t)
	s := &stub{out: event + "\n", code: 0}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"--interval", "1"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock(), Start: park(s)}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := field(t, tr.rows()[0], "from"); got != "pr-watch:--interval" {
		t.Errorf("from = %q, want pr-watch:--interval (the arm's ${1:-}, verbatim)", got)
	}
}

func TestNoArguments(t *testing.T) {
	tr := newTree(t)
	s := &stub{out: event + "\n", code: 0}
	var out, errOut strings.Builder
	if code := Run(context.Background(), nil, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock(), Start: park(s)}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := field(t, tr.rows()[0], "from"); got != "pr-watch:" {
		t.Errorf("from = %q, want pr-watch:", got)
	}
}

// TestStopSignalForwardsAndPostsNothing is the deviation from the arm in the
// open: the signal is handed to the child and the wait goes on until the child
// is gone, and the event the child had already printed is neither posted nor
// printed.
func TestStopSignalForwardsAndPostsNothing(t *testing.T) {
	tr := newTree(t)
	s := &stub{out: event + "\n", code: 143, wait: make(chan struct{})}
	sigs := make(chan os.Signal, 1)
	var out strings.Builder
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), []string{"42"}, tr.paths, &out, io.Discard,
			Options{CrewID: func() string { return "c1" }, Clock: fixedClock(), Start: park(s), Signals: sigs})
	}()

	sigs <- syscall.SIGTERM
	deadline := time.Now().Add(5 * time.Second)
	for len(s.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.seen(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Fatalf("forwarded %v, want [terminated]", got)
	}
	if len(tr.rows()) != 0 {
		t.Fatal("a stopped park posted")
	}

	select {
	case code := <-done:
		t.Fatalf("exit = %d before the child was gone: the wait must not end early", code)
	default:
	}
	close(s.wait)

	var code int
	select {
	case code = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once the child exited")
	}
	if code != 143 {
		t.Errorf("exit = %d, want the child's 143", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if len(tr.rows()) != 0 {
		t.Error("a stopped park posted")
	}
}

// TestStopSignal is the same contract end to end: a real child, the process's own
// SIGTERM, and no orphan left polling GitHub. The child is this test binary
// re-execed under a name the PATH lookup finds, so the case needs no shell and
// runs in the Nix sandbox.
func TestStopSignal(t *testing.T) {
	tr := newTree(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "pr-watch")
	if err := os.Symlink(os.Args[0], bin); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(childEnv, "1")
	t.Setenv(markerEnv, filepath.Join(dir, "started"))

	done := make(chan int, 1)
	var out strings.Builder
	go func() {
		done <- Run(context.Background(), []string{"42"}, tr.paths, &out, io.Discard,
			Options{CrewID: func() string { return "c1" }, Clock: fixedClock()})
	}()

	waitFor(t, os.Getenv(markerEnv), "the child never started")
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var code int
	select {
	case code = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the park did not stop on TERM — the child was left orphaned")
	}
	// The forwarded TERM is what ended the child: 128+15, the status the arm would
	// have reported for a park killed by the same signal.
	if code != 143 {
		t.Errorf("exit = %d, want the child's 143", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if len(tr.rows()) != 0 {
		t.Error("a stopped park posted")
	}
}

func TestMissingBinary(t *testing.T) {
	tr := newTree(t)
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"42"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock()}); code != 127 {
		t.Fatalf("exit = %d, want 127 (the arm's command not found)", code)
	}
	if len(tr.rows()) != 0 {
		t.Error("a park that never started posted")
	}
}

func TestStartFailure(t *testing.T) {
	tr := newTree(t)
	var out, errOut strings.Builder
	boom := errors.New("cannot fork")
	if code := Run(context.Background(), []string{"42"}, tr.paths, &out, &errOut,
		Options{CrewID: func() string { return "c1" }, Clock: fixedClock(),
			Start: func(context.Context, []string, io.Writer, io.Writer) (Child, error) {
				return nil, boom
			}}); code != 127 {
		t.Fatalf("exit = %d, want 127", code)
	}
	if !strings.Contains(errOut.String(), "cannot fork") {
		t.Errorf("stderr = %q, want the reason", errOut.String())
	}
	if len(tr.rows()) != 0 {
		t.Error("a park that never started posted")
	}
}

// field is one string field of a bus row.
func field(t *testing.T, row, key string) string {
	t.Helper()
	v, ok := testjson.MustParse(t, row).Get(key)
	if !ok {
		t.Fatalf("row %s has no %q", row, key)
	}
	s, _ := v.AsString()
	return s
}

func waitFor(t *testing.T, path, why string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(why)
}
