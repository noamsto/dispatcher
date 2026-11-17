package reap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

type keepDoc struct {
	Keeps []Keep `json:"keeps"`
}

func decodeKeeps(t *testing.T, s string) keepDoc {
	t.Helper()
	var d keepDoc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("stdout is not one JSON document: %q: %v", s, err)
	}
	return d
}

func TestJSONDryRunRecordsKeepWithPlainReason(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "OPEN"

	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	_, plain, ok := strings.Cut(f.stdout.String(), "keeping "+rb+" — ")
	if !ok {
		t.Fatalf("no keep note in %q", f.stdout.String())
	}
	plain, _, _ = strings.Cut(plain, "\n")

	if rc := f.reap(f.main, Request{DryRun: true, JSON: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	want := Keep{Repo: f.paths.Common, Crew: "c1", Branch: rb, Reason: plain}
	got := decodeKeeps(t, f.stdout.String())
	if len(got.Keeps) != 1 || got.Keeps[0] != want {
		t.Fatalf("keeps = %+v, want [%+v]", got.Keeps, want)
	}
	if strings.Count(f.stdout.String(), "\n") != 1 {
		t.Fatalf("stdout has more than the document: %q", f.stdout.String())
	}
}

func TestJSONDryRunNoKeepsIsEmptyArray(t *testing.T) {
	f := newFx(t)
	f.bus(status("working", "", 0))
	if rc := f.reap(f.main, Request{DryRun: true, JSON: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if got := strings.TrimSpace(f.stdout.String()); got != `{"keeps":[]}` {
		t.Fatalf("stdout = %q", got)
	}
}

func TestJSONDryRunTakesNoReapLock(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "OPEN"
	if err := os.Mkdir(filepath.Join(f.paths.Dir, "reap.lock.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rc := f.reap(f.main, Request{DryRun: true, JSON: true, NoWait: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if got := decodeKeeps(t, f.stdout.String()); len(got.Keeps) != 1 {
		t.Fatalf("keeps = %+v, want the lock ignored", got.Keeps)
	}
}

func TestFleetJSONAggregatesKeeps(t *testing.T) {
	f := newFleetFixture(t)
	repoA := f.repo("alpha", true)
	repoB := f.repo("bravo", true)
	_ = f.repo("charlie", false) // skipped: its line must not reach stdout
	o := Options{FleetReap: func(_ context.Context, req Request, paths bus.Paths, _ string, stdout, _ io.Writer) int {
		req.keeps.keeps = append(req.keeps.keeps, Keep{Repo: paths.Common, Branch: "feat/x", Reason: "PR OPEN"})
		_, _ = io.WriteString(stdout, "must be discarded\n")
		return 0
	}}

	out, _, code := f.runReq(Request{DryRun: true, JSON: true}, o)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := decodeKeeps(t, out)
	if len(got.Keeps) != 2 || got.Keeps[0].Repo != repoA || got.Keeps[1].Repo != repoB {
		t.Fatalf("keeps = %+v, want one per repo %s %s", got.Keeps, repoA, repoB)
	}
	if strings.Count(out, "\n") != 1 || bytes.Contains([]byte(out), []byte("skipped")) {
		t.Fatalf("stdout carries more than the document: %q", out)
	}
}
