package reap

import (
	"context"
	"io"
	"os"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/migrate/release"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// migratedGate stands in for the PR and issue gates of a branch whose latest
// state is migrated: crew migrate's mark vouches for it (a worker's own
// migrated row never does), the remote resume on the mark's host is confirmed
// by the branch's newest dispatch row, and the local tip is the mark's and is
// on origin, where the remote worker may already have pushed past it. It sets
// mode, rel and label; false has noted why the branch is kept.
func (r *run) migratedGate(ctx context.Context, c *candidate) bool {
	evs, err := bus.ReadEvents(r.paths.Log)
	if err != nil {
		r.keep(c, "migrated, but could not read the bus")
		return false
	}
	rel, ok := release.Active(evs, c.branch)
	if !ok {
		r.keep(c, "migrated, but no crew migrate mark vouches for it")
		return false
	}
	c.mode, c.rel, c.pr, c.label = "migrated", rel, "", "migrated to "+rel.ToHost
	lead := c.label + ", but "
	switch host := remote.Route(evs, c.branch, c.scrow); host {
	case rel.ToHost:
	case "":
		r.keep(c, lead+"no remote dispatch row on "+rel.ToHost+" yet")
		return false
	default:
		r.keep(c, lead+"its newest dispatch row is on "+host)
		return false
	}
	if !r.guardOK(gitguard.Check(r.paths.Common, r.paths.Common, r.stderr)) {
		r.keep(c, "git config drift (run `crew git-baseline`)")
		return false
	}
	if tip, _ := r.commonOutput(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+c.branch); tip != rel.Tip {
		r.keep(c, lead+"its tip is not the mark's "+rel.Tip)
		return false
	}
	origin := "origin/" + c.branch
	switch r.ancestorOf(ctx, rel.Tip, "refs/remotes/"+origin) {
	case 0:
		return true
	case 1:
		r.keep(c, lead+"its tip is not on "+origin)
	default:
		r.keep(c, lead+"could not compare its tip with "+origin+" (not fetched?)")
	}
	return false
}

// ancestorOf is `merge-base --is-ancestor oid ref`'s status from the common
// dir; a guard refusal is 1.
func (r *run) ancestorOf(ctx context.Context, oid, ref string) int {
	cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: io.Discard}, "merge-base", "--is-ancestor", oid, ref)
	if err != nil {
		return 1
	}
	return shellStatus(cmd.Run())
}

// holdsMarkTree reports whether the worktree's scratch tree, gitignored root
// scaffold included, is the mark's tree: what is left uncommitted is only the
// snapshot that travelled. It is taken exactly as crew migrate snapshot took
// it, from the mark's tip.
func (r *run) holdsMarkTree(ctx context.Context, c *candidate) bool {
	tree := r.scratchTree(ctx, c, "migrated.", c.rel.Tip, true)
	return tree != "" && tree == c.rel.Tree
}

// scratchTree is ScratchTree in a throwaway index under the crew dir, "" when
// it cannot be taken.
func (r *run) scratchTree(ctx context.Context, c *candidate, prefix, base string, ignoredScaffold bool) string {
	tmp, err := os.MkdirTemp(r.paths.Dir, prefix)
	if err != nil {
		return ""
	}
	r.clean.setTmp(tmp)
	tree, _ := ScratchTree(ctx, c.admin, c.wtpath, base, tmp+"/index", ignoredScaffold, io.Discard)
	_ = os.RemoveAll(tmp)
	r.clean.setTmp("")
	return tree
}
