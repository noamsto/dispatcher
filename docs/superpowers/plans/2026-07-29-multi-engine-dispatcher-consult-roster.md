# Multi-Engine Dispatcher + Deep-Consult Roster Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the dispatcher orchestrator run on codex or cursor (not just claude/opus), and let deep-tier workers pick their decomposition consultant from a roster (fable / gpt-5.6-sol / grok-4.5-high) instead of Fable-only.

**Architecture:** One fish launcher (`dispatcher.fish`) gains `--agent`/`--model`/`--effort` flags and per-engine launch lines; `DISPATCHER_PROTOCOL.md` gains a per-engine watch-loop section (claude/cursor background park + INV-1, codex blocking park); `WORKER_PROTOCOL.md`'s orchestration consult becomes a worker-picked roster; `dispatch-orchestration.md` stays the single source for concrete model versions. Spec: `docs/superpowers/specs/2026-07-29-multi-engine-dispatcher-consult-roster-design.md`.

**Tech Stack:** Nix (home-manager), fish shell, markdown protocol docs, codex CLI 0.145.0, cursor-agent 2026.07.23.

## Global Constraints

- Commit style is conventional: `type(scope): description` (recent: `feat(ai): …`, `fix(dispatch): …`, `chore(dispatcher): …`).
- **Pre-commit hooks reformat files — the FIRST `git commit` may fail** with "files were modified by this hook" (prettier on markdown, alejandra on nix). That is expected: `git add` the reformatted files and re-run the SAME commit unchanged. Fix statix/deadnix complaints in source first.
- Stage only the files named in each task. **Never** stage `flake.lock` or `home/ai/claude-code/agents/terraform-reviewer.md` (pre-existing unrelated working-tree changes).
- `--effort` valid values: `low|medium|high|xhigh`. For cursor it is accepted-and-ignored (effort lives in the cursor model id) — same convention as `dispatch.sh`.
- The protocol markdown files are read **live from the repo path** at launch time (`~/nix-config/home/ai/claude-code/…`), and `~/.claude/commands` is an out-of-store symlink into the repo — doc edits need **no rebuild**. Only Task 1 (nix-managed fish function) needs `nh home switch`.
- Before running any `nh` command, read the `nix-rebuild` skill (`~/.claude/skills/nix-rebuild/SKILL.md`) — repo workflow gotchas live there.
- Protocol docs are read by dispatchers running on **three engines** — keep engine-specific tool vocabulary inside engine-marked sentences/sections only.
- Model versions appear in exactly two places after this plan: `dispatch-orchestration.md` (reference) and `dispatcher.fish` (mechanism). Protocol prose says "per-engine defaults", not versions.

---

### Task 1: Multi-engine dispatcher launcher

**Files:**

- Modify: `home/terminal/fish/default.nix` (the `"fish/functions/dispatcher.fish".text` heredoc, currently lines 199-248)

**Interfaces:**

- Consumes: `crew id` / `crew register` / `crew deregister` (existing bus CLI); `$DISPATCH_PROFILE` (set from `osConfig.profile` by home-manager in `home/ai/claude-code/default.nix`); `codex` and `cursor-agent` on PATH (work-profile hosts).
- Produces: `dispatcher [--agent claude|codex|cursor] [--model <id>] [--effort <low|medium|high|xhigh>] [first task…]` — same crew registration and tmux badge behavior as today on all engines.

- [ ] **Step 1: Locate the current function**

The file is ~545 lines — do not read it whole. Find the heredoc:

```bash
rg -n 'dispatcher.fish' home/terminal/fish/default.nix
```

Expected: one hit at `"fish/functions/dispatcher.fish".text = ''` (line ~202). Read lines ~195-250 with `Read` offset/limit.

- [ ] **Step 2: Replace the comment header and function**

Replace the comment block directly above the heredoc:

```nix
    # Start a dispatcher session: an orchestrator that judges tasks into tier+model
    # and fans them out via `dispatch`. Bakes DISPATCHER_PROTOCOL.md (the judgment
    # rubric); default model (opus) + full MCP, since the orchestrator needs them.
```

with:

```nix
    # Start a dispatcher session: an orchestrator that judges tasks into
    # tier+engine+model and fans them out via `dispatch`. --agent picks the
    # orchestrator engine: claude (default — opus + full MCP, protocol baked as
    # system prompt) or codex/cursor (work-profile only; protocol injected as the
    # first prompt — neither CLI has an append-system-prompt flag).
```

Replace the entire function body (between the `''` markers) with:

```fish
function dispatcher --description 'Start a dispatcher session (DISPATCHER_PROTOCOL baked; --agent claude|codex|cursor)'
    # Parse flags out of $argv; the rest is the first task. The historical
    # bare `dispatcher <task>` form is unchanged — all flags are optional and
    # order-independent.
    set -l agent claude
    set -l model ""
    set -l effort ""
    set -l task
    set -l i 1
    while test $i -le (count $argv)
        switch $argv[$i]
            case --agent
                set i (math $i + 1)
                set agent $argv[$i]
            case --model
                set i (math $i + 1)
                set model $argv[$i]
            case --effort
                set i (math $i + 1)
                set effort $argv[$i]
            case '*'
                set -a task $argv[$i]
        end
        set i (math $i + 1)
    end

    # Validate before touching tmux or the crew bus. codex/cursor are
    # work-profile only — mirrors the gate in dispatch.sh ($DISPATCH_PROFILE is
    # set from osConfig.profile by home-manager).
    if not contains -- $agent claude codex cursor
        echo "dispatcher: --agent must be claude, codex, or cursor" >&2
        return 1
    end
    if test "$agent" != claude; and test "$DISPATCH_PROFILE" != work
        echo "dispatcher: --agent $agent is work-profile only" >&2
        return 1
    end
    if test -n "$effort"; and not contains -- $effort low medium high xhigh
        echo "dispatcher: --effort must be low, medium, high, or xhigh" >&2
        return 1
    end

    # Give the orchestrator window a fixed identity (accent border + name) so it
    # stands out from the per-branch worker windows it spawns. Guarded on $TMUX
    # since the dispatcher can be launched outside a tmux pane.
    if set -q TMUX
        tmux set-window-option pane-border-style "bg=#{@thm_bg},fg=#{@thm_mauve}"
        tmux set-window-option pane-active-border-style "bg=#{@thm_bg},fg=#{@thm_mauve},bold"
        tmux set-window-option pane-border-format " #[bold]dispatcher#[nobold] "
        # Same @crew_* window options `dispatch` stamps on workers, so lazytmux
        # renders the orchestrator's badge too. Fixed mauve-ish palette colour
        # (theme-independent, like the worker colourNNN codes) keeps it distinct
        # from the hashed worker colours; colour99 clears contrast on both
        # Catppuccin themes (the old colour141 washed out on Latte).
        tmux set-window-option @crew_name dispatcher
        tmux set-window-option @crew_color colour99
        # No tmux event fires on a user-option set, and lazytmux's per-tick
        # poll only runs for the session a client is viewing — so kick a
        # reflow now, else the badge won't render if you tab away at launch.
        tmux-reflow-windows (tmux display-message -p -t "$TMUX_PANE" '#{session_name}') (tmux display-message -p -t "$TMUX_PANE" '#{window_width}')
    end

    # Suffix the first task onto the session name so the /resume picker shows
    # `dispatcher: <task>` instead of a wall of identical `dispatcher` entries
    # (claude only — codex/cursor have no launch-time name flag).
    set -l session_name dispatcher
    if set -q task[1]
        set session_name "dispatcher: $task"
    end

    # Mint + export the crew id once at launch (mirrors `dispatch`), so
    # `crew register` has an id and the launched agent + every child `dispatch`
    # inherit the SAME crew. `crew id` needs no git repo (handled before
    # crew.sh's repo check).
    set -q CREW_ID; or set -gx CREW_ID (crew id)
    # Register this crew in the repo bus (non-exclusive — N crews per repo).
    # $fish_pid is this interactive shell; it blocks on the foreground agent
    # below, so its liveness tracks the whole session (stale-reclaim key).
    if git rev-parse --git-common-dir >/dev/null 2>&1
        crew register $fish_pid
    end

    set -l protocol ~/nix-config/home/ai/claude-code/DISPATCHER_PROTOCOL.md
    switch $agent
        case claude
            set -l claude_args --name "$session_name" --append-system-prompt-file $protocol
            test -z "$model"; or set -a claude_args --model $model
            test -z "$effort"; or set -a claude_args --effort $effort
            claude $claude_args $task
        case codex cursor
            # Neither CLI has --append-system-prompt-file — inject the protocol
            # as the first prompt (the same pattern dispatch.sh uses for
            # codex/cursor workers).
            set -l prompt "Read $protocol and adopt the dispatcher role for the rest of this session: judge each task into tier + engine + model + effort per the rubric, scaffold one worker per task via dispatch, and run the crew-watch loop per YOUR engine's section of the protocol (you are a $agent dispatcher). CREW_ID is already exported in this environment, so dispatch and crew calls inherit it."
            if set -q task[1]
                set prompt "$prompt First task: $task"
            end
            if test $agent = codex
                # --profile worker layers the nix-generated base MCP stack.
                # Effort high, not xhigh: blocked workers wait on a bounded
                # ~300s in-band window; xhigh turns would let blocks go stale.
                set -l codex_model gpt-5.6-sol
                set -l codex_effort high
                test -z "$model"; or set codex_model $model
                test -z "$effort"; or set codex_effort $effort
                codex --profile worker -m $codex_model -c "model_reasoning_effort=\"$codex_effort\"" --dangerously-bypass-approvals-and-sandbox "$prompt"
            else
                # kimi-k3-high: third-family model, strong agentic tool use; the
                # dispatcher's value is judgment, not turn speed. --effort is
                # accepted-and-ignored (effort lives in the cursor model id).
                set -l cursor_model kimi-k3-high
                test -z "$model"; or set cursor_model $model
                if test -n "$effort"
                    echo "dispatcher: --effort is ignored for cursor (effort lives in the model id)" >&2
                end
                cursor-agent --model $cursor_model --force --trust --approve-mcps --disable-indexing --disable-codebase-ref "$prompt"
            end
    end

    # Agent exited (the launches above are child launches, not exec) —
    # deregister so the crew bus doesn't accumulate stale entries.
    if git rev-parse --git-common-dir >/dev/null 2>&1
        crew deregister
    end
end
```

Quoting notes (load-bearing, do not "simplify"):

- `"$agent"` / `"$model"` / `"$effort"` stay **quoted** in every `test` — quoted expansion of an empty/unset fish variable is exactly one empty string, so `test` never sees a missing argument.
- `-c "model_reasoning_effort=\"$codex_effort\""` passes one argument containing literal double quotes — codex parses the value as a TOML string.
- The string contains no `${` sequence, so the surrounding nix `''` string needs no escaping.

- [ ] **Step 3: Rebuild home-manager**

Read the `nix-rebuild` skill first, then:

```bash
nh home switch
```

Expected: builds and activates. The fish function now lives at `~/.config/fish/functions/dispatcher.fish`.

- [ ] **Step 4: Verify — syntax and gates (all headless)**

```bash
fish -n ~/.config/fish/functions/dispatcher.fish; echo "syntax exit=$?"
```

Expected: no output from `fish -n`, `syntax exit=0`.

```bash
fish -c 'dispatcher --agent bogus' 2>&1; echo "exit=$?"
```

Expected: `dispatcher: --agent must be claude, codex, or cursor`, `exit=1`, and **no** agent launches (validation runs before tmux/crew).

```bash
DISPATCH_PROFILE=personal fish -c 'dispatcher --agent codex' 2>&1; echo "exit=$?"
```

Expected: `dispatcher: --agent codex is work-profile only`, `exit=1`, nothing launches.

```bash
fish -c 'dispatcher --agent claude --effort bananas' 2>&1; echo "exit=$?"
```

Expected: `dispatcher: --effort must be low, medium, high, or xhigh`, `exit=1`.

- [ ] **Step 5: Commit**

```bash
git add home/terminal/fish/default.nix
git commit -m "feat(dispatcher): --agent codex|cursor for the orchestrator session"
```

If the first commit fails because alejandra reformatted the file: `git add home/terminal/fish/default.nix` again and re-run the same commit.

---

### Task 2: Dispatcher protocol — engine watch-loop sections

**Files:**

- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md` (196 lines)
- Modify: `home/ai/claude-code/commands/dispatcher.md` (64 lines)

**Interfaces:**

- Consumes: Task 1's launcher (the inject prompt says "run the crew-watch loop per YOUR engine's section" — this task creates those sections).
- Produces: engine-marked watch-loop instructions; `DISPATCHER_PROTOCOL.md` stays the single protocol file for all three engines.

- [ ] **Step 1: Generalize the activation note**

In `DISPATCHER_PROTOCOL.md` line 5, replace:

```markdown
> **Activation:** start a dispatcher with the `dispatcher` launcher (`dispatcher` → `claude --append-system-prompt-file …/DISPATCHER_PROTOCOL.md`), which bakes this protocol as a system prompt. To promote an already-running `claude` session in place, run `/dispatcher` — it loads this protocol into context (sturdier across compaction is the baked launcher, so prefer it for long fan-outs). A plain `claude` session with neither is **not** a dispatcher.
```

with:

```markdown
> **Activation:** start a dispatcher with the `dispatcher` launcher. `--agent claude` (default) bakes this protocol as a system prompt (`claude --append-system-prompt-file …/DISPATCHER_PROTOCOL.md`); `--agent codex` / `--agent cursor` (work profile only) inject it as the session's first prompt — per-engine defaults are in `dispatch-orchestration.md` → "Orchestrator engines". To promote an already-running `claude` session in place, run `/dispatcher` — it loads this protocol into context (claude-only; the baked launcher is sturdier across compaction, so prefer it for long fan-outs). A plain agent session with neither is **not** a dispatcher. The crew-watch park primitive differs by engine — read the section for **your** engine under "Read the bus".
```

- [ ] **Step 2: Generalize the self-similarity bias note**

Replace (in the engine-lever bullet list):

```markdown
underspecified work that needs design judgement mid-flight. "Mildly ambiguous"
is not a claude ticket — most tasks have _some_ ambiguity; only route here when
the ambiguity is the hard part. You are a claude session, so watch for
self-similarity bias: do not let "claude is what I am" become "claude is the fit."
```

with:

```markdown
underspecified work that needs design judgement mid-flight. "Mildly ambiguous"
is not a claude ticket — most tasks have _some_ ambiguity; only route here when
the ambiguity is the hard part. Whatever engine you are running on, watch for
self-similarity bias: do not let "X is what I am" become "X is the fit."
```

- [ ] **Step 3: Generalize the Fable mention in the tier/model rubric**

Replace:

```markdown
the deep worker decides in its worktree whether to bring Fable in to decompose it (see `WORKER_PROTOCOL.md` → "Orchestration consult"); you do not make that call.
```

with:

```markdown
the deep worker decides in its worktree whether to bring a top-tier consultant in to decompose it, and which one (see `WORKER_PROTOCOL.md` → "Orchestration consult"); you do not make that call.
```

- [ ] **Step 4: Split the watch primitive by engine**

Replace this paragraph (the opening of "Read the bus"):

```markdown
**Arm `crew watch` as a background
Bash call** (`run_in_background`) — a held, zero-token poll that returns the instant
any worker needs you. Because it's backgrounded, your LLM loop stays **free**: the
human can add a task or ask a question with **no Esc**, and the watch keeps running.
(A background Bash task is not bound by the foreground tool timeout; `crew watch`'s
own `--timeout` is the real bound.)
```

with:

```markdown
**The watch primitive depends on your engine.**

- **claude / cursor — background park.** Arm `crew watch` as a **background** shell
  call (claude: Bash `run_in_background`; cursor: a backgrounded shell with a
  completion notification) — a held, zero-token poll that returns the instant any
  worker needs you. Because it's backgrounded, your LLM loop stays **free**: the
  human can add a task or ask a question with **no Esc**, and the watch keeps
  running. (A background task is not bound by the foreground tool timeout;
  `crew watch`'s own `--timeout` is the real bound.) INV-1 below applies to you.
- **codex — blocking park.** There is no background-notify primitive, but also no
  short foreground tool timeout: call `crew watch --timeout 270` in the
  **foreground** (`--timeout 3300` when the roster is drained — same partition rule
  as below) and let the turn block until a worker event wakes it or the park
  expires. Handle whatever it returns, then park again. Human input typed during
  the park queues and is delivered when the turn ends — expected, not a stall.
  INV-1 does not apply: a foreground call cannot double-arm.
```

- [ ] **Step 5: Scope the loop and INV-1 headers to claude/cursor**

Replace:

```markdown
**The loop is event-driven, not a foreground spin:**
```

with:

```markdown
**The claude/cursor loop is event-driven, not a foreground spin** (codex: the blocking park above _is_ your loop — skip to the wake semantics below):
```

Replace:

```markdown
**INV-1 — exactly one outstanding watch: never two, never zero.**
```

with:

```markdown
**INV-1 — exactly one outstanding watch: never two, never zero (claude/cursor only).**
```

Replace:

```markdown
**Park length — chosen only at re-arm (never in a human turn).**
```

with:

```markdown
**Park length — chosen at re-arm (claude/cursor: only at re-arm, never in a human turn; codex: at each park call).**
```

- [ ] **Step 6: Note the cursor roster-diagram render lag**

In the "Roster diagram" section, append to the end of the first paragraph (after "…never let drawing it delay a reply to a blocked worker."):

```markdown
A cursor dispatcher writes the same D2 file; the aeye carousel's auto-render hook
is driven by claude/codex plugin hooks, so the render may lag — cosmetic only,
never block a reply on it.
```

- [ ] **Step 7: Note launcher-only promotion in the slash command**

In `home/ai/claude-code/commands/dispatcher.md`, append after the final blockquote (the one ending "…prefer restarting with `dispatcher`."):

```markdown
> Codex/cursor dispatchers are launcher-only: `dispatcher --agent codex|cursor`
> (work profile) injects the protocol as the session's first prompt. This command
> promotes only claude sessions — it can only ever run inside one.
```

- [ ] **Step 8: Verify**

```bash
rg -n "codex — blocking park" home/ai/claude-code/DISPATCHER_PROTOCOL.md
rg -n "INV-1 — exactly one outstanding watch: never two, never zero \(claude/cursor only\)" home/ai/claude-code/DISPATCHER_PROTOCOL.md
rg -n "You are a claude session" home/ai/claude-code/DISPATCHER_PROTOCOL.md
rg -n "launcher-only" home/ai/claude-code/commands/dispatcher.md
```

Expected: hits for the first, second, and fourth; **zero** hits for the third.

- [ ] **Step 9: Commit**

```bash
git add home/ai/claude-code/DISPATCHER_PROTOCOL.md home/ai/claude-code/commands/dispatcher.md
git commit -m "feat(dispatcher): per-engine crew-watch sections in the protocol"
```

Prettier may reformat the markdown on first commit — re-stage and re-run the same commit.

---

### Task 3: Worker protocol — consult roster + `consult_engine` metric

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (123 lines)

**Interfaces:**

- Consumes: the read-only codex MCP already injected into deep claude workers (`dispatch.sh` → `xreview_mcp`, config at `~/.config/claude-code/mcp-codex.json`, work-profile gated); `cursor-agent` on work-profile hosts.
- Produces: the consult roster (consumed by deep claude workers at the plan seam); the metrics record shape `{"consulted":…,"consult_engine":…,"plan_critic_first_pass":…,"rework_count":…,"review_high":…,"review_mode":…}` (Task 4 references it).

- [ ] **Step 1: Update the deep pipeline bullet**

Replace:

```markdown
- **deep** — run `spec-plan-critic` with `{ tier: 'deep', ... }` (spec + spec-critic, then — see **Orchestration consult** — an optional Fable decomposition seeds plan + plan-critic), then execute, then the **fast deterministic gate**, then the code-review gate (one parallel review batch, reconciled once, then a **conditional** second re-review), then `/deslop` + push + PR.
```

with:

```markdown
- **deep** — run `spec-plan-critic` with `{ tier: 'deep', ... }` (spec + spec-critic, then — see **Orchestration consult** — an optional consultant decomposition seeds plan + plan-critic), then execute, then the **fast deterministic gate**, then the code-review gate (one parallel review batch, reconciled once, then a **conditional** second re-review), then `/deslop` + push + PR.
```

- [ ] **Step 2: Update the two "Fable consult" mentions in Plan of record**

Replace:

```markdown
- **`plan: provided`** — the dispatcher wrote the plan into the task doc (root cause/mechanism, explicit file list, named approach, acceptance criteria). The doc **is** your plan of record. Do **not** run the `spec-plan-critic` **plan** phase (a **deep** worker still runs its spec-critic / Fable consult — see **Scope** below).
```

with:

```markdown
- **`plan: provided`** — the dispatcher wrote the plan into the task doc (root cause/mechanism, explicit file list, named approach, acceptance criteria). The doc **is** your plan of record. Do **not** run the `spec-plan-critic` **plan** phase (a **deep** worker still runs its spec-critic / orchestration consult — see **Scope** below).
```

Replace:

```markdown
- **Scope.** The skip applies to the **plan** phase only. A **deep** worker may skip the plan-critic under these rules but **never** its spec-critic / Fable consult — deep is chosen when the _framing_ needs adversarial pressure, which a task doc doesn't settle.
```

with:

```markdown
- **Scope.** The skip applies to the **plan** phase only. A **deep** worker may skip the plan-critic under these rules but **never** its spec-critic / orchestration consult — deep is chosen when the _framing_ needs adversarial pressure, which a task doc doesn't settle.
```

- [ ] **Step 3: Rewrite the consult section header + step 2 as a roster**

Replace the section intro:

```markdown
## Orchestration consult (deep only)

Before the plan phase, decide **once** whether to bring Fable in to decompose the task. This decision is made in the worktree (where the code is), never at dispatch time.
```

with:

```markdown
## Orchestration consult (deep only)

Before the plan phase, decide **once** whether to bring a top-tier consultant in to decompose the task — and if so, **which one**. Both decisions are made in the worktree (where the code is), never at dispatch time.
```

Replace step 2 in full:

```markdown
2. **If it trips, consult Fable.** Spawn an **ephemeral** subagent with the Agent tool, `model: fable`, running in this worktree. Prompt it to read `WORKER_TASK.md` and the relevant code and write `DECOMPOSITION.md` at the worktree root using this exact structure — `components` (each a stable id + one-line + `boundaries` may/​must-not-touch + `risk` tag), `ordering` (dependency order, `∥` for parallel-safe), `interfaces` (contracts that must stay stable across the split). It authors the **decomposition, not the plan** — no plan-schema step tags. `DECOMPOSITION.md` must **not** name its author (the plan-critic reads it author-less; rule 2 discipline).
```

with:

```markdown
2. **If it trips, pick a consultant and consult.** Judge the fit per task and say which you picked and why (one line, in the plan seam) — neutral fit → **fable**:

   | consultant                              | mechanism                                                                                                                                                                                                     | lean                                                                              |
   | --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
   | **fable** (default)                     | **ephemeral** subagent via the Agent tool, `model: fable`, running in this worktree — it writes `DECOMPOSITION.md` itself                                                                                     | hardest decompositions; the only consultant with in-worktree write access         |
   | **gpt-5.6-sol** (work profile)          | the read-only **codex MCP** injected into deep workers — ask it for the decomposition, then write `DECOMPOSITION.md` yourself from its response                                                               | a non-claude-family decomposition, diverse from the opus planner that consumes it |
   | **cursor-grok-4.5-high** (work profile) | one-shot in this worktree: `cursor-agent -p --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model cursor-grok-4.5-high '<prompt>'` — write `DECOMPOSITION.md` yourself from stdout | third-family perspective                                                          |

   Whichever runs, the ask is identical: read `WORKER_TASK.md` and the relevant code and produce `DECOMPOSITION.md` at the worktree root using this exact structure — `components` (each a stable id + one-line + `boundaries` may/​must-not-touch + `risk` tag), `ordering` (dependency order, `∥` for parallel-safe), `interfaces` (contracts that must stay stable across the split). It authors the **decomposition, not the plan** — no plan-schema step tags. `DECOMPOSITION.md` must **not** name its author **or the consulting engine** (the plan-critic reads it author-less; rule 2 discipline).
```

(Note: the source line contains a zero-width space in "may/​must-not-touch" — match loosely if exact replacement fails, and preserve the structure text as written.)

- [ ] **Step 4: Update the fallback step**

Replace:

```markdown
3. **Fallback — a should, not a blocker.** If Fable refuses, times out, or is unavailable, drop the consult and proceed on the plain `writing-plans` path — **byte-identical to a non-consulted deep worker**. Never fail the worker on a missing consult (same rule as the codex-diverse reviewer, "Code review gate").
```

with:

```markdown
3. **Fallback — a should, not a blocker.** If the consultant refuses, times out, or is unavailable — codex/cursor consults are **work-profile only**, so a personal-profile worker's roster is fable-only — drop the consult and proceed on the plain `writing-plans` path, **byte-identical to a non-consulted deep worker**. When a codex/cursor consult was the pick and it failed, you may retry **once** with fable before dropping. Never fail the worker on a missing consult (same rule as the codex-diverse reviewer, "Code review gate").
```

- [ ] **Step 5: Add `consult_engine` to the metrics record**

Replace the record template:

```markdown
crew msg "worker:$(git branch --show-current)" "metrics:$CREW_ID" '{"consulted":<true|false>,"plan_critic_first_pass":"<accept|revise|reject|null>","rework_count":<int>,"review_high":<int>,"review_mode":"<full|downgraded|none>"}'
```

with:

```markdown
crew msg "worker:$(git branch --show-current)" "metrics:$CREW_ID" '{"consulted":<true|false>,"consult_engine":"<fable|codex|cursor|null>","plan_critic_first_pass":"<accept|revise|reject|null>","rework_count":<int>,"review_high":<int>,"review_mode":"<full|downgraded|none>"}'
```

Replace the field prose:

```markdown
(`$CREW_ID` is the `crew_id:` from `WORKER_TASK.md`.) `consulted` = whether the Fable consult ran (deep only; `false` otherwise).
```

with:

```markdown
(`$CREW_ID` is the `crew_id:` from `WORKER_TASK.md`.) `consulted` = whether the orchestration consult ran (deep only; `false` otherwise). `consult_engine` = which consultant ran it — `fable` (subagent), `codex` (gpt-5.6-sol via the read-only MCP), `cursor` (grok-4.5-high one-shot) — `null` whenever `consulted` is `false`.
```

- [ ] **Step 6: Verify**

```bash
rg -n "Fable" home/ai/claude-code/WORKER_PROTOCOL.md; echo "exit=$?"
```

Expected: **zero** hits, `exit=1` (every capital-F "Fable" is now either lowercase `fable` in the roster or "orchestration consult").

```bash
rg -cn "consult_engine" home/ai/claude-code/WORKER_PROTOCOL.md
```

Expected: `2` (record template + field prose).

- [ ] **Step 7: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): consultant roster for the deep orchestration consult"
```

Prettier may reformat on first commit — re-stage and re-run the same commit.

---

### Task 4: Orchestration reference doc — orchestrator table + consult paragraph

**Files:**

- Modify: `home/ai/claude-code/dispatch-orchestration.md` (87 lines)

**Interfaces:**

- Consumes: Task 1's launcher defaults, Task 3's metrics record shape.
- Produces: the "Orchestrator engines" table — the single reference location for dispatcher model versions (Task 2's activation note points here).

- [ ] **Step 1: Add the Orchestrator engines section**

Insert a new section immediately before the line `## Three orthogonal levers`:

```markdown
## Orchestrator engines (dispatcher session)

The dispatcher itself can run on any engine — `dispatcher --agent claude|codex|cursor`
(work-profile gated, same as workers). Orchestrator defaults — bump this table when a
model ships:

| engine | model                   | effort                                                                                 |
| ------ | ----------------------- | -------------------------------------------------------------------------------------- |
| claude | opus (settings default) | settings default                                                                       |
| codex  | **gpt-5.6-sol**         | **high** — not xhigh: blocked workers wait on a bounded ~300s in-band window           |
| cursor | **kimi-k3-high**        | fixed in the model id (no knob; `--model` overrides: composer-2.5, cursor-grok-4.5-\*) |

claude bakes `DISPATCHER_PROTOCOL.md` as a system prompt; codex/cursor inject it as
the first prompt (neither CLI has an append-system-prompt flag). The judging rubric
is identical across engines; the crew-watch park primitive is not — see
`DISPATCHER_PROTOCOL.md` → "Read the bus".
```

- [ ] **Step 2: Update the Orchestration consult paragraph**

Replace:

```markdown
**Orchestration consult (worker-side, deep).** Decomposition help from Fable is decided **in the worker's worktree** at the plan seam, not by the dispatcher — the dispatcher's only lever is tiering the task `deep` (its existing "architectural / wide-blast" signal). See `WORKER_PROTOCOL.md` → "Orchestration consult". Every deep worker emits an outcome-metrics record to the bus at finish:
`crew msg worker:<branch> metrics:<crew_id> '{"consulted":…,"plan_critic_first_pass":…,"rework_count":…,"review_high":…}'`.
It rides `crew msg` (no `crew.sh` change) and never wakes the dispatcher. Consulted vs non-consulted deep workers are the A/B for whether the Fable lever pays — the counterfactual #86's oracle gate needs. Read it offline: `crew log <crew> | jq 'select(.to|startswith("metrics:"))'`.
```

with:

```markdown
**Orchestration consult (worker-side, deep).** Decomposition help from a top-tier consultant — **fable** (default), **gpt-5.6-sol** via the read-only codex MCP, or **cursor-grok-4.5-high** via a `cursor-agent -p` one-shot — is decided **in the worker's worktree** at the plan seam (whether _and_ which), not by the dispatcher — the dispatcher's only lever is tiering the task `deep` (its existing "architectural / wide-blast" signal). Codex/cursor consults are work-profile only. See `WORKER_PROTOCOL.md` → "Orchestration consult". Every deep worker emits an outcome-metrics record to the bus at finish:
`crew msg worker:<branch> metrics:<crew_id> '{"consulted":…,"consult_engine":…,"plan_critic_first_pass":…,"rework_count":…,"review_high":…}'`.
It rides `crew msg` (no `crew.sh` change) and never wakes the dispatcher. Consulted vs non-consulted deep workers are the A/B for whether the consult lever pays — `consult_engine` splits it by consultant — the counterfactual #86's oracle gate needs. Read it offline: `crew log <crew> | jq 'select(.to|startswith("metrics:"))'`.
```

- [ ] **Step 3: Verify**

```bash
rg -n "kimi-k3-high" home/ai/claude-code/dispatch-orchestration.md
rg -n "consult_engine" home/ai/claude-code/dispatch-orchestration.md
rg -n "Fable" home/ai/claude-code/dispatch-orchestration.md; echo "exit=$?"
```

Expected: one hit each for the first two; zero hits (`exit=1`) for the third — capital-F "Fable" survives only in `DISPATCHER_PROTOCOL.md`'s historical escalation note ("↑Fable to escalate"), which this plan deliberately leaves.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/dispatch-orchestration.md
git commit -m "docs(dispatch): orchestrator engine table + consult roster reference"
```

Prettier may reformat on first commit — re-stage and re-run the same commit.

---

## Post-plan smoke (human, not a task)

On a work-profile host, in tmux, in a git repo:

```bash
dispatcher --agent codex
# session opens with the protocol injected; give it one small task
# verify: it dispatches a worker, then blocks on `crew watch` (foreground park)

dispatcher --agent cursor
# verify: kimi-k3-high session, background watch arms, badge renders
```
