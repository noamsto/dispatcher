package gitguard

import (
	"os"
	"slices"
	"testing"
)

func TestPairs(t *testing.T) {
	r := newRepo(t)
	r.config(t, "user.name", "unguarded")
	r.config(t, "remote.origin.url", "https://example.com/r.git")
	r.config(t, "alias.st", "status")
	r.config(t, "alias.x", "!sh")
	r.config(t, "core.pager", "less")
	r.config(t, "--add", "core.pager", "less")
	got, err := Pairs(r.common, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alias.x\n!sh", "core.pager\nless", "remote.origin.url\nhttps://example.com/r.git"}
	if !slices.Equal(got, want) {
		t.Fatalf("Pairs = %q, want %q", got, want)
	}
}

func TestUnion(t *testing.T) {
	r := newRepo(t)
	r.config(t, "extensions.worktreeConfig", "true")
	r.config(t, "core.pager", "less")
	wt, _ := r.addWorktree(t, "wt")
	run(t, wt, "config", "--worktree", "core.editor", "vim")
	// No HEAD, or a dot name: not a context.
	writeFile(t, r.common+"/worktrees/nohead/config.worktree", "[core]\n\teditor = no\n")
	writeFile(t, r.common+"/worktrees/.dot/HEAD", "ref: refs/heads/main\n")
	writeFile(t, r.common+"/worktrees/.dot/config.worktree", "[core]\n\teditor = dot\n")
	got, err := Union(r.common, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"core.editor\nvim", "core.pager\nless"}; !slices.Equal(got, want) {
		t.Fatalf("Union = %q, want %q", got, want)
	}
}
