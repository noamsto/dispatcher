package reap

import "context"

// Step 4: the `--discard <branch>` block; returns the process exit code.
func (r *run) discardBranch(ctx context.Context) int {
	r.clean.setTmp("")
	r.discardPatch, r.discardSt, r.discardTree = "", "", ""
	return 1
}
