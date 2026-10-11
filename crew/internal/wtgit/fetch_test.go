package wtgit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/gitguard"
)

// originRepo is newRepo's checkout with a bare origin remote and feat/x pushed
// to it: enough to fetch a branch name through the anchored fetch.
func originRepo(t *testing.T) (repo, string) {
	t.Helper()
	r := newRepo(t)
	origin := filepath.Join(r.root, "origin.git")
	run(t, r.root, "init", "-q", "--bare", "-b", "main", origin)
	run(t, r.main, "remote", "add", "origin", origin)
	run(t, r.main, "push", "-q", "origin", "main")
	run(t, r.main, "checkout", "-q", "-b", "feat/x")
	run(t, r.main, "commit", "-q", "--allow-empty", "-m", "x1")
	run(t, r.main, "push", "-q", "origin", "feat/x")
	// The baseline is re-recorded now that the remote exists: remote.origin.url
	// is a pair the guard reads, and Init records a baseline once.
	if err := os.Remove(gitguard.BaselinePath(r.common)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := gitguard.Init(r.common, &stderr); err != nil {
		t.Fatalf("Init: %v\n%s", err, stderr.String())
	}
	return r, run(t, r.main, "rev-parse", "feat/x")
}

func TestPlainBranchName(t *testing.T) {
	var stderr bytes.Buffer
	o := Opts{Stderr: &stderr}
	for _, good := range []string{"main", "feat/x", "feat/x-1", "feat/x.y_z", "feat/x#1"} {
		if !PlainBranchName(context.Background(), good, o) {
			t.Errorf("PlainBranchName(%q) = false, want true (%s)", good, stderr.String())
		}
	}
	for _, bad := range []string{"", "a:b", "+refs/heads/x", "bad name", "feat/a..b", "-x", ".hidden", "feat/x/", "feat//x", "feat/x.", "feat/x.lock"} {
		if PlainBranchName(context.Background(), bad, o) {
			t.Errorf("PlainBranchName(%q) = true, want false", bad)
		}
	}
}

func TestFetchOriginBranch(t *testing.T) {
	r, oid := originRepo(t)
	var stderr bytes.Buffer
	o := Opts{Stderr: &stderr}

	got, err := FetchOriginBranch(context.Background(), r.common, "feat/x", o)
	if err != nil {
		t.Fatalf("FetchOriginBranch: %v\n%s", err, stderr.String())
	}
	if got != oid {
		t.Fatalf("fetched oid = %q, want %q", got, oid)
	}
	// The ref it wrote is the anchored remote-tracking one, never a bare ref.
	if ref := run(t, r.main, "rev-parse", "refs/remotes/origin/feat/x"); ref != oid {
		t.Fatalf("refs/remotes/origin/feat/x = %q, want %q", ref, oid)
	}
	// A second call reads the same oid from its own fetch, not a re-read.
	again, err := FetchOriginBranch(context.Background(), r.common, "feat/x", o)
	if err != nil || again != oid {
		t.Fatalf("FetchOriginBranch (again) = %q, %v", again, err)
	}
}

func TestFetchOriginBranchRefuses(t *testing.T) {
	r, _ := originRepo(t)
	before := run(t, r.main, "for-each-ref", "--format=%(refname) %(objectname)", "refs")
	for _, bad := range []string{"", "a:b", "+refs/heads/x:refs/remotes/origin/main", "--upload-pack=touch /tmp/pwned", "nope"} {
		var stderr bytes.Buffer
		got, err := FetchOriginBranch(context.Background(), r.common, bad, Opts{Stderr: &stderr})
		if err == nil || got != "" {
			t.Errorf("FetchOriginBranch(%q) = %q, %v; want a refusal", bad, got, err)
		}
	}
	// Nothing the refused names could spell was written, and no fetch option
	// reached git as a command.
	if after := run(t, r.main, "for-each-ref", "--format=%(refname) %(objectname)", "refs"); after != before {
		t.Fatalf("a refused fetch wrote a ref:\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("a fetch option reached git as a command")
	}
}

// The fetch's oid comes from its own porcelain line (#695): a ref another
// process repoints between the fetch and a later rev-parse must not be read.
func TestFetchOriginBranchIgnoresARepointedRef(t *testing.T) {
	r, oid := originRepo(t)
	var stderr bytes.Buffer
	if _, err := FetchOriginBranch(context.Background(), r.common, "feat/x", Opts{Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	// Repoint the shared ref after the fetch, as a worker with the checkout can.
	run(t, r.main, "update-ref", "refs/remotes/origin/feat/x", strings.TrimSpace(run(t, r.main, "rev-parse", "main")))
	got, err := FetchOriginBranch(context.Background(), r.common, "feat/x", Opts{Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if got != oid {
		t.Fatalf("FetchOriginBranch read the repointed ref: %q, want the fetched %q", got, oid)
	}
}
