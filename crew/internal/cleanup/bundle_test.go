package cleanup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// bundleScenario archives a branch with two pushed commits and one unpushed
// commit, deletes the remote branch, and checks the bundle restores in a
// fresh clone of the default branch.
func bundleScenario(t *testing.T, prep func(f *fx)) {
	t.Helper()
	f := newFx(t)
	bare := filepath.Join(f.root, "origin.git")
	runGit(t, f.root, "init", "-q", "--bare", "-b", "main", bare)
	runGit(t, f.main, "push", "-q", bare, "main")
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(f.wt, name+".txt"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, f.wt, "add", name+".txt")
		runGit(t, f.wt, "commit", "-q", "-m", name)
	}
	runGit(t, f.wt, "push", "-q", bare, branch)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/"+branch, "refs/heads/"+branch)
	f.dirty(true)
	prep(f)

	f.input = "r\n"
	if rc := f.run(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, f.stderr.String())
	}
	dirs := f.archiveDirs()
	if len(dirs) != 1 {
		t.Fatalf("archive dirs %v", dirs)
	}
	var meta info
	b, err := os.ReadFile(filepath.Join(dirs[0], "info.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}

	runGit(t, bare, "update-ref", "-d", "refs/heads/"+branch)
	runGit(t, bare, "reflog", "expire", "--expire=now", "--all")
	runGit(t, bare, "gc", "-q", "--prune=now")

	fresh := filepath.Join(f.root, "fresh")
	runGit(t, f.root, "clone", "-q", bare, fresh)
	bundle := filepath.Join(dirs[0], "commits.bundle")
	runGit(t, fresh, "bundle", "verify", bundle)
	runGit(t, fresh, "fetch", "-q", bundle, "refs/heads/"+branch+":refs/heads/restored")
	if got := runGit(t, fresh, "rev-parse", "restored"); got != meta.Tip {
		t.Fatalf("restored %s, want %s", got, meta.Tip)
	}
}

func TestBundleSelfContainedAfterRemoteBranchDeleted(t *testing.T) {
	bundleScenario(t, func(*fx) {})
}

func TestBundleSelfContainedWithoutOriginHead(t *testing.T) {
	bundleScenario(t, func(f *fx) {
		runGit(t, f.main, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	})
}

func TestBundleSelfContainedWithoutDefaultRef(t *testing.T) {
	bundleScenario(t, func(f *fx) {
		runGit(t, f.main, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
		runGit(t, f.main, "update-ref", "-d", "refs/remotes/origin/main")
	})
}
