package entry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SelfProbes are the reads EnsureSelf makes beyond its arguments, so the
// derivation is testable without touching this process's environment.
type SelfProbes struct {
	Getenv func(string) string
	Setenv func(string, string)
	// LookPath is a bare command's $PATH lookup: exec.LookPath in production.
	LookPath func(string) (string, error)
	// Abs makes a path absolute and resolves nothing: filepath.Abs.
	Abs func(string) (string, error)
}

// DefaultSelfProbes is EnsureSelf's production wiring.
func DefaultSelfProbes() SelfProbes {
	return SelfProbes{
		Getenv:   os.Getenv,
		Setenv:   func(k, v string) { _ = os.Setenv(k, v) },
		LookPath: exec.LookPath,
		Abs:      filepath.Abs,
	}
}

// EnsureSelf gives this process — and so every child it spawns — the CREW_SELF
// crew.sh's delegation arm exported as its own `$0`, and returns it. The three
// readers are `dash` (which `readlink`s it into CREW_BIN), `stream` (which
// re-enters it for `watch`, `hold due` and `reap`), and `roster-render` (which
// compares it against the installed `crew` and re-execs that when they differ,
// so a home-manager switch reaches a running daemon).
//
// An inherited value is kept exactly as it is: a caller that names the entry it
// wants outranks anything derived here.
func EnsureSelf(p SelfProbes, argv0 string) string {
	if self := p.Getenv("CREW_SELF"); self != "" {
		return self
	}
	self := resolveSelf(argv0, p)
	if self == "" {
		return ""
	}
	p.Setenv("CREW_SELF", self)
	return self
}

// EntryArgv is the argv that runs the crew entry `self` names, with `args` after
// it. The entry is a program — the Go binary, or the PATH wrapper flake.nix
// installs behind it — so it is named as it is, and a `self` that is not
// executable fails as the child's 126/127 rather than running as a script.
func EntryArgv(self string, args ...string) []string {
	return append([]string{self}, args...)
}

// resolveSelf derives the entry path from how this process was invoked: a name
// with no slash is the command PATH resolves, and anything else is that path made
// absolute. Neither branch resolves a symlink — CREW_SELF is the entry a switch
// repoints, and `readlink`ing it here would freeze the build it pointed at when
// this process started. A bare name PATH does not have is no path at all, so it
// leaves CREW_SELF unset and `stream`'s own fallback (the `crew` on PATH) answers.
func resolveSelf(argv0 string, p SelfProbes) string {
	if argv0 == "" {
		return ""
	}
	candidate := argv0
	if !strings.Contains(candidate, "/") {
		hit, err := p.LookPath(candidate)
		if err != nil {
			return ""
		}
		candidate = hit
	}
	abs, err := p.Abs(candidate)
	if err != nil {
		return candidate
	}
	return abs
}
