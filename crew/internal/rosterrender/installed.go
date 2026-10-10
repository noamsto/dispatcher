package rosterrender

import (
	"os"
	"path/filepath"
	"strings"
)

// InstalledCrew is `_rr_installed_crew`: the `crew` this process's environment
// resolves to, realpath'd, or "" when nothing does.
//
// A writeShellApplication wrapper — crew's own, and `dispatch`'s, which lists
// crew in its runtimeInputs — prepends its store bin dirs to PATH, so a plain
// `command -v crew` inside a running daemon answers the build that started it and
// never a home-manager switch. The ambient PATH a switch repoints starts at the
// first entry outside /nix/store, which is why those entries are skipped. Only
// absolute entries count: the daemon runs from the git common dir, so a relative
// one would resolve against the bus rather than anywhere a crew is installed.
//
// Always reports a result: the caller captures it in a context where a failure
// would end the daemon, and "no crew installed" is a normal answer.
func InstalledCrew(path string) string {
	for _, d := range strings.Split(path, string(os.PathListSeparator)) {
		if d == "" || d[0] != '/' {
			continue
		}
		if d == "/nix/store" || strings.HasPrefix(d, "/nix/store/") {
			continue
		}
		t := filepath.Join(d, "crew")
		st, err := os.Stat(t)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		// realpath'd to read like the running build, which is one too. A failed
		// realpath is the arm's empty answer, and the walk stops here either way.
		real, err := filepath.EvalSymlinks(t)
		if err != nil {
			return ""
		}
		return real
	}
	return ""
}
