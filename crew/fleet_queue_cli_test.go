package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fleetQueueEnv is fleetLoadEnv with a dispatch-config that sets fleetBudget 8,
// and no worker identity, which `fleet prio` refuses.
func fleetQueueEnv(t *testing.T) env {
	t.Helper()
	e := fleetLoadEnv(t)
	stub := filepath.Join(t.TempDir(), "dispatch-config")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '{\"fleetBudget\": 8}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPATCH_CONFIG_BIN", stub)
	t.Setenv("CREW_WORKER_ID", "")
	t.Setenv("CREW_ROLE_ID", "")
	return e
}

func TestFleetQueueJSONAndAdmitOutsideAnyRepo(t *testing.T) {
	e := fleetQueueEnv(t)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"fleet", "queue", "--json"}, &stdout, &stderr, e); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var doc struct {
		Budget int
		Due    any
		Head   any
		Holds  []any
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	if doc.Budget != 8 || doc.Due != nil || doc.Head != nil || doc.Holds == nil || len(doc.Holds) != 0 {
		t.Errorf("document %+v", doc)
	}

	stdout.Reset()
	if code := run(context.Background(), []string{"fleet", "queue", "--admit", "2", "--hold", "h1", "--own", "feat/x"}, &stdout, &stderr, e); code != 0 || stdout.Len() != 0 {
		t.Errorf("an empty queue admits: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestFleetQueueGrammarAndHelp(t *testing.T) {
	e := fleetQueueEnv(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"fleet", "queue", "--bogus"}},
		{"extra positional", []string{"fleet", "queue", "extra"}},
		{"table flag before", []string{"fleet", "--all", "queue"}},
		{"table json before", []string{"fleet", "--json", "queue"}},
		{"table crew before", []string{"fleet", "--crew", "c1", "queue"}},
		{"zero admit", []string{"fleet", "queue", "--admit", "0"}},
		{"negative admit", []string{"fleet", "queue", "--admit", "-1"}},
		{"admit not a number", []string{"fleet", "queue", "--admit", "many"}},
		{"admit missing value", []string{"fleet", "queue", "--admit"}},
		{"hold without admit", []string{"fleet", "queue", "--hold", "h1"}},
		{"own without admit", []string{"fleet", "queue", "--own", "feat/x"}},
		{"empty own", []string{"fleet", "queue", "--admit", "2", "--own", ""}},
		{"empty hold", []string{"fleet", "queue", "--admit", "2", "--hold", ""}},
		{"empty crew", []string{"fleet", "queue", "--crew", ""}},
		{"crew missing value", []string{"fleet", "queue", "--crew"}},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "crew: fleet queue [--json]") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", tc.name, code, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"fleet", "queue", "--help"}, {"fleet", "prio", "-h"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr, e); code != 0 || !strings.Contains(stdout.String(), "usage: crew fleet "+args[1]) {
			t.Errorf("%v: exit %d %q %q", args, code, stdout.String(), stderr.String())
		}
	}
}

// TestFleetQueueFlagsBind: every flag reaches the typed command.
func TestFleetQueueFlagsBind(t *testing.T) {
	p, ok := parseCrew([]string{"fleet", "queue", "--json", "--admit", "3", "--hold", "h1", "--crew", "c9", "--own", "a", "--own", "b"})
	q := p.typed.Fleet.Queue
	if !ok || p.refusal != nil || !q.JSON || q.Admit == nil || *q.Admit != 3 || str(q.Hold) != "h1" || str(q.Crew) != "c9" || !slices.Equal(q.Own, []string{"a", "b"}) {
		t.Errorf("ok %v, refusal %+v, queue %+v", ok, p.refusal, q)
	}
}

// TestFleetPrioNegativeNumber: kong reads -3 as a short flag unless the repo
// word ends flag parsing, so a negative priority binds as the second word.
// Flags after the repo are words too, so a stray one is a bad number, not a
// flag.
func TestFleetPrioNegativeNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		repo string
		n    string
	}{
		{"positive", []string{"acme/solo", "3"}, "acme/solo", "3"},
		{"negative", []string{"acme/solo", "-3"}, "acme/solo", "-3"},
	} {
		p, ok := parseCrew(append([]string{"fleet", "prio"}, tc.args...))
		if got := p.typed.Fleet.Prio; !ok || p.refusal != nil || got.Repo != tc.repo || got.N != tc.n {
			t.Errorf("%s: ok %v, refusal %+v, prio %+v", tc.name, ok, p.refusal, got)
		}
	}
}

func TestFleetPrioGrammar(t *testing.T) {
	e := fleetQueueEnv(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no words", []string{"fleet", "prio"}},
		{"no number", []string{"fleet", "prio", "acme/solo"}},
		{"extra word", []string{"fleet", "prio", "acme/solo", "1", "2"}},
		{"unknown flag", []string{"fleet", "prio", "--bogus", "acme/solo", "1"}},
		{"flag after the repo", []string{"fleet", "prio", "acme/solo", "--bogus"}},
		{"table flag before", []string{"fleet", "--all", "prio", "acme/solo", "1"}},
		{"not a number", []string{"fleet", "prio", "acme/solo", "high"}},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr, e)
		if code != 1 || stdout.Len() != 0 || stderr.String() != "crew: fleet prio <repo> <n>\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", tc.name, code, stdout.String(), stderr.String())
		}
	}
}

// TestFleetPrioReachesTheRegistryCheck: a negative number is a priority, not a
// usage refusal, so the run gets as far as asking which repo.
func TestFleetPrioReachesTheRegistryCheck(t *testing.T) {
	e := fleetQueueEnv(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"fleet", "prio", "nosuchrepo", "-3"}, &stdout, &stderr, e)
	if code != 1 || stdout.Len() != 0 || stderr.String() != "crew: fleet prio: nosuchrepo is not a registered repo\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
