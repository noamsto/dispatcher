# Worker protocol: rebase

Phase file of `WORKER_PROTOCOL.md`: it binds like the core. References to "this protocol" or a section of it resolve through the core's Phase files table.

## Base ref: rebase directives

Rebase only your own branch, only when the dispatcher directs; name refs explicitly (`git rebase origin/<base>` hits the snippet's `$base`, a merge-base id):
- First `git fetch origin "+refs/heads/<new-base>:refs/remotes/origin/<new-base>"`.
- Every directive carries your cut point = recorded old head (fast-forward too): parent oid the dispatcher recorded at your (re)base, whether or not the parent was told to rebase; never a fresh `gh pr view` (rewritten tip).
- Squash-merged parent: `git rebase --onto "origin/<new-base>" <cut-oid>`; cut commit not local → `git fetch origin pull/<parent-PR>/head` (directive names it).
- Parent's push rewrote history: `git rebase --onto "origin/<parent>" <recorded old head>`.
- Fast-forward only: `git rebase "origin/<parent>"`, gated on `git merge-base --is-ancestor <old head> "origin/<parent>"`.
- Rewrite header `base:` (header line only; `sed -i` is GNU-only): `sed '1,/^$/s|^base: .*|base: <new-base>|' WORKER_TASK.md > WORKER_TASK.md.tmp && mv WORKER_TASK.md.tmp WORKER_TASK.md`. Open PR on old parent → `gh pr edit --base <new-base>`.
- Re-run the fast deterministic gate (+ targeted re-review per `EVIDENCE_REVIEW.md` if conflict resolution changed behavior), continue. Branch on origin → next push (rule 4 order) is `git push --force-with-lease origin <own-branch>`.
- No cascading rebase; never touch another layer's branch.
