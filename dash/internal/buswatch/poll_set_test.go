package buswatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testWait = 3 * time.Second

func awaitEvent(t *testing.T, w Watcher, what string) {
	t.Helper()
	select {
	case <-w.Events():
	case <-time.After(testWait):
		t.Fatalf("%s: no event within %s", what, testWait)
	}
}

// awaitTicks returns once paths has been called n times.
func awaitTicks(t *testing.T, ticks <-chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-ticks:
		case <-time.After(testWait):
			t.Fatalf("paths() not called %d times within %s", n, testWait)
		}
	}
}

func TestPollFiresOnRewriteAndNewMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.cursor"), "one")

	ticks := make(chan struct{}, 100)
	paths := func() []string {
		select {
		case ticks <- struct{}{}:
		default:
		}
		return []string{filepath.Join(dir, "*.cursor")}
	}
	w := NewPoll(paths, 20*time.Millisecond)
	defer func() { _ = w.Close() }()

	tmp := filepath.Join(dir, "a.tmp")
	writeFile(t, tmp, "three three")
	if err := os.Rename(tmp, filepath.Join(dir, "a.cursor")); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, w, "rewrite via rename")

	writeFile(t, filepath.Join(dir, "b.cursor"), "new")
	awaitEvent(t, w, "new matching file")
}

func TestPollStaysQuietWithoutChanges(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.cursor"), "one")

	ticks := make(chan struct{}, 100)
	paths := func() []string {
		select {
		case ticks <- struct{}{}:
		default:
		}
		return []string{filepath.Join(dir, "*.cursor")}
	}
	w := NewPoll(paths, 20*time.Millisecond)
	defer func() { _ = w.Close() }()

	awaitTicks(t, ticks, 7)
	select {
	case <-w.Events():
		t.Fatal("poll fired with no change to the matched files")
	default:
	}
}

func TestPollCloseClosesEvents(t *testing.T) {
	dir := t.TempDir()
	w := NewPoll(func() []string { return []string{filepath.Join(dir, "*.cursor")} }, 20*time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-w.Events():
		if ok {
			t.Fatal("Events delivered a value after Close")
		}
	case <-time.After(testWait):
		t.Fatal("Close did not close Events")
	}
}

// fakeWatcher is a Watcher whose events the test sends by hand.
type fakeWatcher struct {
	events chan struct{}
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{events: make(chan struct{})} }

func (f *fakeWatcher) Events() <-chan struct{} { return f.events }

func (f *fakeWatcher) Close() error {
	close(f.events)
	return nil
}

func sendFake(t *testing.T, f *fakeWatcher) {
	t.Helper()
	select {
	case f.events <- struct{}{}:
	case <-time.After(testWait):
		t.Fatalf("fake input did not accept an event within %s", testWait)
	}
}

func TestMergeForwardsEventsFromEveryInput(t *testing.T) {
	a, b := newFakeWatcher(), newFakeWatcher()
	m := Merge(a, nil, b)
	defer func() { _ = m.Close() }()

	sendFake(t, a)
	awaitEvent(t, m, "first input")
	sendFake(t, b)
	awaitEvent(t, m, "second input")
}

func TestMergeCloseClosesInputsAndOutput(t *testing.T) {
	a, b := newFakeWatcher(), newFakeWatcher()
	m := Merge(a, b)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for name, ev := range map[string]<-chan struct{}{"merged": m.Events(), "a": a.Events(), "b": b.Events()} {
		select {
		case _, ok := <-ev:
			if ok {
				t.Fatalf("%s delivered a value after Close", name)
			}
		case <-time.After(testWait):
			t.Fatalf("%s was not closed by Merge.Close", name)
		}
	}
}

func TestPollChangeRightAfterNewPollFires(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.cursor")
	writeFile(t, file, "one")

	w := NewPoll(func() []string { return []string{filepath.Join(dir, "*.cursor")} }, 50*time.Millisecond)
	defer func() { _ = w.Close() }()
	writeFile(t, file, "two two")
	awaitEvent(t, w, "write between NewPoll and the first tick")
}

func TestTickerFiresAndCloses(t *testing.T) {
	w := NewTicker(10 * time.Millisecond)
	awaitEvent(t, w, "tick")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
