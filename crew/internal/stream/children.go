// The loop's three subprocesses, run the way the bash arm ran them: the crew
// entry re-entered with entry.EntryArgv (the binary or wrapper directly, bash in
// front of a script that is not executable), stdin from /dev/null, each stream
// either captured or redirected to a file.
//
// Two of them are backgrounded, and each has a reason to be a process rather
// than a function call:
//
//   - the inner `crew watch` holds `watch.lock.d` for its whole park and
//     releases it from its own handler, so the stream stops it with TERM and
//     waits — never KILL, which would skip the release and leave the lock's
//     record behind;
//   - `crew reap` is never awaited and never signalled by the stream (an
//     interrupted removal strands a worktree), so its output goes to files it
//     owns rather than to pipes the stream would close on the way out.
package stream

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/entry"
)

// exitCantExec is what `command not found` left in `$?` when the script the arm
// re-entered was gone: the loop reads it as an inner failure, one error line.
const exitCantExec = 127

// children re-enters one crew entrypoint.
type children struct {
	self string
	// dir is the cwd of `start`'s child; empty inherits ours.
	dir string
}

// command is the arm's `bash -euo pipefail "$0" …`, the entry re-entered the way
// entry.EntryArgv runs it.
func (c children) command(args []string) *exec.Cmd {
	argv := entry.EntryArgv(c.self, args...)
	return exec.Command(argv[0], argv[1:]...)
}

// run is the arm's `out=$(bash … 2>file)`: stdout captured, stderr redirected
// to stderrPath (or dropped when it is empty), and the child's exit status. A
// child that never started is 127 with the reason where its stderr would have
// gone, because that is the pair of facts the arm was left with.
func (c children) run(args []string, stderrPath string) (string, int) {
	cmd := c.command(args)
	var out bytes.Buffer
	cmd.Stdout = &out
	if stderrPath != "" {
		f, err := os.Create(stderrPath)
		if err != nil {
			return "", exitCantExec
		}
		defer func() { _ = f.Close() }()
		cmd.Stderr = f
	}
	err := cmd.Run()
	var startErr *exec.Error
	if errors.As(err, &startErr) {
		if f, cerr := os.Create(stderrPath); cerr == nil {
			_, _ = f.WriteString(err.Error() + "\n")
			_ = f.Close()
		}
		return "", exitCantExec
	}
	return out.String(), waitStatus(err)
}

// start is the arm's `bash … >out 2>err &`: both streams are files, each
// truncated at spawn exactly as the shell's redirect truncated them.
func (c children) start(args []string, stdoutPath, stderrPath string) (*child, error) {
	out, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Close() }()
	errf, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	defer func() { _ = errf.Close() }()

	cmd := c.command(args)
	cmd.Stdout, cmd.Stderr = out, errf
	if c.dir != "" {
		cmd.Dir = c.dir
		// git prefers these to the cwd, so an inherited one would point every
		// directory-scoped child at the same repo.
		cmd.Env = withoutGitDirs(os.Environ())
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := &child{cmd: cmd, done: make(chan struct{})}
	go ch.reap()
	return ch, nil
}

func withoutGitDirs(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// startReap is the arm's background subshell: HUP ignored (a signal aimed at
// the whole process group still reaches it), and both streams in files created
// for this child alone, so a reap that outlives its stream cannot clobber the
// next stream's pair.
func (c children) startReap(dir string) (*reapRun, error) {
	out, err := os.CreateTemp(dir, "stream.reap.out.")
	if err != nil {
		return nil, err
	}
	errf, err := os.CreateTemp(dir, "stream.reap.err.")
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	// $0 is the entry path, and the exec'd reap inherits this shell's two files
	// and its HUP disposition. The outer bash exists for that trap alone; what it
	// execs follows entry.EntryArgv's rule.
	script := `trap '' HUP; ` + c.reapExec()
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script, c.self)
	cmd.Stdout, cmd.Stderr = out, errf
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		_ = errf.Close()
		return nil, err
	}
	_ = out.Close()
	_ = errf.Close()
	r := &reapRun{cmd: cmd, out: out.Name(), errf: errf.Name(), done: make(chan struct{})}
	return r, nil
}

// reapExec is the reap's exec in that shell, by the same rule: the entry path
// itself when it is executable, bash in front of it when it is a script.
func (c children) reapExec() string {
	const args = ` reap --quiet --no-wait`
	if entry.IsExecutable(c.self) {
		return `exec "$0"` + args
	}
	return `exec bash -euo pipefail "$0"` + args
}

// child is one running subprocess the loop waits on.
type child struct {
	cmd  *exec.Cmd
	code int
	done chan struct{}
}

func (c *child) reap() {
	defer close(c.done)
	c.code = waitStatus(c.cmd.Wait())
}

// wait is the arm's `wait "$child"`: its exit status, or 128+n when it died of
// a signal. Repeatable, because the cleanup may run after the loop already read
// the status.
func (c *child) wait() int {
	<-c.done
	return c.code
}

// stop is the arm's `kill -TERM "$child"; wait "$child"`. TERM, never KILL: the
// inner watch releases `watch.lock.d` from its own handler.
func (c *child) stop() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
	}
	c.wait()
}

// reapRun is one background `crew reap`. The stream neither awaits it nor
// signals it; it only reads the pair once the child is gone, and closes done
// once that pair holds its stream lines.
type reapRun struct {
	cmd  *exec.Cmd
	out  string // the reap's stdout, rewritten with its stream lines once it exits
	errf string // its stderr, whose first line becomes the error detail
	code int
	ts   int64
	done chan struct{}
}

// live reports whether the child is still running: a running one may still be
// writing its pair.
func (r *reapRun) live() bool {
	if r == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// waitStatus is a Wait error read as the arm's `$?` under `set -e`: the child's
// own status, or 128+n when it died of a signal. watch's helper, same reasoning.
func waitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}
