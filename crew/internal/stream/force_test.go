package stream

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"
)

const (
	forceModeEnv   = "CREW_STREAM_FORCE_HELPER"
	forceLockEnv   = "CREW_STREAM_FORCE_LOCK"
	forceSharedEnv = "CREW_STREAM_FORCE_SHARED"

	forceWait = 15 * time.Second
)

// TestForceHelper is the child process the TestForce* rows spawn; it does
// nothing unless forceModeEnv names its mode. "hold" takes the lock as its own
// pid. "force" runs acquire with --force and, once it holds, proves exclusion
// with an O_EXCL sentinel. Both release on SIGTERM or stdin EOF.
func TestForceHelper(*testing.T) {
	mode := os.Getenv(forceModeEnv)
	if mode == "" {
		return
	}
	lockd, shared := os.Getenv(forceLockEnv), os.Getenv(forceSharedEnv)

	// Armed before any lock work: a rival forcer may TERM this process the
	// moment it holds, and the default action would skip the release below.
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGTERM)
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		stop <- syscall.SIGTERM
	}()

	occupied := filepath.Join(shared, "occupied")
	switch mode {
	case "hold":
		if !lock.Acquire(lockd, strconv.Itoa(os.Getpid())) {
			fmt.Println("refused")
			os.Exit(1)
		}
		fmt.Println("held")
		<-stop
		lock.Release(lockd)
	case "force":
		var stderr bytes.Buffer
		o := Options{Timer: func(d time.Duration) <-chan time.Time { return time.After(d) }}
		if !acquire(lockd, call{crew: "c1", force: true}, o, &stderr) {
			fmt.Println("refused")
			fmt.Print(stderr.String())
			os.Exit(0)
		}
		f, err := os.OpenFile(occupied, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Println("overlap:", err)
			os.Exit(1)
		}
		_ = f.Close()
		fmt.Println("held")
		<-stop
		_ = os.Remove(occupied)
		lock.Release(lockd)
	}
	os.Exit(0)
}

// forceChild is a running TestForceHelper process. Wait is reaped as soon as
// the child starts, since a zombie still answers kill -0 and would make
// cleared time out.
type forceChild struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	done  chan struct{}
	err   error
}

func startForceChild(t *testing.T, mode, lockd, shared string) *forceChild {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestForceHelper$", "-test.timeout=60s")
	// A race-built child otherwise sleeps a second at exit.
	cmd.Env = append(os.Environ(),
		forceModeEnv+"="+mode, forceLockEnv+"="+lockd, forceSharedEnv+"="+shared,
		"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	c := &forceChild{cmd: cmd, stdin: stdin, lines: make(chan string, 64), done: make(chan struct{})}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		_ = r.Close()
	}()
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-c.done
	})
	return c
}

func (c *forceChild) pid() int { return c.cmd.Process.Pid }

// next is the child's next stdout line.
func (c *forceChild) next(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			t.Fatalf("pid %d: stdout closed before a line", c.pid())
		}
		return line
	case <-time.After(forceWait):
		t.Fatalf("pid %d: no output within %s", c.pid(), forceWait)
	}
	return ""
}

// exited waits for the child to be reaped and returns the rest of its stdout.
func (c *forceChild) exited(t *testing.T) []string {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(forceWait):
		t.Fatalf("pid %d: still running after %s", c.pid(), forceWait)
	}
	var rest []string
	for line := range c.lines {
		rest = append(rest, line)
	}
	return rest
}

func (c *forceChild) running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func forceDirs(t *testing.T) (lockd, shared string) {
	t.Helper()
	shared = t.TempDir()
	return filepath.Join(shared, "stream.lock.d"), shared
}

func TestForceTermsAFlockHolder(t *testing.T) {
	lockd, shared := forceDirs(t)
	holder := startForceChild(t, "hold", lockd, shared)
	if got := holder.next(t); got != "held" {
		t.Fatalf("holder said %q", got)
	}

	var stderr bytes.Buffer
	if !acquire(lockd, call{crew: "c1", force: true}, Options{}.withDefaults(), &stderr) {
		t.Fatalf("acquire --force refused: %s", stderr.String())
	}
	t.Cleanup(func() { lock.Release(lockd) })

	if rest := holder.exited(t); len(rest) != 0 {
		t.Errorf("holder said more: %q", rest)
	}
	if holder.err != nil {
		t.Errorf("holder exit: %v", holder.err)
	}
	if got, want := lock.Holder(lockd), strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("Holder = %q, want %q", got, want)
	}
}

func TestForceRefusedWithoutForceNamesTheFlockHolder(t *testing.T) {
	lockd, shared := forceDirs(t)
	holder := startForceChild(t, "hold", lockd, shared)
	if got := holder.next(t); got != "held" {
		t.Fatalf("holder said %q", got)
	}

	var stderr bytes.Buffer
	if acquire(lockd, call{crew: "c1"}, Options{}.withDefaults(), &stderr) {
		lock.Release(lockd)
		t.Fatal("acquire without --force took a held lock")
	}
	want := fmt.Sprintf("another stream is already running for this crew (c1) — pid %d", holder.pid())
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if !holder.running() {
		t.Error("holder was signalled without --force")
	}
}

func TestForceConcurrentNeverOverlaps(t *testing.T) {
	lockd, shared := forceDirs(t)
	holder := startForceChild(t, "hold", lockd, shared)
	if got := holder.next(t); got != "held" {
		t.Fatalf("holder said %q", got)
	}

	forcers := []*forceChild{
		startForceChild(t, "force", lockd, shared),
		startForceChild(t, "force", lockd, shared),
	}
	var first []string
	for _, f := range forcers {
		line := f.next(t)
		if line != "held" && line != "refused" {
			t.Errorf("forcer %d said %q", f.pid(), line)
		}
		first = append(first, line)
	}
	if !strings.Contains(strings.Join(first, " "), "held") {
		t.Errorf("no forcer took the lock: %q", first)
	}

	// A forcer that held and was then TERMed by its rival has already left;
	// only simultaneous holding is a failure, and the sentinel reports it.
	holding := 0
	for i, f := range forcers {
		if first[i] == "held" && f.running() {
			holding++
		}
	}
	if holding > 1 {
		t.Errorf("%d forcers still hold the lock", holding)
	}

	for _, f := range forcers {
		_ = f.stdin.Close()
	}
	for i, f := range forcers {
		rest := f.exited(t)
		for _, line := range rest {
			if strings.HasPrefix(line, "overlap") {
				t.Errorf("forcer %d: %s", f.pid(), line)
			}
		}
		if f.err != nil {
			t.Errorf("forcer %d (%s) exit: %v; output %q", f.pid(), first[i], f.err, rest)
		}
	}
	if !holder.running() {
		if holder.err != nil {
			t.Errorf("holder exit: %v", holder.err)
		}
	} else {
		_ = holder.stdin.Close()
		holder.exited(t)
	}
	if _, err := os.Stat(filepath.Join(shared, "occupied")); !os.IsNotExist(err) {
		t.Errorf("sentinel left behind: %v", err)
	}
}

func TestForceAfterSIGKILLedHolder(t *testing.T) {
	lockd, shared := forceDirs(t)
	holder := startForceChild(t, "hold", lockd, shared)
	if got := holder.next(t); got != "held" {
		t.Fatalf("holder said %q", got)
	}
	if err := holder.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	holder.exited(t)

	var stderr bytes.Buffer
	if !acquire(lockd, call{crew: "c1"}, Options{}.withDefaults(), &stderr) {
		t.Fatalf("acquire after a SIGKILLed holder refused: %s", stderr.String())
	}
	t.Cleanup(func() { lock.Release(lockd) })
	if got, want := lock.Holder(lockd), strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("Holder = %q, want %q", got, want)
	}
}
