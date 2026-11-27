package lock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const helperEnv = "LOCK_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "1":
		os.Exit(holdHelper(os.Args[1]))
	case "inherit":
		os.Exit(inheritHelper())
	}
	os.Exit(m.Run())
}

// holdHelper takes the lock, announces it on stdout, and holds it until stdin
// closes, so the parent can observe ErrHeld from another process.
func holdHelper(path string) int {
	l, err := Acquire(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("locked")
	_, _ = io.ReadAll(os.Stdin)
	if err := l.Release(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// inheritHelper stands in for an engine child: it only keeps the inherited
// fd 3 open until stdin closes, never touching the lock itself.
func inheritHelper() int {
	f := os.NewFile(3, "session.lock")
	if f == nil {
		fmt.Fprintln(os.Stderr, "no fd 3")
		return 1
	}
	fmt.Println("ready")
	_, _ = io.ReadAll(os.Stdin)
	return 0
}

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func startHelper(t *testing.T, mode string, extra []*os.File, args ...string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = extra
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("helper did not start: %v", err)
	}
	return &helper{cmd: cmd, stdin: stdin}
}

func (h *helper) finish(t *testing.T) {
	t.Helper()
	if err := h.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("helper exited with error: %v", err)
	}
}

func TestAcquireAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	h := startHelper(t, "1", nil, path)

	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire while held: want ErrHeld, got %v", err)
	}
	h.finish(t)

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after helper exit: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

// The supervisor hands its lock file to the engine child; the flock must
// outlive the supervisor's Release while that child is alive.
func TestInheritedLockOutlivesRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	h := startHelper(t, "inherit", []*os.File{l.File()})
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire while child holds inherited fd: want ErrHeld, got %v", err)
	}
	h.finish(t)

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after child exit: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatal(err)
	}
}
