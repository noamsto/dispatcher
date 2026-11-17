package main

import (
	"strings"
	"testing"
)

func TestMigrateGrammarRefusals(t *testing.T) {
	e, _ := decideRepo(t)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, args := range [][]string{
		{"migrate"},
		{"migrate", "go"},
		{"migrate", "stop"},
		{"migrate", "stop", "feat/x", "--timeout", "-1"},
		{"migrate", "stop", "feat/x", "--crew"},
		{"migrate", "snapshot", "feat/x", "extra"},
		{"migrate", "mark", "feat/x", "--mode", "now", "--tip", sha, "--tree", sha},
		{"migrate", "mark", "feat/x", "--to-host", "h", "--tip", sha, "--tree", sha},
		{"migrate", "mark", "feat/x", "--to-host", "h", "--mode", "now", "--tree", sha},
		{"migrate", "mark", "feat/x", "--to-host", "h", "--mode", "now", "--tip", sha},
		{"migrate", "mark", "feat/x", "--to-host", "h", "--mode", "now", "--tip", sha, "--tree", sha, "--bogus"},
	} {
		if code, out, errs := crewRun(t, e, args...); code != exitFailure || out != "" || errs != migrateUsage+"\n" {
			t.Errorf("crew %v: %d %q %q", args, code, out, errs)
		}
	}
}

func TestMigrateRefusesAWorker(t *testing.T) {
	e, _ := decideRepo(t)
	t.Setenv("CREW_WORKER_ID", "worker:feat/x#s1-1")
	for _, args := range [][]string{
		{"migrate", "stop", "feat/x"},
		{"migrate", "snapshot", "feat/x"},
		{"migrate", "mark", "feat/x", "--to-host", "h", "--mode", "now", "--tip", "x", "--tree", "x"},
	} {
		code, out, errs := crewRun(t, e, args...)
		if code != exitFailure || out != "" || !strings.HasPrefix(errs, "crew migrate: refusing "+args[1]+" feat/x — this is a worker or role session") {
			t.Errorf("crew %v: %d %q %q", args, code, out, errs)
		}
	}
}

func TestMigrateHelpRoutes(t *testing.T) {
	for _, args := range [][]string{{"migrate", "--help"}, {"migrate", "mark", "--help"}, {"help", "migrate"}} {
		code, out, errs := crewRun(t, env{}, args...)
		if code != 0 || errs != "" || !strings.HasPrefix(out, "usage: crew migrate stop ") {
			t.Errorf("crew %v: %d %q %q", args, code, out, errs)
		}
	}
}
