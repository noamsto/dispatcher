---
name: grid-lazy
description: a grid whose roles spawn on demand (--lazy)
---

# grid-lazy

- **Lazy grid.** `--lazy` records the topology to `roles.json` in the crew dir and stamps `lazy: 1` into `WORKER_TASK.md` but creates no panes; the lead runs `dispatch --spawn-role <role>` on demand (idempotent; reads `roles.json`; pane in the caller's own window/worktree) and may `dispatch --reap-roles` to kill every role pane in its window. It rides any resolved topology, explicit or default (`deep` unless `--review` or `--plan provided`), so a default-grid `deep` takes `--lazy` alone. Do not add `--grid` "to satisfy" `--lazy`: on `deep` it adds a `reviewer` pane, never a default. `--lazy` is a usage error only when no roles resolve (`trivial`, `standard` without `--grid`/`--roles`, or with `--no-grid`). Its gain is narrow and temporary, not "lazy saves tokens": the one-time startup read of `GRID_PROTOCOL.md` + `WORKER_TASK.md` (subscription quota), an engine process per never-spawned role, and screen space. An idle eager pane already costs no repainting poll and no park cap (`GRID_PROTOCOL.md`) once `dispatch --role-watch` delivers its assignments: typed at an idle input box for claude, pi and codex roles (an undelivered one stays queued), pulled with `crew await` by cursor roles (an un-pulled one yields an `assignment_deferred` msg and the lead falls back). Use it when a role will likely sit unused for much of the run (a `deep` dispatch unlikely to reach its plan seam, a cost-sensitive/high-fan-out batch), not as a blanket default.
