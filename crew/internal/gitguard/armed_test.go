package gitguard

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (r repo) armedMarker(t *testing.T) string {
	t.Helper()
	p, err := ArmedPath(r.common)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// armedThenPlant records a baseline, deletes it and plants an exec key.
func armedThenPlant(t *testing.T) repo {
	t.Helper()
	r := newRepo(t)
	r.init(t)
	if err := os.Remove(r.baseline()); err != nil {
		t.Fatal(err)
	}
	r.config(t, "core.fsmonitor", "/evil")
	return r
}

func TestInitArmedRefusesAfterDelete(t *testing.T) {
	r := armedThenPlant(t)
	wantRefusal(t, "refusing git-config baseline: "+r.baseline()+" is missing, but this repo had one (armed at "+r.armedMarker(t)+") — a dispatch never re-records it: inspect the git config, then run `crew git-baseline --accept` from your own terminal\n",
		func(w io.Writer) error { return Init(r.common, w) })
	if _, err := os.Stat(r.baseline()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Init wrote a baseline: %v", err)
	}
	wantRefusal(t, "refusing git: no git-config baseline at "+r.baseline()+", but this repo had one — a dispatch never re-records it: run `crew git-baseline --accept` from your own terminal\n",
		func(w io.Writer) error { return Check(r.common, "", w) })
}

func TestAcceptAfterArmWritesFresh(t *testing.T) {
	r := armedThenPlant(t)
	b := runBaseline(r.main, true, "yes\n", true)
	if b.code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", b.code, b.stdout, b.stderr)
	}
	if !strings.Contains(readFile(t, r.baseline()), "core.fsmonitor\n/evil") {
		t.Fatalf("baseline lacks the accepted key: %q", readFile(t, r.baseline()))
	}
	wantPass(t, func(w io.Writer) error { return Check(r.common, "", w) })
}

func TestInitFirstTimeRecordsAndArms(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.fsmonitor", "/mine")
	out, err := capture(func(w io.Writer) error { return Init(r.common, w) })
	if err != nil {
		t.Fatalf("Init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "git-config baseline: recorded") {
		t.Fatalf("no recorded note: %q", out)
	}
	if got := readFile(t, r.armedMarker(t)); got != r.common+"\n" {
		t.Fatalf("marker %q, want the realpath", got)
	}
}

func TestInitArmsExistingBaseline(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	if err := os.RemoveAll(filepath.Join(r.root, "data")); err != nil {
		t.Fatal(err)
	}
	r.init(t)
	if _, err := os.Stat(r.armedMarker(t)); err != nil {
		t.Fatalf("marker: %v", err)
	}
}

// blockMarkerDir points XDG_DATA_HOME at a regular file so arming fails.
func blockMarkerDir(t *testing.T, root string) {
	t.Helper()
	f := filepath.Join(root, "blocked")
	writeFile(t, f, "")
	t.Setenv("XDG_DATA_HOME", f)
}

func TestInitArmFailureWritesNothing(t *testing.T) {
	r := newRepo(t)
	blockMarkerDir(t, r.root)
	if out, err := capture(func(w io.Writer) error { return Init(r.common, w) }); err == nil {
		t.Fatalf("Init passed: %s", out)
	}
	if _, err := os.Stat(r.baseline()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Init wrote a baseline: %v", err)
	}
}

func TestRunBaselineListArmedMissing(t *testing.T) {
	r := armedThenPlant(t)
	runBaseline(r.main, false, "", false).want(t, 1, "",
		"git-config baseline "+r.baseline()+": missing, but this repo had one — a dispatch refuses until you run `crew git-baseline --accept` from your own terminal\n")
}

func TestRunBaselineAcceptArmFailure(t *testing.T) {
	r := newRepo(t)
	blockMarkerDir(t, r.root)
	b := runBaseline(r.main, true, "yes\n", true)
	if b.code != 1 || !strings.HasPrefix(b.stderr, "crew: ") {
		t.Fatalf("exit %d, stderr %q; want 1 and a crew: error", b.code, b.stderr)
	}
	if _, err := os.Stat(r.baseline()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--accept wrote a baseline: %v", err)
	}
}
