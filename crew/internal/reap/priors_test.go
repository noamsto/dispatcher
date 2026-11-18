package reap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

func priorsOpts(now *time.Time) Options {
	return Options{
		PID:    os.Getpid(),
		Probes: Probes{Clock: clock.Clock{Now: func() time.Time { return *now }}},
	}
}

// fakePriorsBin puts a `priors` on a PATH of its own and returns the file its
// calls are appended to.
func fakePriorsBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	body := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "priors"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func countCalls(path string) int {
	data, _ := os.ReadFile(path)
	return strings.Count(string(data), "import\n")
}

func TestImportPriorsRunsOnceThenRateLimits(t *testing.T) {
	calls := fakePriorsBin(t, "echo imported")
	store := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	o := priorsOpts(&now)
	var out, errb bytes.Buffer

	importPriors(store, o, &out, &errb)
	if n := countCalls(calls); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	for _, want := range []string{"priors import start", "imported", "priors import done rc=0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("log %q lacks %q", out.String(), want)
		}
	}

	now = now.Add(59 * time.Minute)
	importPriors(store, o, &out, &errb)
	if n := countCalls(calls); n != 1 {
		t.Errorf("calls = %d inside the window, want 1", n)
	}

	now = now.Add(2 * time.Minute)
	importPriors(store, o, &out, &errb)
	if n := countCalls(calls); n != 2 {
		t.Errorf("calls = %d after the window, want 2", n)
	}
}

func TestImportPriorsAbsentLogsOneLine(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	now := time.Unix(1_700_000_000, 0)
	var out, errb bytes.Buffer
	importPriors(t.TempDir(), priorsOpts(&now), &out, &errb)
	if got := strings.Count(out.String(), "\n"); got != 1 || !strings.Contains(out.String(), "not on PATH") {
		t.Errorf("log = %q, want one not-on-PATH line", out.String())
	}
}

func TestImportPriorsNonZeroExitIsLoggedAndStamped(t *testing.T) {
	calls := fakePriorsBin(t, "exit 3")
	store := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	var out, errb bytes.Buffer
	importPriors(store, priorsOpts(&now), &out, &errb)
	if !strings.Contains(out.String(), "priors import done rc=3") {
		t.Errorf("log = %q, want rc=3", out.String())
	}
	importPriors(store, priorsOpts(&now), &out, &errb)
	if n := countCalls(calls); n != 1 {
		t.Errorf("a failed import was retried inside the window: calls = %d", n)
	}
}

func TestImportPriorsSkipsWhileAnotherImportHoldsTheLock(t *testing.T) {
	calls := fakePriorsBin(t, "")
	store := t.TempDir()
	ld := filepath.Join(store, priorsLockDir)
	if !lock.Acquire(ld, "other-holder") {
		t.Fatal("could not take the lock for the test")
	}
	t.Cleanup(func() { lock.Release(ld) })
	now := time.Unix(1_700_000_000, 0)
	var out, errb bytes.Buffer
	importPriors(store, priorsOpts(&now), &out, &errb)
	if n := countCalls(calls); n != 0 {
		t.Errorf("calls = %d with the lock held, want 0", n)
	}
}

func TestImportPriorsTimeoutKillsAHungImport(t *testing.T) {
	fakePriorsBin(t, "exec sleep 30")
	old := priorsImportTimeout
	priorsImportTimeout = 200 * time.Millisecond
	t.Cleanup(func() { priorsImportTimeout = old })
	now := time.Unix(1_700_000_000, 0)
	var out, errb bytes.Buffer
	start := time.Now()
	importPriors(t.TempDir(), priorsOpts(&now), &out, &errb)
	if time.Since(start) > 10*time.Second || !strings.Contains(out.String(), "priors import done rc=-1") {
		t.Errorf("hung import not bounded: %v, log %q", time.Since(start), out.String())
	}
}
