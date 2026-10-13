package entry

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// selfRun drives EnsureSelf over an environment it owns, and reports what it
// returned together with the one Setenv it may have made.
func selfRun(t *testing.T, inherited, argv0 string) (string, string) {
	t.Helper()
	set := ""
	p := SelfProbes{
		Getenv: func(string) string { return inherited },
		Setenv: func(_, v string) { set = v },
		// The production reads, so the rows test the derivation and not a stub's
		// idea of what PATH and Abs do.
		LookPath: exec.LookPath,
		Abs:      filepath.Abs,
	}
	return EnsureSelf(p, argv0), set
}

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnsureSelfDerivesTheInvocation(t *testing.T) {
	// A PATH dir that is a symlink to another dir: the entry a profile installs,
	// which CREW_SELF must keep naming unresolved so roster-render can follow a
	// switch under it.
	bin := t.TempDir()
	link := filepath.Join(t.TempDir(), "profile-bin")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	executable(t, bin, "crew")

	t.Run("a bare name is the crew PATH resolves", func(t *testing.T) {
		t.Setenv("PATH", link)
		want := filepath.Join(link, "crew")
		got, set := selfRun(t, "", "crew")
		if got != want || set != want {
			t.Fatalf("EnsureSelf(crew) = (%q, set %q), want %q both ways", got, set, want)
		}
	})

	t.Run("a relative path is made absolute, not resolved", func(t *testing.T) {
		t.Chdir(bin)
		want := filepath.Join(bin, "crew")
		if _, set := selfRun(t, "", "./crew"); set != want {
			t.Fatalf("CREW_SELF = %q, want %q", set, want)
		}
		up := filepath.Join("..", filepath.Base(bin), "crew")
		if _, set := selfRun(t, "", up); set != want {
			t.Fatalf("CREW_SELF = %q, want %q for %q", set, want, up)
		}
	})

	t.Run("an absolute path is kept as it was invoked", func(t *testing.T) {
		want := filepath.Join(link, "crew")
		_, set := selfRun(t, "", want)
		if set != want {
			t.Fatalf("CREW_SELF = %q, want %q unchanged", set, want)
		}
	})

	t.Run("a bare name PATH has no answer for leaves it unset", func(t *testing.T) {
		t.Setenv("PATH", "")
		_, set := selfRun(t, "", "crew")
		if set != "" {
			t.Fatalf("CREW_SELF = %q, want unset", set)
		}
	})

	t.Run("no argv0 is no path", func(t *testing.T) {
		_, set := selfRun(t, "", "")
		if set != "" {
			t.Fatalf("CREW_SELF = %q, want unset", set)
		}
	})
}

func TestEnsureSelfKeepsAnInheritedValue(t *testing.T) {
	// The value is a path this process could not have derived: the arm exported
	// the script it was running, and a caller may name an entry elsewhere.
	inherited := "/nix/store/abc-crew/bin/crew"
	got, set := selfRun(t, inherited, "crew")
	if got != inherited {
		t.Fatalf("returned %q, want the inherited %q", got, inherited)
	}
	if set != "" {
		t.Fatalf("set CREW_SELF to %q over the inherited value", set)
	}
}

func TestResolveSelfReportsAFailedLookup(t *testing.T) {
	// The two failure paths of the injected reads: a lookup that fails, and an
	// Abs that cannot ask where it is.
	p := SelfProbes{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Abs:      filepath.Abs,
	}
	if got := resolveSelf("crew", p); got != "" {
		t.Fatalf("resolveSelf = %q, want \"\" when PATH has no hit", got)
	}
	p = SelfProbes{
		LookPath: exec.LookPath,
		Abs:      func(string) (string, error) { return "", errors.New("no cwd") },
	}
	if got := resolveSelf("./crew", p); got != "./crew" {
		t.Fatalf("resolveSelf = %q, want the path it was given", got)
	}
}

func TestEntryArgv(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "crew")
	if err := os.WriteFile(plain, []byte("#!/usr/bin/env bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "crew-binary")
	if err := os.WriteFile(binary, []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The entry is named as it is, exec bit or not. Prefixing bash for a file
	// without one would run crew.sh — which since #821 dispatches nothing — and
	// the caller would read a silent 0 where it should see the child's 126.
	for _, path := range []string{binary, plain, filepath.Join(dir, "gone")} {
		got := EntryArgv(path, "watch", "--crew", "c1")
		if strings.Join(got, " ") != path+" watch --crew c1" {
			t.Errorf("EntryArgv(%s) = %v, want the entry named first", path, got)
		}
	}
}
