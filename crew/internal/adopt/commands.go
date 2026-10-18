package adopt

import (
	"bytes"
	"os/exec"
	"strconv"
)

// The arm's three external commands, one function each, with the argv the bash
// writes. `$(…)` strips the trailing newlines of what it captures, which is what
// these return.

func ghCombined(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	// `gh … 2>&1` into one variable: the failure line prints what it captured,
	// so gh's own message has to land in the same buffer as its output.
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return trimNewlines(buf.String()), err
}

func ghQuiet(args ...string) error {
	cmd := exec.Command("gh", args...)
	return cmd.Run()
}

func worktreeList() string {
	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		// The arm's `$(git … | awk …)`: no worktree, not an error.
		return ""
	}
	return trimNewlines(string(out))
}

// PsArgs is `ps -o args= -p <pid>`.
func PsArgs(pid int) string {
	out, _ := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return trimNewlines(string(out))
}

func trimNewlines(s string) string {
	for len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}
