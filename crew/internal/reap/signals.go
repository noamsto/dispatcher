package reap

import (
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// cleaner is the arm's EXIT trap: the locks reap holds and the --discard
// scratch dir, released on every return and on a fatal signal. Its state is
// mutex-guarded because the signal handler runs on its own goroutine.
type cleaner struct {
	mu     sync.Mutex
	tmpidx string
	locks  []string
}

// setTmp records the scratch dir to remove, "" once it is gone.
func (c *cleaner) setTmp(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tmpidx = dir
}

// hold records an acquired lock dir.
func (c *cleaner) hold(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.locks = append(c.locks, dir)
}

// release gives up one lock dir now.
func (c *cleaner) release(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock.Release(dir)
	c.locks = slices.DeleteFunc(c.locks, func(d string) bool { return d == dir })
}

// cleanup is the trap body: remove the scratch dir, release every lock. Safe
// to run twice.
func (c *cleaner) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tmpidx != "" {
		_ = os.RemoveAll(c.tmpidx)
		c.tmpidx = ""
	}
	for _, d := range c.locks {
		lock.Release(d)
	}
	c.locks = nil
}

// notify runs cleanup and exits 128+signo — the status bash reported for a
// shell killed by the signal after its EXIT trap ran — on SIGTERM, SIGINT or
// SIGHUP. An inherited-ignored SIGHUP or SIGINT (nohup, a background job's
// SIGINT) stays ignored, as it did for bash; Go's runtime keeps an inherited
// SIG_IGN only for those two, so SIGTERM is always handled even where bash
// would have ignored it. The returned func stops listening.
func (c *cleaner) notify() func() {
	var sigs []os.Signal
	for _, s := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, sigs...)
	go func() {
		select {
		case sig := <-ch:
			c.cleanup()
			os.Exit(128 + int(sig.(syscall.Signal)))
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
