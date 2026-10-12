package entry

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type execCall struct {
	path string
	argv []string
	env  []string
}

// dashEnv is the Getenv/Environ pair of a test: env is what CREW_SELF and friends
// read, and what the child's environment starts from.
func dashEnv(env map[string]string) (func(string) string, func() []string) {
	var list []string
	for k, v := range env {
		list = append(list, k+"="+v)
	}
	return func(k string) string { return env[k] }, func() []string { return slices.Clone(list) }
}

func TestDashResolvesCrewSelfIntoCrewBin(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "crew"), "#!/bin/sh\n")
	symlink(t, filepath.Join(dir, "crew-link"), filepath.Join(dir, "crew"))

	var calls []execCall
	getenv, environ := dashEnv(map[string]string{"CREW_SELF": filepath.Join(dir, "crew-link"), "PATH": "/usr/bin"})
	o := DashOptions{
		Getenv:   getenv,
		Environ:  environ,
		LookPath: func(name string) (string, error) { return "/store/crew-dash/bin/" + name, nil },
		Exec: func(path string, argv, env []string) error {
			calls = append(calls, execCall{path, argv, env})
			return nil
		},
		Resolve: realpath,
	}
	var stderr bytes.Buffer
	if code := RunDash([]string{"--once"}, &stderr, o); code != 0 {
		t.Fatalf("code %d, want 0 (stderr %q)", code, stderr.String())
	}
	if len(calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.path != "/store/crew-dash/bin/crew-dash" {
		t.Errorf("exec path = %q", call.path)
	}
	// bash's `exec crew-dash "$@"` names the command, not the path it found.
	if want := []string{"crew-dash", "--once"}; !slices.Equal(call.argv, want) {
		t.Errorf("argv = %q, want %q", call.argv, want)
	}
	if got := envValue(call.env, "CREW_BIN"); got != filepath.Join(dir, "crew") {
		t.Errorf("CREW_BIN = %q, want the resolved entry %q", got, filepath.Join(dir, "crew"))
	}
	// The rest of the environment rides along untouched.
	if got := envValue(call.env, "PATH"); got != "/usr/bin" {
		t.Errorf("PATH = %q, want /usr/bin", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// TestDashCarriesEveryArgument: `crew dash --json` reaches crew-dash as
// `--json`, and an argument that looks like a flag is no argument of ours.
func TestDashCarriesEveryArgument(t *testing.T) {
	var calls []execCall
	getenv, environ := dashEnv(map[string]string{"CREW_SELF": "/bin/sh"})
	o := DashOptions{
		Getenv: getenv, Environ: environ,
		LookPath: func(string) (string, error) { return "/bin/true", nil },
		Exec: func(path string, argv, env []string) error {
			calls = append(calls, execCall{path, argv, env})
			return nil
		},
		Resolve: func(p string) (string, error) { return p, nil },
	}
	if code := RunDash([]string{"--once", "--json"}, &bytes.Buffer{}, o); code != 0 {
		t.Fatalf("code %d", code)
	}
	if want := []string{"crew-dash", "--once", "--json"}; !slices.Equal(calls[0].argv, want) {
		t.Fatalf("argv = %q, want %q", calls[0].argv, want)
	}
}

// TestDashWithNoCrewSelf: a direct `crew-go dash` has no CREW_SELF, and an empty
// CREW_BIN is what crew-dash reads as "use crew from PATH".
func TestDashWithNoCrewSelf(t *testing.T) {
	var calls []execCall
	getenv, environ := dashEnv(map[string]string{})
	o := DashOptions{
		Getenv: getenv, Environ: environ,
		LookPath: func(string) (string, error) { return "/bin/true", nil },
		Exec: func(path string, argv, env []string) error {
			calls = append(calls, execCall{path, argv, env})
			return nil
		},
		Resolve: realpath,
	}
	if code := RunDash(nil, &bytes.Buffer{}, o); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := envValue(calls[0].env, "CREW_BIN"); got != "" {
		t.Fatalf("CREW_BIN = %q, want empty", got)
	}
	if n := countEnv(calls[0].env, "CREW_BIN"); n != 1 {
		t.Fatalf("CREW_BIN appears %d times in the child's environment", n)
	}
}

// TestDashReplacesAnInheritedCrewBin: the arm always sets CREW_BIN, so a stale
// one in the environment must not win by landing last.
func TestDashReplacesAnInheritedCrewBin(t *testing.T) {
	var calls []execCall
	getenv, environ := dashEnv(map[string]string{"CREW_SELF": "/bin/sh", "CREW_BIN": "/stale/crew"})
	o := DashOptions{
		Getenv: getenv, Environ: environ,
		LookPath: func(string) (string, error) { return "/bin/true", nil },
		Exec: func(path string, argv, env []string) error {
			calls = append(calls, execCall{path, argv, env})
			return nil
		},
		Resolve: func(p string) (string, error) { return p, nil },
	}
	if code := RunDash(nil, &bytes.Buffer{}, o); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := envValue(calls[0].env, "CREW_BIN"); got != "/bin/sh" {
		t.Errorf("CREW_BIN = %q, want /bin/sh", got)
	}
	if n := countEnv(calls[0].env, "CREW_BIN"); n != 1 {
		t.Errorf("CREW_BIN appears %d times in the child's environment", n)
	}
}

// TestDashWithNoCrewDash: flake.nix keeps crew out of crew-dash's inputs and
// crew-dash off any baked path, so "not on PATH" is a real outcome. bash's was
// `command not found`, 127, with nothing else printed.
func TestDashWithNoCrewDash(t *testing.T) {
	getenv, environ := dashEnv(map[string]string{"CREW_SELF": "/bin/sh"})
	o := DashOptions{
		Getenv:  getenv,
		Environ: environ,
		LookPath: func(string) (string, error) {
			return "", errors.New(`exec: "crew-dash": executable file not found in $PATH`)
		},
		Exec: func(string, []string, []string) error {
			t.Error("exec ran with no crew-dash to run")
			return nil
		},
		Resolve: realpath,
	}
	var stdout, stderr bytes.Buffer
	if code := RunDash(nil, &stderr, o); code != exitCantExec {
		t.Fatalf("code %d, want %d", code, exitCantExec)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q", stdout.String())
	}
	if line := stderr.String(); !strings.HasPrefix(line, "crew: dash: ") || strings.Count(line, "\n") != 1 {
		t.Errorf("stderr = %q, want one crew: dash: line", line)
	}
}

func TestDashExecRefused(t *testing.T) {
	getenv, environ := dashEnv(map[string]string{"CREW_SELF": "/bin/sh"})
	o := DashOptions{
		Getenv:   getenv,
		Environ:  environ,
		LookPath: func(string) (string, error) { return "/bin/true", nil },
		Exec: func(string, []string, []string) error {
			return errors.New("permission denied")
		},
		Resolve: realpath,
	}
	var stderr bytes.Buffer
	if code := RunDash(nil, &stderr, o); code != exitNotExec {
		t.Fatalf("code %d, want %d", code, exitNotExec)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr = %q, want the exec error", stderr.String())
	}
}

// TestDashResolveFailureKeepsThePath: `readlink -f` prints the path it was
// given when it cannot resolve it, and dash only reads CREW_BIN to run it.
func TestDashResolveFailureKeepsThePath(t *testing.T) {
	var calls []execCall
	getenv, environ := dashEnv(map[string]string{"CREW_SELF": "/loop/crew"})
	o := DashOptions{
		Getenv: getenv, Environ: environ,
		LookPath: func(string) (string, error) { return "/bin/true", nil },
		Exec: func(path string, argv, env []string) error {
			calls = append(calls, execCall{path, argv, env})
			return nil
		},
		Resolve: func(string) (string, error) { return "", errors.New("too many levels of symbolic links") },
	}
	if code := RunDash(nil, &bytes.Buffer{}, o); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := envValue(calls[0].env, "CREW_BIN"); got != "/loop/crew" {
		t.Fatalf("CREW_BIN = %q, want the unresolved CREW_SELF", got)
	}
}

func TestRealpath(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "crew"), "#!/bin/sh\n")
	link := filepath.Join(dir, "crew-link")
	symlink(t, link, filepath.Join(dir, "crew"))

	got, err := realpath(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(dir, "crew"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("realpath = %q, want %q", got, want)
	}

	// readlink -f never leaves a relative path behind, and neither does this.
	t.Chdir(dir)
	rel, err := realpath("crew-link")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rel) {
		t.Fatalf("realpath(crew-link) = %q, want an absolute path", rel)
	}
}

func TestWithEnv(t *testing.T) {
	got := withEnv([]string{"A=1", "CREW_BIN=old", "B=2"}, "CREW_BIN", "new")
	if want := []string{"A=1", "CREW_BIN=new", "B=2"}; !slices.Equal(got, want) {
		t.Fatalf("withEnv = %q, want %q", got, want)
	}
	if got := withEnv(nil, "CREW_BIN", "x"); !slices.Equal(got, []string{"CREW_BIN=x"}) {
		t.Fatalf("withEnv(nil) = %q", got)
	}
	// A value that is empty is still a value: crew-dash reads it as "no CREW_BIN".
	if got := withEnv([]string{"CREW_BIN=old"}, "CREW_BIN", ""); !slices.Equal(got, []string{"CREW_BIN="}) {
		t.Fatalf("withEnv empty = %q", got)
	}
	// A name that is only a prefix of the key is no match.
	if got := withEnv([]string{"CREW_BINX=1"}, "CREW_BIN", "v"); !slices.Equal(got, []string{"CREW_BINX=1", "CREW_BIN=v"}) {
		t.Fatalf("withEnv prefix = %q", got)
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func countEnv(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			n++
		}
	}
	return n
}
