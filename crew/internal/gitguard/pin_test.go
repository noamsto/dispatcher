package gitguard

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func redirectLine(dir string) string {
	return "refusing git: " + Quote(dir) + "/commondir redirects the git common dir — a main repository's git dir never has one; inspect and remove it before re-running\n"
}

func TestPinRule1(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	writeFile(t, r.common+"/commondir", "/elsewhere\n")
	want := redirectLine(r.common)
	wantRefusal(t, want, func(w io.Writer) error { return Check(r.common, "", w) })
	wantRefusal(t, want, func(w io.Writer) error { return Init(r.common, w) })
	wantRefusal(t, want, func(w io.Writer) error { return CheckCwd(r.common, r.main, w) })
	wantRefusal(t, want, func(w io.Writer) error { return PinCwd(r.common, r.main, w) })
}

// plantRedirect points the main checkout's git dir at a fresh bare repo, as git
// discovery from the main checkout then reports it as the common dir.
func plantRedirect(t *testing.T, r repo, withBaseline bool) string {
	t.Helper()
	evil := filepath.Join(r.root, "evil")
	run(t, r.root, "init", "-q", "--bare", evil)
	if withBaseline {
		if _, err := capture(func(w io.Writer) error { return Init(evil, w) }); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, r.common+"/commondir", evil+"\n")
	if got := run(t, r.main, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != evil {
		t.Fatalf("git resolves the common dir to %s, want the planted %s", got, evil)
	}
	return evil
}

func TestPinRule2aMainCheckout(t *testing.T) {
	for _, withBaseline := range []bool{true, false} {
		r := newRepo(t)
		evil := plantRedirect(t, r, withBaseline)
		want := redirectLine(r.common)
		wantRefusal(t, want, func(w io.Writer) error { return CheckCwd(evil, r.main, w) })
		wantRefusal(t, want, func(w io.Writer) error { return PinCwd(evil, r.main, w) })
		sub := filepath.Join(r.main, "sub")
		mustDo(t, os.Mkdir(sub, 0o755))
		wantRefusal(t, want, func(w io.Writer) error { return PinCwd(evil, sub, w) })
	}
}

func TestPinRule2aMainCheckoutNamedWorktrees(t *testing.T) {
	root := gitEnv(t)
	p := filepath.Join(root, "p")
	run(t, root, "init", "-q", "--bare", p)
	main := filepath.Join(p, "worktrees")
	run(t, root, "init", "-q", main)
	g := filepath.Join(main, ".git")
	writeFile(t, g+"/commondir", "../..\n")
	if got := run(t, main, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != p {
		t.Fatalf("git resolves the common dir to %s, want %s", got, p)
	}
	// The admin shape and git's own resolution both agree: only the walk tells.
	wantPass(t, func(w io.Writer) error { return PinAdmin(p, g, w) })
	want := redirectLine(g)
	wantRefusal(t, want, func(w io.Writer) error { return PinCwd(p, main, w) })
	wantRefusal(t, want, func(w io.Writer) error { return CheckCwd(p, main, w) })
}

func TestPinRule2bAdmin(t *testing.T) {
	r := newRepo(t)
	wt, admin := r.addWorktree(t, "wt")
	r.init(t)
	wantPass(t, func(w io.Writer) error { return Check(r.common, admin, w) })
	wantPass(t, func(w io.Writer) error { return CheckCwd(r.common, wt, w) })
	wantPass(t, func(w io.Writer) error { return PinCwd(r.common, wt, w) })

	evil := filepath.Join(r.root, "evil")
	run(t, r.root, "init", "-q", "--bare", evil)
	writeFile(t, admin+"/commondir", evil+"\n")
	want := "refusing git: " + Quote(admin) + "/commondir resolves to " + Quote(evil) + ", not " + Quote(r.common) + "\n"
	wantRefusal(t, want, func(w io.Writer) error { return Check(r.common, admin, w) })
	wantRefusal(t, want, func(w io.Writer) error { return PinCwd(r.common, wt, w) })
	wantRefusal(t, want, func(w io.Writer) error { _, err := Union(r.common, w); return err })

	mustDo(t, os.Remove(r.baseline()))
	wantRefusal(t, want, func(w io.Writer) error { return Init(r.common, w) })
	if entries, _ := os.ReadDir(r.common + "/crew"); len(entries) != 0 {
		t.Fatalf("refused Init left files: %v", entries)
	}
}

func TestPinRule2bShape(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	other := filepath.Join(r.root, "other")
	run(t, r.root, "init", "-q", "--bare", other)
	writeFile(t, other+"/commondir", r.common+"\n")
	wantRefusal(t, "refusing git: "+Quote(other)+" has a commondir but is not a linked-worktree admin dir of "+Quote(r.common)+"\n",
		func(w io.Writer) error { return Check(r.common, other, w) })
}
