package main

import (
	"bytes"
	"context"
	"encoding/json"
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
		{[]string{"fleet", "status", "--bogus"}, 1, "", "crew: fleet status [--json] [--gh]"},
		{[]string{"fleet", "--all", "status"}, 1, "", "crew: fleet status [--json] [--gh]"},
		{[]string{"fleet", "status", "--help"}, 0, "usage: crew fleet status", ""},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != tc.code || !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.errs) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", tc.args, code, stdout.String(), stderr.String())
		}
	}
}
