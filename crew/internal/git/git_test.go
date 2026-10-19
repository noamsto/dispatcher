package git_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
)

func TestExec(t *testing.T) {
	ctx, dir, g := context.Background(), t.TempDir(), git.Exec{}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if got, err := g.Run(ctx, dir, "rev-parse", "--git-dir"); got != ".git" || err != nil {
		t.Errorf("Run = %q, %v; want trimmed .git", got, err)
	}
	_, err := g.Run(ctx, dir, "rev-parse", "--verify", "nope")
	var ee *git.ExitError
	if !errors.As(err, &ee) || ee.Code != 128 || ee.Stderr == "" {
		t.Errorf("Run err = %v, want ExitError 128 with stderr", err)
	}
	if got, err := git.Origin(ctx, g, dir+"/.git"); got != "" || err != nil {
		t.Errorf("unset Origin = %q, %v; want \"\", nil", got, err)
	}
}

func TestWorktrees(t *testing.T) {
	ctx := context.Background()
	f := &gittest.Fake{Out: map[string]string{"--git-dir=/c worktree list --porcelain": "worktree /c"}}
	if got, err := git.Worktrees(ctx, f, "", "/c"); got != "worktree /c" || err != nil {
		t.Errorf("Worktrees = %q, %v", got, err)
	}
	t.Setenv("PATH", "")
	g := git.Exec{}
	var ee *git.ExitError
	if _, err := git.Worktrees(ctx, g, t.TempDir(), ""); !errors.As(err, &ee) || ee.Code != 127 {
		t.Errorf("missing git = %v, want ExitError 127", err)
	}
}

func TestRepoSlug(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acme/widgets.git":       "acme/widgets",
		"https://github.com/acme/widgets":           "acme/widgets",
		"git@github.com:acme/widgets.git":           "acme/widgets",
		"https://gitlab.example.com/group/proj.git": "group/proj",
		"":                                      "",
		"/srv/git/local.git":                    "/srv/git/local",
		"ssh://git@github.com/acme/widgets.git": "ssh://acme/widgets",
	} {
		if got := git.RepoSlug(url); got != want {
			t.Errorf("RepoSlug(%q) = %q, want %q", url, got, want)
		}
	}
}
