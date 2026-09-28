package ui

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// busWatcher abstracts the roster view's two live-refresh implementations
// (fsnotify / a poll fallback) behind one channel: the view never cares
// which is live, only that a change fires an event. Events is buffered by
// one (a coalescing signal, not a queue of every change) and is closed by
// Close, so a blocked reader unblocks rather than leaking.
type busWatcher interface {
	Events() <-chan struct{}
	Close() error
}

// newBusWatcher tries ctor (fsnotify by default — nil picks
// newFsnotifyWatcher) and falls back to a 2s poll of size+mtime when the
// watcher cannot be constructed (fsnotify.NewWatcher or Add failing — e.g.
// an inotify-instance limit, or a sandboxed FS that doesn't support it).
func newBusWatcher(path string, ctor func(string) (busWatcher, error)) busWatcher {
	if ctor == nil {
		ctor = newFsnotifyWatcher
	}
	if w, err := ctor(path); err == nil {
		return w
	}
	return newPollWatcher(path, 2*time.Second)
}

// fsnotifyWatcher watches path's directory (so a not-yet-created log file
// still gets picked up once it's written) and forwards only events whose
// name matches path's base name.
type fsnotifyWatcher struct {
	w      *fsnotify.Watcher
	events chan struct{}
	done   chan struct{}
}

func newFsnotifyWatcher(path string) (busWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}
	fw := &fsnotifyWatcher{w: w, events: make(chan struct{}, 1), done: make(chan struct{})}
	go fw.loop(filepath.Base(path))
	return fw, nil
}

func (fw *fsnotifyWatcher) loop(base string) {
	defer close(fw.done)
	for {
		select {
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			select {
			case fw.events <- struct{}{}:
			default:
			}
		case _, ok := <-fw.w.Errors:
			if !ok {
				return
			}
		}
	}
}

func (fw *fsnotifyWatcher) Events() <-chan struct{} { return fw.events }

// Close closes the underlying watcher (which ends loop's select) and waits
// for loop to exit before closing events itself, so no goroutine can ever
// send on an already-closed channel.
func (fw *fsnotifyWatcher) Close() error {
	err := fw.w.Close()
	<-fw.done
	close(fw.events)
	return err
}

// pollWatcher is the fallback: a 2s (parameterized for tests) poll of the
// file's size and mtime.
type pollWatcher struct {
	events chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

func newPollWatcher(path string, interval time.Duration) busWatcher {
	pw := &pollWatcher{events: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go pw.loop(path, interval)
	return pw
}

func (pw *pollWatcher) loop(path string, interval time.Duration) {
	defer close(pw.done)
	statOf := func() (int64, time.Time, bool) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, time.Time{}, false
		}
		return info.Size(), info.ModTime(), true
	}
	lastSize, lastMod, _ := statOf()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-pw.stop:
			return
		case <-ticker.C:
			size, mod, ok := statOf()
			if !ok {
				continue
			}
			if size != lastSize || !mod.Equal(lastMod) {
				lastSize, lastMod = size, mod
				select {
				case pw.events <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (pw *pollWatcher) Events() <-chan struct{} { return pw.events }

func (pw *pollWatcher) Close() error {
	close(pw.stop)
	<-pw.done
	close(pw.events)
	return nil
}
