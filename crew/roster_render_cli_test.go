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

// `--map` refuses what only the daemon takes, and every refusal comes before the
// crew dir is made.
func TestRosterRenderMapRefusals(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"once", []string{"--map", "--crew", "c1", "--px", "640x480", "--out", "m.png", "--once"},
			"crew: roster-render: --map and --once are mutually exclusive\n"},
		{"interval", []string{"--map", "--crew", "c1", "--px", "640x480", "--out", "m.png", "--interval", "3"},
			"crew: roster-render: --map and --interval are mutually exclusive\n"},
		{"no px", []string{"--map", "--crew", "c1", "--out", "m.png"}, "crew: roster-render: --map requires --px\n"},
		{"no out", []string{"--map", "--crew", "c1", "--px", "640x480"}, "crew: roster-render: --map requires --out\n"},
		{"small px", []string{"--map", "--crew", "c1", "--px", "32x480", "--out", "m.png"},
			"crew: roster-render: --px must be WxH with each side from 64 to 4096 pixels\n"},
		{"px without map", []string{"--crew", "c1", "--px", "640x480"},
			"crew: roster-render: --px, --select, --select-repo and --out need --map\n"},
		{"empty px", []string{"--map", "--crew", "c1", "--px="}, "crew: --px needs a value\n"},
		{"empty select", []string{"--map", "--crew", "c1", "--select="}, "crew: --select needs a value\n"},
		{"empty select-repo", []string{"--map", "--crew", "c1", "--select-repo="}, "crew: --select-repo needs a value\n"},
		{"empty out", []string{"--map", "--crew", "c1", "--out="}, "crew: --out needs a value\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), append([]string{"roster-render"}, tc.args...), &stdout, &stderr, e)
			if code != exitUsage || stderr.String() != tc.want || stdout.Len() != 0 {
				t.Errorf("code %d, stdout %q, stderr %q; want %d and %q", code, stdout.String(), stderr.String(), exitUsage, tc.want)
			}
			if _, err := os.Stat(filepath.Join(repo, ".git", "crew", "crews", "c1")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refusal made the crew dir: %v", err)
			}
		})
	}
}
