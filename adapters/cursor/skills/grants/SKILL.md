---
name: grants
description: a path outside the worktree (--add-dir) and its refusal
---

# grants

**Task-specific dirs.** When the task body names a path outside the worktree that the worker must read, pass `--add-dir <DIR>` (repeatable) with the narrowest covering dir (e.g. `~/other-repo/docs/specs`, not `~/other-repo`). Never grant a secrets dir. The refusal text and the relay rules stay in the core protocol.

An unsafe `--add-dir` refusal is relayed verbatim and the narrowest subdir the refusal allows is the one to grant; the refusal text and the grant-root rules stay in the core protocol.
