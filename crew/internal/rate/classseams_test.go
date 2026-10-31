package rate

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// settledFixture is a sweep that succeeds and writes one row.
func settledFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	f.gh = func(args string) (string, bool) {
		switch {
		case strings.HasPrefix(args, "pr view"):
			return `{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}`, true
		case strings.Contains(args, "graphql"):
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false}]}}}}}`, true
		default:
			return `{"workflow_runs":[]}`, true
		}
	}
	return f
}

type classCheckRecorder struct {
	calls [][]any
}

func (r *classCheckRecorder) check(rows []any, _ io.Writer) { r.calls = append(r.calls, rows) }

func TestClassCheckRunsOnceAfterASuccessfulSweep(t *testing.T) {
	f := settledFixture(t)
	rec := &classCheckRecorder{}
	o := f.options()
	o.ClassCheck = rec.check
	var out, errb bytes.Buffer
	if code := Run(Request{}, f.paths, "/repo", &out, &errb, o); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("ClassCheck called %d times, want 1", len(rec.calls))
	}
	want := foldStore(f.store)
	if len(want) != 1 {
		t.Fatalf("store holds %d rows, want 1", len(want))
	}
	if !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("ClassCheck rows = %v, want %v", rec.calls[0], want)
	}
}

func TestClassCheckSkippedWhenTheSweepFails(t *testing.T) {
	f := settledFixture(t)
	if err := os.MkdirAll(f.store, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &classCheckRecorder{}
	o := f.options()
	o.ClassCheck = rec.check
	var out, errb bytes.Buffer
	if code := Run(Request{}, f.paths, "/repo", &out, &errb, o); code == 0 {
		t.Fatal("a store that cannot be opened for append must fail the sweep")
	}
	if len(rec.calls) != 0 {
		t.Errorf("ClassCheck called %d times after a failed sweep, want 0", len(rec.calls))
	}
}

func TestClassCheckSkippedBySweepAll(t *testing.T) {
	f := newSweepAllFixture(t)
	f.repo("a", "https://github.com/acme/a.git")
	rec := &classCheckRecorder{}
	o := f.options()
	o.ClassCheck = rec.check
	_, errb, code := f.run(o, Request{SweepAll: true, Roots: []string{f.root}})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	if len(f.swept) == 0 {
		t.Fatal("the loop swept nothing; the test proves nothing")
	}
	if len(rec.calls) != 0 {
		t.Errorf("ClassCheck called %d times by --sweep-all, want 0", len(rec.calls))
	}
}

func TestClassReportAppendsToTheTextReportOnly(t *testing.T) {
	store := rowA + "\n" + rowB + "\n"
	const origin = "https://github.com/acme/widgets.git"
	runWith := func(req Request, cr func([]any) []string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "ratings.jsonl")
		if err := os.WriteFile(path, []byte(store), 0o644); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Run(req, bus.Paths{}, "/repo", &out, &errb,
			Options{StorePath: path, Git: fakeGit(origin, "/repo"), ClassReport: cr})
		if code != 0 {
			t.Fatalf("exit %d, stderr: %s", code, errb.String())
		}
		return out.String()
	}

	var gotRows []any
	lines := func(rows []any) []string {
		gotRows = rows
		return []string{"Class trials: none"}
	}

	plain := runWith(Request{Report: true}, nil)
	got := runWith(Request{Report: true}, lines)
	if want := strings.TrimSuffix(plain, "\n") + "\n\nClass trials: none\n"; got != want {
		t.Errorf("report =\n%q\nwant\n%q", got, want)
	}
	if len(gotRows) != 2 {
		t.Errorf("ClassReport got %d rows, want both stored rows unscoped", len(gotRows))
	}

	if empty := runWith(Request{Report: true}, func([]any) []string { return nil }); empty != plain {
		t.Errorf("an empty ClassReport changed the report:\n%q\nwant\n%q", empty, plain)
	}

	plainJSON := runWith(Request{Report: true, JSON: true}, nil)
	if gotJSON := runWith(Request{Report: true, JSON: true}, lines); gotJSON != plainJSON {
		t.Errorf("--json changed with ClassReport set:\n%q\nwant\n%q", gotJSON, plainJSON)
	}
}
