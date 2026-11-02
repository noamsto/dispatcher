package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fleetLoadEnv runs `crew fleet load` outside any git repo, with no tmux and
// a stubbed nproc on PATH, a 1-minute load of 1.0.
func fleetLoadEnv(t *testing.T) env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "nproc"), []byte("#!/bin/sh\necho 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	loadavg := filepath.Join(dir, "loadavg")
	if err := os.WriteFile(loadavg, []byte("1.00 1.00 1.00 1/1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DISPATCH_LOADAVG_FILE", loadavg)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	return env{getwd: func() (string, error) { return dir, nil }, probes: noTouchProbes()}
}

// TestFleetLoadJSONOutsideAnyRepo: `fleet load --json` needs no git repo and
// no tmux server, and prints the whole document.
func TestFleetLoadJSONOutsideAnyRepo(t *testing.T) {
	e := fleetLoadEnv(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"fleet", "load", "--json", "--budget", "8"}, &stdout, &stderr, e)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var doc struct {
		Budget, Weight, Pending int
		Free, Cores             *int
		Load1                   *float64
		LoadOver                bool `json:"load_over"`
		Leads                   []any
		Engines                 map[string]struct {
			InBudget bool `json:"in_budget"`
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	if doc.Budget != 8 || doc.Weight != 0 || doc.Free == nil || *doc.Free != 8 || doc.Cores == nil || *doc.Cores != 4 ||
		doc.Load1 == nil || *doc.Load1 != 1 || doc.LoadOver || len(doc.Leads) != 0 || !doc.Engines["claude"].InBudget {
		t.Errorf("document %+v", doc)
	}
}

// TestFleetLoadExitCodes: 0 fits, 3 refuses with the reason, 1 for a grammar
// refusal.
func TestFleetLoadExitCodes(t *testing.T) {
	e := fleetLoadEnv(t)
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"fits", []string{"--budget", "8", "--add", "2"}, 0, "fleetBudget 8", ""},
		{"refuses", []string{"--budget", "1", "--add", "3"}, 3, "live lead weight 0 + this deep dispatch (3) exceeds fleetBudget 1\n", ""},
		{"unknown flag", []string{"--bogus"}, 1, "", "crew: fleet load [--json]"},
		{"negative add", []string{"--add", "-1"}, 1, "", "crew: fleet load [--json]"},
		{"not a number", []string{"--budget", "many"}, 1, "", "crew: fleet load [--json]"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"fleet", "load"}, tc.args...), &stdout, &stderr, e)
		if code != tc.code || !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.stderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", tc.name, code, stdout.String(), stderr.String())
		}
	}
}

func TestFleetLoadHelp(t *testing.T) {
	e := fleetLoadEnv(t)
	for _, args := range [][]string{{"fleet", "load", "--help"}, {"fleet", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr, e); code != 0 || !strings.Contains(stdout.String(), "usage: crew fleet load") {
			t.Errorf("%v: exit %d %q %q", args, code, stdout.String(), stderr.String())
		}
	}
}
