package entry

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// DashOptions is everything RunDash reads beyond the writers.
type DashOptions struct {
	// Getenv reads CREW_SELF — the crew entry the arm was invoked as.
	Getenv func(string) string
	// Environ is the environment to hand crew-dash, with CREW_BIN replaced.
	Environ func() []string
	// LookPath finds crew-dash, the way the arm's bare `exec crew-dash` did.
	LookPath func(string) (string, error)
	// Exec replaces this process; it only returns on failure.
	Exec func(path string, argv, env []string) error
	// Resolve is `readlink -f`: the path with every symlink in it resolved.
	Resolve func(string) (string, error)
}

// DefaultDashOptions wires the real reads.
func DefaultDashOptions() DashOptions {
	return DashOptions{
		Getenv:   os.Getenv,
		Environ:  os.Environ,
		LookPath: exec.LookPath,
		Exec:     func(path string, argv, env []string) error { return syscall.Exec(path, argv, env) },
		Resolve:  realpath,
	}
}

// RunDash is crew.sh's `dash` arm: hand the terminal to crew-dash, naming in
// CREW_BIN the crew entry the dashboard shells back into for every bus read.
//
// crew-dash is looked up on PATH because flake.nix deliberately keeps `crew` out
// of crew-dash's own inputs (crew lists crew-dash, and naming crew back would
// close an eval-time cycle), so "no such command" is a real outcome and keeps
// the arm's 127.
func RunDash(args []string, stderr io.Writer, o DashOptions) int {
	// CREW_SELF is unset only for a direct `crew-go dash`, which bash's `$0`
	// never was; an empty CREW_BIN is what crew-dash reads as "use crew from
	// PATH", which is the same fallback the arm left in place.
	crewBin := ""
	if self := o.Getenv("CREW_SELF"); self != "" {
		// `readlink -f` resolves what exists and prints the path anyway, so a
		// failure is no reason to refuse: the unresolved path is the closest
		// thing to it, and dash only reads CREW_BIN to exec it or bash its way
		// back into crew.
		p, err := o.Resolve(self)
		if err != nil {
			p = self
		}
		crewBin = p
	}

	bin, err := o.LookPath("crew-dash")
	if err != nil {
		say(stderr, "crew: dash: %v\n", err)
		return exitCantExec
	}
	if err := o.Exec(bin, append([]string{"crew-dash"}, args...), withEnv(o.Environ(), "CREW_BIN", crewBin)); err != nil {
		// Found but not runnable: the shell's own answer for that is 126.
		say(stderr, "crew: dash: %v\n", err)
		return exitNotExec
	}
	return 0 // unreachable: Exec replaces this process
}

// withEnv returns env with key set to value, replacing any earlier entry so the
// child never sees CREW_BIN twice with the arm's value last.
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	set := false
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == key {
			if !set {
				out = append(out, key+"="+value)
				set = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !set {
		out = append(out, key+"="+value)
	}
	return out
}

// realpath is `readlink -f <path>`: absolute, normalised, every symlink
// resolved. filepath.Abs first because EvalSymlinks leaves a relative path
// relative, where readlink never does.
func realpath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
