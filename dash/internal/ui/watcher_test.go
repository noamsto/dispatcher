package ui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFsnotifyWatcherFiresOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := newBusWatcher(path, nil)
	defer w.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		f.WriteString("{}\n")
		f.Close()
	}()

	select {
	case <-w.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire within 3s of a write")
	}
}

func TestFsnotifyWatcherIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o644)
	other := filepath.Join(dir, "unrelated.txt")

	w := newBusWatcher(path, nil)
	defer w.Close()

	os.WriteFile(other, []byte("hi"), 0o644)

	select {
	case <-w.Events():
		t.Fatal("watcher fired for a write to an unrelated file")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPollWatcherFallbackOnFailingCtor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o644)

	failing := func(string) (busWatcher, error) { return nil, errors.New("no fsnotify here") }
	w := newBusWatcher(path, failing)
	defer w.Close()
	if _, ok := w.(*pollWatcher); !ok {
		t.Fatalf("newBusWatcher with a failing ctor = %T, want *pollWatcher", w)
	}
}

func TestPollWatcherFiresOnSizeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o644)

	w := newPollWatcher(path, 20*time.Millisecond)
	defer w.Close()

	go func() {
		time.Sleep(30 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString("{}\n")
		f.Close()
	}()

	select {
	case <-w.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("poll watcher did not fire within 2s of a size change")
	}
}

// TestWatcherCloseUnblocksReaderAndExits proves the standard leak scenario
// doesn't happen: a goroutine blocked reading Events() (the waitForBusChange
// Cmd pattern) unblocks the moment Close returns, and Close itself does not
// hang waiting on the internal loop goroutine.
func TestWatcherCloseUnblocksReaderAndExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o644)

	w := newBusWatcher(path, nil)

	readerDone := make(chan struct{})
	go func() {
		<-w.Events() // blocks until Close closes the channel
		close(readerDone)
	}()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- w.Close()
	}()

	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s")
	}

	select {
	case <-readerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("a reader blocked on Events() was not unblocked by Close")
	}
}
