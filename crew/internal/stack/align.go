package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// prInfo is one `gh pr view` read: the arm's stack_pr_{n,head,base,state}. The
// number stays text, as bash kept it, because every message prints it verbatim;
// num is what the JSON row carries.
type prInfo struct {
	n     string
	num   int64
	head  string
	base  string
	state string
}

// layer is one step of the stack, top first, as the walk collected it.
type layer struct {
	pr     prInfo
	parent string
	// ppr is the merged parent's PR number, "" when the parent is still open
	// or the layer is the bottom one.
	ppr  string
	pprn int64
	// merged is the squash-merged case: the parent PR landed on the default
	// branch, so the layer rebases onto the default branch instead.
	merged bool
}

// align is the arm's `align)`: plan the rebase of a whole stack from its top PR
// down to the default branch. Read-only: nothing is written to the bus,
// dispatched, replied or pushed.
func (s *stackCmd) align(ctx context.Context, top string) error {
	// gh output is worker-influenceable, so every name is validated before use
	// and every oid comes from our own fetch, never from headRefOid.
	def, err := s.p.Gh(ctx, "repo", "view", "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	if err != nil {
		return s.refuse("crew: cannot read the default branch\n")
	}
	if !wtgit.PlainBranchName(ctx, def, s.o) {
		return s.refuse("crew: '%s' is not a plain branch name\n", def)
	}
	s.defaultBranch = def

	ref := trimOne(top, "#")
	if !isNumber(ref) && !wtgit.PlainBranchName(ctx, ref, s.o) {
		return s.refuse("crew: '%s' is not a plain branch name\n", top)
	}
	if err := s.safeName(def, "the default branch"); err != nil {
		return err
	}
	topPR, err := s.pr(ctx, ref)
	if err != nil {
		return err
	}
	if topPR.state != "OPEN" {
		return s.refuse("crew: PR #%s is %s, not OPEN\n", topPR.n, topPR.state)
	}

	layers, err := s.walk(ctx, topPR)
	if err != nil {
		return err
	}
	if _, err := s.fetch(ctx, def); err != nil {
		return err
	}
	for _, l := range layers {
		if _, err := s.fetch(ctx, l.pr.head); err != nil {
			return err
		}
		if !l.merged {
			if _, err := s.fetch(ctx, l.parent); err != nil {
				return err
			}
		}
	}
	return s.plan(ctx, layers)
}

// pr is `_stack_pr`: one `gh pr view` read, every refusal the arm made. A fork,
// a number that is not one, a head or base that is not a plain branch name and
// a branch lookup that answered with someone else's branch are all fatal.
func (s *stackCmd) pr(ctx context.Context, ref string) (prInfo, error) {
	out, err := s.p.Gh(ctx, "pr", "view", ref, "--json",
		"number,headRefName,headRefOid,baseRefName,state,mergedAt,isCrossRepository")
	if err != nil {
		return prInfo{}, s.refuse("crew: cannot read the PR for %s\n", ref)
	}
	var g struct {
		Number            json.Number `json:"number"`
		HeadRefName       string      `json:"headRefName"`
		BaseRefName       string      `json:"baseRefName"`
		State             string      `json:"state"`
		IsCrossRepository *bool       `json:"isCrossRepository"`
	}
	if !decodeGh(out, &g) || g.IsCrossRepository == nil || *g.IsCrossRepository {
		return prInfo{}, s.refuse("crew: the PR for %s is from a fork; stacks are planned for same-repo branches only\n", ref)
	}
	n := g.Number.String()
	if !isNumber(n) {
		return prInfo{}, s.refuse("crew: the PR for %s has no usable number\n", ref)
	}
	// The row carries the number as a JSON number, as the arm's --argjson did.
	nVal, err := strconv.ParseInt(n, 10, 64)
	if err != nil {
		return prInfo{}, s.refuse("crew: the PR for %s has no usable number\n", ref)
	}
	head, base, state := g.HeadRefName, g.BaseRefName, g.State
	if head == "" || !wtgit.PlainBranchName(ctx, head, s.o) ||
		base == "" || !wtgit.PlainBranchName(ctx, base, s.o) {
		return prInfo{}, s.refuse("crew: PR #%s has a head or base that is not a plain branch name\n", n)
	}
	if err := s.safeName(head, "PR #"+n); err != nil {
		return prInfo{}, err
	}
	if err := s.safeName(base, "PR #"+n); err != nil {
		return prInfo{}, err
	}
	if !isNumber(ref) && head != ref {
		return prInfo{}, s.refuse("crew: the PR for %s has head %s, not %s\n", ref, head, ref)
	}
	return prInfo{n: n, num: nVal, head: head, base: base, state: state}, nil
}

// walk is the arm's top-to-bottom loop: follow each layer's PR base down to the
// default branch, refusing a cycle, a parent that is neither open nor merged,
// and a parent that merged somewhere other than the default branch.
func (s *stackCmd) walk(ctx context.Context, topPR prInfo) ([]layer, error) {
	var layers []layer
	seen := map[string]bool{}
	cur := topPR
	for {
		if seen[cur.head] {
			return nil, s.refuse("crew: cycle in the stack at %s\n", cur.head)
		}
		seen[cur.head] = true
		l := layer{pr: cur, parent: s.defaultBranch}
		var next prInfo
		hasNext := false
		if cur.base != s.defaultBranch {
			l.parent = cur.base
			p, err := s.pr(ctx, cur.base)
			if err != nil {
				return nil, err
			}
			switch p.state {
			case "MERGED":
				if err := s.mergedIntoDefault(p, cur.base); err != nil {
					return nil, err
				}
				l.ppr, l.pprn, l.merged = p.n, p.num, true
			case "OPEN":
				next, hasNext = p, true
			default:
				return nil, s.refuse("crew: PR #%s for %s is %s, not merged; cannot plan the stack above it\n",
					p.n, cur.base, p.state)
			}
		} else {
			// Retargeted onto the default branch: only a merged recorded parent
			// stands in for it. Any other keeps the default branch as parent and
			// is planned unknown, ending the walk.
			rp, _, err := s.record(cur.head)
			if err != nil {
				return nil, err
			}
			if rp != "" && rp != s.defaultBranch {
				p, err := s.pr(ctx, rp)
				if err != nil {
					return nil, err
				}
				if p.state == "MERGED" {
					if err := s.mergedIntoDefault(p, rp); err != nil {
						return nil, err
					}
					l.parent, l.ppr, l.pprn, l.merged = rp, p.n, p.num, true
				}
			}
		}
		layers = append(layers, l)
		if !hasNext {
			return layers, nil
		}
		cur = next
	}
}

// mergedIntoDefault is `_stack_merged_into_default`: the merged PR just read
// must have landed on the default branch, or the stack above it cannot be planned.
func (s *stackCmd) mergedIntoDefault(p prInfo, branch string) error {
	if p.base == s.defaultBranch {
		return nil
	}
	return s.refuse("crew: PR #%s for %s merged into %s, not %s; cannot plan the stack above it\n",
		p.n, branch, p.base, s.defaultBranch)
}

// plan is the arm's bottom-to-top pass. The lowest layer with work to do is the
// only one that can act: its rebase rewrites it, so every layer above waits on
// its push.
func (s *stackCmd) plan(ctx context.Context, layers []layer) error {
	def := s.defaultBranch
	var out []string
	pending := false
	for i := len(layers) - 1; i >= 0; i-- {
		l := layers[i]
		b, p := l.pr.head, l.parent
		rp, cut, err := s.record(b)
		if err != nil {
			return err
		}
		newbase := p
		if l.merged {
			newbase = def
		}
		mode, directive, reason := "none", "", ""
		ready := false
		bottom := p == def && !l.merged && (rp == "" || rp == def)

		// Why this layer's cut cannot be trusted, if it cannot; checked even when
		// the layer looks done, since a layer above a pending one still needs it.
		doubt, differs := "", false
		switch {
		case bottom:
		case rp != "" && rp != p:
			differs = true
			doubt = fmt.Sprintf("PR base %s differs from the recorded parent %s — ask the child; never guess", p, rp)
		case cut == "":
			doubt = fmt.Sprintf("no recorded cut for %s — ask the child for the parent oid it is based on "+
				"(DISPATCHER_PROTOCOL.md → Rebase coordination); never guess", b)
		default:
			has, err := s.hasCut(ctx, cut, s.fetched[b])
			if err != nil {
				return err
			}
			if !has {
				doubt = fmt.Sprintf("recorded cut %s is not in origin/%s — ask the child; never guess", short(cut), b)
			}
		}

		switch {
		case bottom:
			reason = fmt.Sprintf("bottom layer, based on %s", def)
		// After a squash-merge, containing origin/<default> is no proof the child
		// dropped the parent's commits.
		case differs || (l.merged && doubt != ""):
			mode, reason = "unknown", doubt
		default:
			based, err := s.anc(ctx, s.fetched[newbase], s.fetched[b], true)
			if err != nil {
				return err
			}
			switch {
			case based:
				reason = fmt.Sprintf("already based on origin/%s", newbase)
			case doubt != "":
				mode, reason = "unknown", doubt
			case l.merged:
				mode = "onto"
				directive = fmt.Sprintf("new base %s; recorded old head %s; parent PR #%s squash-merged; "+
					"run: git rebase --onto origin/%s %s", def, cut, l.ppr, def, cut)
				reason = fmt.Sprintf("parent PR #%s squash-merged", l.ppr)
			default:
				onTop, err := s.anc(ctx, cut, s.fetched[p], true)
				if err != nil {
					return err
				}
				if onTop {
					mode = "ff"
					directive = fmt.Sprintf("new base %s; recorded old head %s; run: git rebase origin/%s", p, cut, p)
					reason = fmt.Sprintf("origin/%s advanced", p)
				} else {
					mode = "onto"
					directive = fmt.Sprintf("new base %s; recorded old head %s; run: git rebase --onto origin/%s %s",
						p, cut, p, cut)
					reason = fmt.Sprintf("origin/%s was rewritten", p)
				}
			}
		}

		if pending {
			if doubt != "" {
				mode, directive = "unknown", ""
				reason = fmt.Sprintf("waits for %s push; %s", p, doubt)
			} else {
				mode = "onto"
				directive = fmt.Sprintf("new base %s; recorded old head %s; run: git rebase --onto origin/%s %s",
					p, cut, p, cut)
				reason = fmt.Sprintf("waits for %s push", p)
			}
		} else if mode != "none" {
			pending = true
			ready = mode != "unknown"
		}

		row := layerRow(l.pr.num, b, p, l.pprn, l.merged && l.ppr != "", cut, mode, ready, directive, reason)
		out = append(out, row)
	}
	for _, line := range out {
		_, _ = fmt.Fprintf(s.stdout, "%s\n", line)
	}
	return nil
}

// layerRow is the arm's `jq -nc …` row: one line per layer, value-equal JSON
// (docs/crew-go-port.md, "Output contract"), with the arm's key order and its
// nulls — an empty parent PR, cut or directive is JSON null, not "".
func layerRow(pr int64, branch, parent string, ppr int64, hasPPR bool, cut, mode string, ready bool, directive, reason string) string {
	row := layerOut{
		PR: pr, Branch: branch, Parent: parent,
		Cut: nullWhenEmpty(cut), Mode: mode, Ready: ready,
		Directive: nullWhenEmpty(directive), Reason: reason,
	}
	if hasPPR {
		row.ParentPR = &ppr
	}
	// Plain strings, ints and bools always encode.
	line, _ := marshalRow(row)
	return line
}

// layerOut is the layer row, in the arm's key order.
type layerOut struct {
	PR        int64   `json:"pr"`
	Branch    string  `json:"branch"`
	Parent    string  `json:"parent"`
	ParentPR  *int64  `json:"parent_pr"`
	Cut       *string `json:"cut"`
	Mode      string  `json:"mode"`
	Ready     bool    `json:"ready"`
	Directive *string `json:"directive"`
	Reason    string  `json:"reason"`
}

// nullWhenEmpty is the arm's `(if $x == "" then null else $x end)`.
func nullWhenEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
