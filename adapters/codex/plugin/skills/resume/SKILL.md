---
name: resume
description: Use when a worker died mid-task or a tmux-restored window appears, to restart it in place
---

# resume

To restart a worker that died mid-task, prefer `dispatch resume` to a fresh same-title `dispatch`. Run it in the worker's worktree, or anywhere in the repo with a target: `dispatch resume '#N'` (quoted: a bare `#` starts a shell comment), a Linear id, a branch, a codename or a `worker:<branch>#<session>` id. A target resolves from your own `kind:"dispatch"` rows and the dispatcher-written worktree record, then takes the cwd-bound path; refused: a live worker (naming its window), a branch dispatched on another host, an ambiguous target (listing the candidates), an unknown one. A leading word of none of those shapes that no row names is an extra prompt. Resume restores the lead's own recorded session by id, or relaunches fresh with the reorient note when it cannot be identified, and reads the engine/model/effort tuple from `WORKER_TASK.md` (don't restate it). The worker keeps its crew, posts a `resume` row and, if you are live, messages you. Resume refuses as possible tampering a worktree whose crew dir, branch or git dir no longer matches its `dispatch` record (**Trust boundary**): relay it. A worktree with no record (dispatched before resume anchoring) is refused too: re-dispatch onto its branch; the worktree carries over under `resume: true`.
A tmux-remux-restored window is not a live worker: a target proceeds when at least one pane carries the branch stamp, every such pane sits in the recorded worktree, and no lead pane among them runs an engine; with no stamped pane, `cd` into the worktree and resume there. When tmux-og already relaunched the lead's engine (a full resume refuses that pane), `dispatch resume [<target>] --rearm-roles` re-attaches the role panes (watchers, stall-watch, window stamps) and launches nothing.

**A refusal that names a live claim.** An `already claimed` abort means another crew holds the issue: relay it verbatim and do not remove the label (the core protocol's **Claim** bullet is the rule). Resume applies only to a dead worker of your own.

**Never carry an authorization in the extra prompt.** `dispatch resume`'s extra prompt has no cap, no literal-text check and no human yes, and the bus `resume` row records no `owner_auth`; an authorization travels only through a re-dispatch's `--owner-auth`.
