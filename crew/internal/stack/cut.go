package stack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// cut is the arm's `cut)`: print the oid a stacked child was last cut from, or
// with --record fetch the parent tip (or take the oid the child reports), check
// it, and record it.
func (s *stackCmd) cut(ctx context.Context, record bool, child, oid string) error {
	// A PR number resolves to its head branch; a fork's branch belongs to
	// another repo, so its cut is never recorded.
	pr := trimOne(child, "#")
	if ascii.IsDigits(pr) {
		out, err := s.p.Gh.Run(ctx, "pr", "view", pr, "--json", "headRefName,isCrossRepository")
		if err != nil {
			return s.refuse("crew: cannot read PR #%s\n", pr)
		}
		var g struct {
			HeadRefName       string `json:"headRefName"`
			IsCrossRepository *bool  `json:"isCrossRepository"`
		}
		if !decodeGh(out, &g) || g.IsCrossRepository == nil || *g.IsCrossRepository {
			return s.refuse("crew: PR #%s is from a fork; stacked cuts are recorded for same-repo branches only\n", pr)
		}
		child = g.HeadRefName
	}
	if child == "" || !wtgit.PlainBranchName(ctx, child, s.o) {
		return s.refuse("crew: '%s' is not a plain branch name\n", child)
	}

	if !record {
		cut, err := s.recordedCut(child)
		if err != nil {
			return err
		}
		if cut != "" {
			_, _ = fmt.Fprintf(s.stdout, "%s\n", cut)
			return nil
		}
		return s.refuse("crew: no recorded cut for %s in crew %s — ask the child for the parent oid "+
			"it is based on (DISPATCHER_PROTOCOL.md → Rebase coordination); never guess\n", child, s.crew)
	}

	parent, err := s.recordedParent(child)
	if err != nil {
		return err
	}
	if parent == "" {
		return s.refuse("crew: no recorded parent for %s; dispatch it with --base first\n", child)
	}

	tip := oid
	if oid != "" {
		// The child's own answer: the parent may have moved or been rewritten, so
		// only require the oid to be a commit the child's head already contains.
		childOID, err := s.fetch(ctx, child)
		if err != nil {
			return err
		}
		if oid == childOID {
			return s.refuse("crew: %s is origin/%s's own head, not a cut — not recording it\n", short(oid), child)
		}
		// The arm's `if ! cat-file -e … || ! merge-base --is-ancestor …`: any
		// failure of either, git's own error included, is the same refusal.
		if !s.isCommit(ctx, oid) || !s.isAncestor(ctx, oid, childOID) {
			return s.refuse("crew: %s is not an ancestor of origin/%s — not recording it\n", short(oid), child)
		}
	} else {
		tip, err = s.fetch(ctx, parent)
		if err != nil {
			return err
		}
		childOID, err := s.fetch(ctx, child)
		if err != nil {
			return err
		}
		if !s.isAncestor(ctx, tip, childOID) {
			return s.refuse("crew: origin/%s (%s) is not an ancestor of origin/%s — the parent moved or the child "+
				"is not rebased onto it yet; ask the child for its cut, then record it with "+
				"'crew stack cut --record %s <oid>'; never guess\n", parent, short(tip), child, child)
		}
	}

	if err := s.recordRow(ctx, child, parent, tip); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(s.stdout, "%s\n", tip)
	return nil
}

// cutRow is the stack_cut bus row, in the arm's key order.
type cutRow struct {
	TS     int64  `json:"ts"`
	CrewID string `json:"crew_id"`
	Kind   string `json:"kind"`
	Branch string `json:"branch"`
	Parent string `json:"parent"`
	Cut    string `json:"cut"`
}

// recordRow is the arm's `jq -nc … ; mkdir -p "$dir"; _bus_append "$log" …`.
// The row's keys are the arm's order; `body` is not a string, so the row is
// value-equal, not byte-exact (docs/crew-go-port.md, "Output contract").
func (s *stackCmd) recordRow(ctx context.Context, child, parent, tip string) error {
	line, err := marshalRow(cutRow{
		TS: s.p.Clock.Now().UnixMilli(), CrewID: s.crew, Kind: "stack_cut",
		Branch: child, Parent: parent, Cut: tip,
	})
	if err != nil {
		return s.refuse("crew: stack: %v\n", err)
	}
	if err := os.MkdirAll(s.paths.Dir, 0o755); err != nil {
		return s.refuse("crew: stack: %v\n", err)
	}
	if err := s.p.Append(s.paths.Log, line); err != nil {
		return s.refuse("crew: stack: %v\n", err)
	}
	return nil
}

// fetch is `_wt_fetch_origin_branch`, memoised as align's `stack_oids`: the oid
// of origin/<branch> from our own fetch.
func (s *stackCmd) fetch(ctx context.Context, branch string) (string, error) {
	if oid, ok := s.fetched[branch]; ok {
		return oid, nil
	}
	oid, err := wtgit.FetchOriginBranch(ctx, s.paths.Common, branch, s.o)
	if err != nil {
		return "", s.refuse("crew: cannot fetch origin/%s\n", branch)
	}
	s.fetched[branch] = oid
	return oid, nil
}

// isCommit is `_wt_git_common … cat-file -e <oid>^{commit}` read as the `cut)`
// arm read it: any failure is "no".
func (s *stackCmd) isCommit(ctx context.Context, oid string) bool {
	cmd, err := wtgit.CommonCommand(ctx, s.paths.Common, s.o, "cat-file", "-e", oid+"^{commit}")
	if err != nil {
		return false
	}
	return cmd.Run() == nil
}

// isAncestor is `merge-base --is-ancestor` read as the `cut)` arm read it: any
// failure is "no".
func (s *stackCmd) isAncestor(ctx context.Context, ancestor, descendant string) bool {
	ok, _ := s.anc(ctx, ancestor, descendant, false)
	return ok
}

// anc is `_stack_anc`: whether ancestor is an ancestor of descendant. With
// fatal, a git that cannot tell (any status past 1) is the arm's refusal;
// without it the caller reads "no", as the `cut)` arm's `if !` did.
func (s *stackCmd) anc(ctx context.Context, ancestor, descendant string, fatal bool) (bool, error) {
	cmd, err := wtgit.CommonCommand(ctx, s.paths.Common, s.o, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() <= 1 {
		return false, nil
	}
	if !fatal {
		return false, nil
	}
	return false, s.refuse("crew: cannot compare %s with %s\n", short(ancestor), short(descendant))
}

// hasCut is `_stack_has_cut`: the cut is a commit the fetched descendant
// contains. The descendant was fetched, so a cut it contains is present locally.
func (s *stackCmd) hasCut(ctx context.Context, cut, descendant string) (bool, error) {
	if !s.isCommit(ctx, cut) {
		return false, nil
	}
	return s.anc(ctx, cut, descendant, true)
}
