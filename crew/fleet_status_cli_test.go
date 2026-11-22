package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestFleetStatusJSONOutsideAnyRepo: `fleet status --json` needs no git repo,
// no tmux and no gh, and prints the whole document with exit 0.
func TestFleetStatusJSONOutsideAnyRepo(t *testing.T) {
	e := fleetLoadEnv(t)
	t.Setenv("CREW_SWEEP_ROOTS", t.TempDir())
	t.Setenv("DISPATCH_FLEET_BUDGET", "8")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"fleet", "status", "--json", "--gh"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	for _, k := range []string{"ts", "host", "machine", "queue", "repos", "skipped"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("document lacks %q: %s", k, stdout.String())
		}
	}
}

func TestFleetStatusGrammarAndHelp(t *testing.T) {
	e := fleetLoadEnv(t)
	for _, tc := range []struct {
		args         []string
		code         int
		stdout, errs string
	}{
		{[]string{"fleet", "status", "--bogus"}, 1, "", "crew: fleet status [--json] [--gh] [--max-age S]"},
		{[]string{"fleet", "--all", "status"}, 1, "", "crew: fleet status [--json] [--gh] [--max-age S]"},
		{[]string{"fleet", "status", "--help"}, 0, "usage: crew fleet status", ""},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != tc.code || !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.errs) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", tc.args, code, stdout.String(), stderr.String())
		}
	}
}

// TestFleetStatusMaxAge: a second call within --max-age is served from the
// cache file, and a negative --max-age is a grammar refusal.
func TestFleetStatusMaxAge(t *testing.T) {
	e := fleetLoadEnv(t)
	t.Setenv("CREW_SWEEP_ROOTS", t.TempDir())
	t.Setenv("DISPATCH_FLEET_BUDGET", "8")
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	var first, second, errw bytes.Buffer
	args := []string{"fleet", "status", "--json", "--max-age", "60"}
	if code := run(context.Background(), args, &first, &errw, e); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errw.String())
	}
	if code := run(context.Background(), args, &second, &errw, e); code != 0 || second.String() != first.String() {
		t.Fatalf("exit %d, second %q, first %q", code, second.String(), first.String())
	}
	if m, _ := filepath.Glob(filepath.Join(cache, "crew", "fleet-status-*.json")); len(m) != 1 {
		t.Errorf("cache files %v", m)
	}
	var out, stderr bytes.Buffer
	if code := run(context.Background(), []string{"fleet", "status", "--max-age", "-1"}, &out, &stderr, e); code != 1 || !strings.Contains(stderr.String(), "--max-age S") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}
