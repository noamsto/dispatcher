package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/dash/internal/buswatch"
	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

const (
	testDebounce = 20 * time.Millisecond
	readTimeout  = 5 * time.Second
)

// startWatch runs runWatch on a pipe and returns its output lines. Cleanup
// cancels the loop, closes the pipe and waits for runWatch to return.
func startWatch(t *testing.T, changes <-chan struct{}, collect func(context.Context) (any, error), debounce time.Duration) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- runWatch(ctx, pw, changes, collect, debounce) }()

	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = pr.Close()
		select {
		case <-done:
		case <-time.After(readTimeout):
			t.Error("runWatch did not return after cancel")
		}
	})
	return lines
}

func readLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("watch output closed")
		}
		return l
	case <-time.After(readTimeout):
		t.Fatal("timed out waiting for a watch line")
	}
	return ""
}

func send(t *testing.T, changes chan<- struct{}) {
	t.Helper()
	select {
	case changes <- struct{}{}:
	case <-time.After(readTimeout):
		t.Fatal("timed out sending a change")
	}
}

func counter() (func(context.Context) (any, error), *atomic.Int64) {
	var n atomic.Int64
	return func(context.Context) (any, error) {
		return map[string]int64{"n": n.Add(1)}, nil
	}, &n
}

func decodeN(t *testing.T, line string) int64 {
	t.Helper()
	var v struct{ N int64 }
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("line %q is not JSON: %v", line, err)
	}
	return v.N
}

func TestWatchOneLinePerChange(t *testing.T) {
	changes := make(chan struct{})
	collect, _ := counter()
	lines := startWatch(t, changes, collect, testDebounce)

	if n := decodeN(t, readLine(t, lines)); n != 1 {
		t.Fatalf("start line n = %d, want 1", n)
	}
	for want := int64(2); want <= 4; want++ {
		send(t, changes)
		if n := decodeN(t, readLine(t, lines)); n != want {
			t.Fatalf("line n = %d, want %d", n, want)
		}
	}
}

func TestWatchDebounceAbsorbsBurst(t *testing.T) {
	// Wide enough that three back-to-back sends always land in one window.
	const debounce = 200 * time.Millisecond
	changes := make(chan struct{})
	collect, calls := counter()
	lines := startWatch(t, changes, collect, debounce)

	readLine(t, lines)
	for range 3 {
		send(t, changes)
	}
	if n := decodeN(t, readLine(t, lines)); n != 2 {
		t.Fatalf("line n = %d, want 2", n)
	}
	time.Sleep(2 * debounce)
	if got := calls.Load(); got != 2 {
		t.Errorf("collects = %d, want 2 (a burst inside one debounce is one change)", got)
	}
}

func TestWatchCoalescesDuringCollect(t *testing.T) {
	changes := make(chan struct{})
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int64
	collect := func(context.Context) (any, error) {
		n := calls.Add(1)
		if n == 2 {
			started <- struct{}{}
			<-gate
		}
		return map[string]int64{"n": n}, nil
	}
	lines := startWatch(t, changes, collect, testDebounce)

	readLine(t, lines)
	send(t, changes)
	select {
	case <-started:
	case <-time.After(readTimeout):
		t.Fatal("second collect never started")
	}
	for range 3 {
		send(t, changes)
	}
	close(gate)

	if n := decodeN(t, readLine(t, lines)); n != 2 {
		t.Fatalf("line n = %d, want 2", n)
	}
	if n := decodeN(t, readLine(t, lines)); n != 3 {
		t.Fatalf("follow-up line n = %d, want 3", n)
	}
	time.Sleep(5 * testDebounce)
	if got := calls.Load(); got != 3 {
		t.Errorf("collects = %d, want 3 (changes during a collect coalesce into one follow-up)", got)
	}
}

func TestWatchReturnsOnWriteError(t *testing.T) {
	pr, pw := io.Pipe()
	_ = pr.Close()
	collect, _ := counter()
	done := make(chan error, 1)
	go func() { done <- runWatch(context.Background(), pw, nil, collect, testDebounce) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("runWatch returned nil on a closed output")
		}
	case <-time.After(readTimeout):
		t.Fatal("runWatch kept running after its output closed")
	}
}

func appendEvent(t *testing.T, path string, ts int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"ts":` + strconv.FormatInt(ts, 10) + `,"crew_id":"c1","from":"worker:feat/a#s1","to":"dispatcher:c1","kind":"status","body":{"state":"working","detail":"gate"}}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func eventCount(t *testing.T, line string) int {
	t.Helper()
	var v struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("line is not JSON: %v\n%s", err, line)
	}
	return len(v.Events)
}

func TestWatchFixtureBus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	bus, err := os.ReadFile("internal/view/testdata/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bus, 0o644); err != nil {
		t.Fatal(err)
	}

	w := buswatch.New(path)
	t.Cleanup(func() { _ = w.Close() })
	in := viewtest.Inputs()
	scanner := data.NewBusScanner(path)
	collect := func(context.Context) (any, error) {
		d, err := scanner.Scan(viewtest.NowMS)
		if err != nil {
			return nil, err
		}
		in.Bus = d
		return view.Build("overview", in)
	}
	lines := startWatch(t, w.Events(), collect, testDebounce)

	first := eventCount(t, readLine(t, lines))
	appendEvent(t, path, viewtest.NowMS-1000)
	if got := eventCount(t, readLine(t, lines)); got != first+1 {
		t.Fatalf("events after one append = %d, want %d", got, first+1)
	}
	appendEvent(t, path, viewtest.NowMS-500)
	if got := eventCount(t, readLine(t, lines)); got != first+2 {
		t.Fatalf("events after a second append = %d, want %d", got, first+2)
	}
}

func TestWatchRelayCursor(t *testing.T) {
	common := t.TempDir()
	relay := filepath.Join(common, "crew", "crews", "c1", "relay")
	if err := os.MkdirAll(relay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(common, "crew", "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(relay, "h.cursor")
	if err := os.WriteFile(cursor, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := buswatch.Merge(buswatch.NewPoll(func() []string { return watchPaths([]string{common}) }, 50*time.Millisecond))
	t.Cleanup(func() { _ = w.Close() })
	collect, _ := counter()
	lines := startWatch(t, w.Events(), collect, testDebounce)
	if n := decodeN(t, readLine(t, lines)); n != 1 {
		t.Fatalf("start line n = %d, want 1", n)
	}

	time.Sleep(200 * time.Millisecond) // let the poll record its baseline first
	tmp := cursor + ".tmp"
	if err := os.WriteFile(tmp, []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, cursor); err != nil {
		t.Fatal(err)
	}
	if n := decodeN(t, readLine(t, lines)); n != 2 {
		t.Fatalf("line n = %d, want 2 after a cursor rewrite", n)
	}
}
