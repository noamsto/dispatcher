package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

const workerRefusal = "crew: class: refusing to run with CREW_WORKER_ID set\n"

var classWorkerArgs = [][]string{
	{"class", "revert", "codex", "standard"},
	{"class", "promote", "codex", "standard", "gpt-5.7-terra"},
}

func TestClassRefusesAWorker(t *testing.T) {
	e, _ := decideRepo(t)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("CREW_WORKER_ID", "worker:x")
	for _, args := range classWorkerArgs {
		code, out, errs := crewRun(t, e, args...)
		if code != exitFailure || out != "" || errs != workerRefusal {
			t.Errorf("crew %v: %d %q %q", args, code, out, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "crew", "model-class-state.json")); !os.IsNotExist(err) {
		t.Errorf("state file written for a worker: %v", err)
	}
}

func TestClassRefusesAWorkerOutsideARepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	t.Setenv("CREW_WORKER_ID", "worker:x")
	e := env{getwd: func() (string, error) { return dir, nil }}
	for _, args := range classWorkerArgs {
		code, out, errs := crewRun(t, e, args...)
		if code != exitFailure || out != "" || errs != workerRefusal {
			t.Errorf("crew %v: %d %q %q", args, code, out, errs)
		}
	}
}

func TestClassGrammarRefusals(t *testing.T) {
	e, _ := decideRepo(t)
	for _, args := range [][]string{
		{"class"},
		{"class", "bogus"},
		{"class", "revert"},
		{"class", "revert", "codex"},
		{"class", "promote", "codex", "standard"},
	} {
		code, out, errs := crewRun(t, e, args...)
		if code != exitFailure || out != "" || !strings.HasPrefix(errs, "usage: crew class") {
			t.Errorf("crew %v: %d %q %q", args, code, out, errs)
		}
	}
}

// classRepo is a repo and crew c1 with the real resolver and defaults behind
// the stub settings command, and a codex catalog with a trial model.
func classRepo(t *testing.T) (e env, logPath, statePath string) {
	t.Helper()
	script, err := filepath.Abs("../adapters/core/model-resolve.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		drift.Missing(t, script, err)
	}
	defaults, err := filepath.Abs("../adapters/core/defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaults); err != nil {
		drift.Missing(t, defaults, err)
	}
	e, _ = decideRepo(t)
	repo, _ := e.getwd()
	logPath = filepath.Join(repo, ".git", "crew", "events.jsonl")

	stub := filepath.Join(t.TempDir(), "dispatch-config")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat '"+defaults+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	catalog := `{"codex":{"models":[{"id":"gpt-5.6-luna"},{"id":"gpt-5.6-terra"},{"id":"gpt-5.7-terra"},{"id":"gpt-5.6-sol"},{"id":"gpt-5.7-sol"}]}}`
	if err := os.MkdirAll(filepath.Join(data, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "crew", "model-catalog.json"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("MODEL_RESOLVE_SCRIPT", script)
	t.Setenv("DISPATCH_CONFIG_BIN", stub)
	return e, logPath, filepath.Join(data, "crew", "model-class-state.json")
}

func classDefault(t *testing.T, statePath, engine, class string) string {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]map[string]struct {
		Default string `json:"default"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("state %s: %v", b, err)
	}
	return st[engine][class].Default
}

func busKinds(t *testing.T, logPath string) (msgs, decisions []map[string]any) {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		switch row["kind"] {
		case "msg":
			msgs = append(msgs, row)
		case "decision":
			decisions = append(decisions, row)
		}
	}
	return msgs, decisions
}

func TestClassPromoteThenRevert(t *testing.T) {
	e, logPath, statePath := classRepo(t)

	if code, out, errs := crewRun(t, e, "class", "promote", "codex", "standard", "gpt-5.7-terra"); code != 0 {
		t.Fatalf("promote: %d %q %q", code, out, errs)
	}
	if got := classDefault(t, statePath, "codex", "standard"); got != "gpt-5.7-terra" {
		t.Fatalf("default after promote = %q", got)
	}
	msgs, decisions := busKinds(t, logPath)
	if len(msgs) != 1 || msgs[0]["to"] != "dispatcher:c1" || len(decisions) != 1 {
		t.Errorf("bus rows after promote: msgs %v decisions %v", msgs, decisions)
	}

	if code, out, errs := crewRun(t, e, "class", "revert", "codex", "standard"); code != 0 {
		t.Fatalf("revert: %d %q %q", code, out, errs)
	}
	if got := classDefault(t, statePath, "codex", "standard"); got != "" {
		t.Fatalf("default after revert = %q, want none (follows the seed default)", got)
	}
	msgs, decisions = busKinds(t, logPath)
	if len(msgs) != 2 || len(decisions) != 2 {
		t.Errorf("bus rows after revert: msgs %v decisions %v", msgs, decisions)
	}
}
