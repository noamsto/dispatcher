package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// `crew roster-render`'s own flag refusals are the arm's, at the arm's status, and
// they are reached before the crew dir is touched: a usage error leaves no
// renderer's lock behind.
func TestRosterRenderRefusesOnceWithDetach(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"roster-render", "--crew", "c1", "--once", "--detach"}, &stdout, &stderr, e)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d (stderr %q)", code, exitUsage, stderr.String())
	}
	if want := "crew: roster-render: --once and --detach are mutually exclusive\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	lock := filepath.Join(repo, ".git", "crew", "crews", "c1", "roster-render.lock.d")
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a usage error left a renderer's lock behind: %v", err)
	}
}
