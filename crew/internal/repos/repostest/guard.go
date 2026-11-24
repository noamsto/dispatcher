// Package repostest keeps a package's tests off the machine's real repo
// registry.
package repostest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// Runner is what a TestMain runs: *testing.M, or testscript.Main's argument.
type Runner interface{ Run() int }

// Guard wraps m for TestMain. Its Run points XDG_DATA_HOME at a scratch dir,
// runs m, and returns non-zero if the registry the original environment names
// changed meanwhile. It does not import repos, whose own tests use it.
func Guard(m Runner) Runner { return guard{m, realRegistry()} }

type guard struct {
	m    Runner
	real string
}

func realRegistry() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = os.Getenv("HOME") + "/.local/share"
	}
	return filepath.Join(base, "crew", "repos")
}

func snapshot(path string) (data []byte, exists bool) {
	data, err := os.ReadFile(path)
	return data, err == nil
}

func (g guard) Run() int {
	before, existed := snapshot(g.real)
	scratch, err := os.MkdirTemp("", "crew-test-data-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "repostest:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	prev, had := os.LookupEnv("XDG_DATA_HOME")
	_ = os.Setenv("XDG_DATA_HOME", scratch)
	code := g.m.Run()
	if had {
		_ = os.Setenv("XDG_DATA_HOME", prev)
	} else {
		_ = os.Unsetenv("XDG_DATA_HOME")
	}
	after, exists := snapshot(g.real)
	if existed != exists || !bytes.Equal(before, after) {
		fmt.Fprintf(os.Stderr, "repostest: tests changed the real registry %s; point XDG_DATA_HOME at a temp dir\n", g.real)
		return 1
	}
	return code
}
