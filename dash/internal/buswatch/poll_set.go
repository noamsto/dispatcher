package buswatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// NewPoll returns a Watcher that every interval re-expands paths() as glob
// patterns and signals when the matched files' names, sizes or mtimes change.
// The baseline is taken before NewPoll returns, so files that exist then do
// not fire and any later change does.
func NewPoll(paths func() []string, interval time.Duration) Watcher {
	pw := &pollSetWatcher{events: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go pw.loop(pollSignature(paths()), paths, interval)
	return pw
}

// NewTicker returns a Watcher that signals every interval.
func NewTicker(interval time.Duration) Watcher {
	pw := &pollSetWatcher{events: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(pw.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-pw.stop:
				return
			case <-ticker.C:
				select {
				case pw.events <- struct{}{}:
				default:
				}
			}
		}
	}()
	return pw
}

type pollSetWatcher struct {
	events chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

func (pw *pollSetWatcher) loop(last string, paths func() []string, interval time.Duration) {
	defer close(pw.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-pw.stop:
			return
		case <-ticker.C:
			sig := pollSignature(paths())
			if sig != last {
				select {
				case pw.events <- struct{}{}:
				default:
				}
			}
			last = sig
		}
	}
}

func (pw *pollSetWatcher) Events() <-chan struct{} { return pw.events }

func (pw *pollSetWatcher) Close() error {
	close(pw.stop)
	<-pw.done
	close(pw.events)
	return nil
}

// pollSignature describes every file matched by the glob patterns. A pattern
// that fails to parse matches nothing; a match that vanishes between Glob and
// Stat drops out of the signature, which is itself a change.
func pollSignature(patterns []string) string {
	var entries []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, name := range matches {
			info, err := os.Stat(name)
			if err != nil {
				continue
			}
			entries = append(entries, fmt.Sprintf("%s|%d|%d", name, info.Size(), info.ModTime().UnixNano()))
		}
	}
	slices.Sort(entries)
	return strings.Join(entries, "\n")
}

// Merge fans the events of ws into one Watcher. Nil entries are skipped.
func Merge(ws ...Watcher) Watcher {
	mw := &mergedWatcher{events: make(chan struct{}, 1)}
	for _, w := range ws {
		if w == nil {
			continue
		}
		mw.inputs = append(mw.inputs, w)
		mw.forwarders.Add(1)
		go mw.forward(w)
	}
	return mw
}

type mergedWatcher struct {
	events     chan struct{}
	inputs     []Watcher
	forwarders sync.WaitGroup
}

func (mw *mergedWatcher) forward(w Watcher) {
	defer mw.forwarders.Done()
	for range w.Events() {
		select {
		case mw.events <- struct{}{}:
		default:
		}
	}
}

func (mw *mergedWatcher) Events() <-chan struct{} { return mw.events }

// Close closes every input before waiting, so each forwarder's range over an
// input's Events ends and no goroutine can send on the closed merged channel.
func (mw *mergedWatcher) Close() error {
	var errs []error
	for _, w := range mw.inputs {
		errs = append(errs, w.Close())
	}
	mw.forwarders.Wait()
	close(mw.events)
	return errors.Join(errs...)
}
