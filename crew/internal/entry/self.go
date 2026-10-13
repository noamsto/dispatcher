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
// wants outranks anything derived here, which is what the arm's
// `CREW_SELF="$0" exec` meant for the process it replaced.
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

// EntryArgv is the argv prefix that runs the crew entry `self` names, with
// `args` after it: the file itself when it is executable — the Go binary, or the
// PATH wrapper flake.nix installs it behind — and bash in front of it otherwise,
// which is how a non-executable script gets run. Before this change the entry
// was always that script, and every re-entry said so in code.
//
// dash/main.go:resolveCrew resolves CREW_BIN by the same rule; it is a separate
// module, so it keeps its own copy.
func EntryArgv(self string, args ...string) []string {
	if !IsExecutable(self) {
		return append([]string{"bash", "-euo", "pipefail", self}, args...)
	}
	return append([]string{self}, args...)
}

// IsExecutable is a regular file with any execute bit, the way a shell decides
// to run a path directly. It is EntryArgv's rule, exported for the one caller
// that has to write the re-entry into a shell command line of its own.
func IsExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
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
