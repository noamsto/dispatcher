package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReapFleetCLIRoutesBeforeLocateAndReportsANoBusCandidate is the routing
// test for `crew reap --fleet`: it runs from outside any repo and discovers a
// registry candidate with no bus, so it exercises the pre-Locate route and
// discovery without a live reap (which needs tmux, gh and gitguard anchors).
func TestReapFleetCLIRoutesBeforeLocateAndReportsANoBusCandidate(t *testing.T) {
	c := newSweepAllCLI(t)
	common := filepath.Join(c.root, "nobus", ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(c.data, "crew", "repos")
	if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registry, []byte(common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := c.runIn(t.TempDir(), "reap", "--fleet")
	if code != 0 {
		t.Fatalf("reap --fleet: code %d, want 0\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "not in a git repo") {
		t.Errorf("stderr = %q, want no outside-a-repo refusal", stderr)
	}
	if want := "crew reap --fleet: " + common + ": skipped (no bus)"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout, want)
	}
}
