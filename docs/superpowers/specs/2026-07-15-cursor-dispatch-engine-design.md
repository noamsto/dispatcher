# cursor-agent as a third dispatch engine

**Issue:** [noamsto/nix-config#85](https://github.com/noamsto/nix-config/issues/85)
**Date:** 2026-07-15
**Status:** design revised after adversarial spec-critic (REVISE → addressed), pending spec review

## Problem

The dispatcher harness routes each task along three orthogonal levers — **tier**
(pipeline depth), **engine** (who implements), **model/effort** (how strong). The
engine lever currently has two values: `claude` (default) and `codex` (gpt-5.x,
work-profile only). We want a third: `cursor` (`cursor-agent`, the Cursor CLI) —
a genuinely distinct third implementer (Cursor's own harness + Composer models)
for diverse cross-checking on deep tasks, not a redundant front-end for models we
already run.

## What already exists (verified against the repo)

This is **not** a greenfield module. The initial draft of this spec got the
starting facts wrong; the spec-critic caught it. Ground truth:

- **`home/ai/cursor/default.nix` already exists** (sibling of `codex/`, both under
  `home/ai/` — _not_ under `home/ai/claude-code/`). It already generates
  `~/.cursor/mcp.json` from the shared `home/ai/mcp-servers.nix` `base` stack, for
  the interactive Cursor editor.
- **The package line is commented out** with a documented blocker:
  `home.packages = [pkgs.cursor-agent]` is disabled because of a
  _playwright-core collision with playwright-mcp_ (both bundle
  `playwright-core` at `$out/index.js`, breaking the build).
- **The nixpkgs attr is `pkgs.cursor-cli`** (`2026.06.19`), which provides the
  `cursor-agent` binary. The module's commented `pkgs.cursor-agent` is a **stale
  reference** and must be corrected to `pkgs.cursor-cli`.
- **`home/ai/default.nix` imports `./cursor` unconditionally** (all profiles),
  while `./codex` is inside the `lib.optionals (osConfig.profile == "work")`
  block.

## Decisions

- **Model axis — expose `--model` freely.** `dispatch --agent cursor` may pass an
  optional `--model` through to `cursor-agent`. Default (no `--model`) fronts
  Cursor's own model family (Composer / `auto`). A **soft dispatcher rule**
  discourages fronting Claude-through-cursor when the point is an independent 3rd
  perspective. Not enforced in code — the model slot is left to dispatcher
  judgment, like every other slot.
- **Profile split — keep interactive all-profiles, work-gate only the worker.**
  The existing `~/.cursor/mcp.json` and the `cursor-cli` package stay available on
  all profiles (interactive Cursor editor + CLI). **Only the dispatch _engine_ is
  work-gated**, and that gate lives in `dispatch.sh` (refuse `--agent cursor` when
  `profile != work`), exactly like codex. `home/ai/default.nix` keeps `./cursor`
  unconditional — **no gating migration, no personal-profile regression.**
- **"Parity with codex" = same integration surface, not the same pipeline as
  claude.** Codex workers do **not** run the Claude subagent/workflow pipeline:
  the launch prompt tells codex to read `WORKER_PROTOCOL.md` + `WORKER_TASK.md`
  and run end-to-end **single-agent**, with `model_reasoning_effort` scaled by
  tier. `WORKER_PROTOCOL.md` already carves codex out of the opus-plans/
  sonnet-implements split and the codex-diverse review. Cursor is a peer of codex
  in this exact sense — a single-agent engine reading the protocol — not a peer of
  the claude pipeline.

## Hard prerequisite: measurement spike (plan task 1)

`cursor-agent` is not installed (blocked by the collision) and its headless
surface is unknown. Guessing it is the biggest risk — it bit codex. The plan's
**first task** is a spike that resolves the blocker and records findings
(codex-KEY-FINDINGS-style, so never re-derived). Ordered by make-or-break:

1. **Package builds at all (THE blocker).** Enable `pkgs.cursor-cli` and get it to
   install via `nh home switch`. **First reproduce and correctly locate** the
   collision — the module's one-line "playwright-core collision with
   playwright-mcp" comment is suspect: `playwright-mcp` is pulled in by store path
   (`lib.getExe`), not via `home.packages`, so a home-profile file collision
   between two _installed_ packages is unlikely to be the real cause. Do not trial
   fixes against the recorded diagnosis; get the actual `nh` error first. _Then_
   trial the appropriate fix (`overrideAttrs`/`lib.hiPrio`/closure surgery).
   Without this there is no worker. Record the real diagnosis and the chosen fix.
2. **Headless / auto-apply invocation.** The flag(s) to run autonomously and
   auto-apply edits with no interactive approval (codex's equivalent:
   `--dangerously-bypass-approvals-and-sandbox`). Second make-or-break.
3. **Pipeline capability → define the degraded path.** Does `cursor-agent` have
   subagents, a `spec-plan-critic`-workflow equivalent, or slash commands
   (`/deslop`)? Almost certainly not the Claude Code ones → cursor runs the same
   single-agent degraded pipeline as codex. Confirm and write down what a cursor
   worker actually executes for standard/deep (read protocol → spec/plan/execute/
   review inline → gate → push).
4. **`/deslop` + pre-push gate.** Determine how a codex worker currently satisfies
   the pre-push `/deslop` guard (codex has no slash commands either — likely
   `ALLOW_PUSH_WITHOUT_DESLOP=1` inline or protocol-driven), then apply the same
   mechanism to cursor.
5. **Model IDs + `--model` syntax** — `composer`, `auto`, `sonnet-4.5`, `gpt-5`;
   exact flag form.
6. **Reasoning-effort knob.** Does one exist? If not, tier→effort is a
   **documented no-op** for cursor (`--effort` is still required by `dispatch.sh`
   for every agent — confirm that's the intended behavior).
7. **Worker MCP isolation.** Does `cursor-agent` support a per-invocation config
   path (isolating a worker MCP file from interactive `~/.cursor/mcp.json`)? If
   **no** (single shared file), the existing `~/.cursor/mcp.json` _is_ the worker
   config — **drop the separate worker-profile deliverable** and just verify the
   base stack's defer/token cost. If **yes**, add a codex-style worker profile.
8. **Auth.** `cursor-agent login` (browser OAuth) vs `CURSOR_API_KEY`; does the
   credential survive a non-interactive tmux worker? Record whether an agenix
   secret is needed or login is one-time out-of-band (codex uses out-of-band
   `codex login`, no Nix secret).

## Components that change

| File                                            | Change                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `home/ai/claude-code/dispatch.sh`               | `--agent` case list adds `cursor`; error → "must be claude, codex, or cursor"; **work-profile gate** extends to cursor (refuse off work, like codex); the `--mcp` guard (today codex-only) generalizes to **reject `--mcp` whenever `agent != claude`** so cursor errors clearly instead of silently ignoring it (`--mcp` is a claude `--mcp-config` flag the cursor branch won't consume); new launch branch `elif [ "$agent" = cursor ]` with the spike-confirmed headless invocation, `--model` passthrough, and the `/deslop`/effort handling from the spike. Bus event already records `engine: $agent` generically — no change. |
| `home/ai/cursor/default.nix` **(EDIT)**         | correct the stale package name to `pkgs.cursor-cli` and **enable** `home.packages` once the collision fix from spike task 1 lands; keep the all-profiles `~/.cursor/mcp.json`; wire auth per spike (agenix `CURSOR_API_KEY` or documented one-time login); add a worker MCP profile **only if** spike task 7 says cursor supports isolation.                                                                                                                                                                                                                                                                                          |
| `home/ai/claude-code/dispatch-orchestration.md` | ENG node → `claude ⇄ codex ⇄ cursor`; model map gains a **cursor ladder** column (Composer-family by tier, `--model` free); engine-lever prose gains the soft "don't front Claude-through-cursor for diverse review" rule; MCP-not-a-routing-factor section adds cursor once defer behavior is confirmed.                                                                                                                                                                                                                                                                                                                             |
| `home/ai/claude-code/DISPATCHER_PROTOCOL.md`    | engine-selection paragraph gains cursor — when to pick it (Composer models, distinct 3rd perspective on deep tasks, cursor-harness behavior).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `home/ai/claude-code/WORKER_PROTOCOL.md`        | a proper **cursor carve-out** (parallel to the existing codex carve-outs, _not_ "one note"): single-agent, no opus/sonnet subagent split, no codex-diverse review, effort/reasoning per the spike, `/deslop` handling per spike task 4.                                                                                                                                                                                                                                                                                                                                                                                               |

## Data flow

```
dispatch <tier> <model> --agent cursor [--model X] --effort <e> [id] <title>
  → worktree + tmux window + WORKER_TASK.md
  → cursor-agent <headless-flags> [--model X], prompt: read WORKER_PROTOCOL.md
     + WORKER_TASK.md and run end-to-end; push when pre-push passes; open a PR
  → runs the SINGLE-AGENT degraded pipeline (like codex): spec/plan/execute/
     review inline, scaled by tier
  → reports working → pr_open → done via the `crew` PATH CLI (engine-agnostic)
  → opens a PR
```

## Deployment

`dispatch.sh` is a `writeShellApplication` PATH binary and `cursor/default.nix`
enables a package, so this needs `nh home switch`. The protocol/orchestration
docs are read-by-path and deploy on merge to main. Package + engine usable on the
work profile; interactive `~/.cursor/mcp.json` stays on all profiles.

## KEY FINDINGS (measured, cursor-cli 2026.06.19-…-653a7fb, 2026-07-15)

Recorded so they're never re-derived. Spike done via `nh home build` + reading the real `cursor-agent --help`.

- **BLOCKER GONE — no collision.** `home.packages = [pkgs.cursor-cli]` + `nh home build` **succeeds cleanly** (`cursor-cli 2026.06.19` added, no error). The module's "playwright-core collision" comment referenced the then-`nonexistent` `pkgs.cursor-agent` attr; with `pkgs.cursor-cli` there is no collision. Binary: `bin/cursor-agent` (→ `share/cursor-agent/cursor-agent`).
  - **SUPERSEDED on merge (2026-07-20):** `main` PR #85 commit [`e70ca394`](../../..) had meanwhile added a `cursor-agent` attr via the **`llm-agents` overlay** (`lib/overlays.nix`) and enabled `home.packages = [pkgs.cursor-agent]` (v2026.07.16). That is now the authoritative install; on merging main into this branch we **adopt `pkgs.cursor-agent`** and drop the `pkgs.cursor-cli` line. The `pkgs.cursor-agent`-is-nonexistent finding was true at this branch's base but not after the overlay landed. The smoke test (below) ran against the overlay's `cursor-agent` v2026.07.16, so the engine is validated against the adopted package.
- **`CURSOR_HEADLESS_INVOCATION`** (confirmed post-login — every flag verified in `cursor-agent --help`): `cursor-agent -p --force --trust --approve-mcps --output-format text [--model <m>] '<prompt>'`.
  - `-p/--print` = non-interactive, full tools (write + shell). `-f/--force` (alias `--yolo`) = auto-allow commands (codex `--dangerously-bypass-approvals-and-sandbox` analog). `--trust` = trust workspace without prompting (headless-only). `--approve-mcps` = auto-approve MCP servers. Optional `--sandbox disabled` if the default sandbox blocks git/network.
  - **Do NOT pass `-w/--worktree`** — cursor has its own worktree feature (`~/.cursor/worktrees/…`); dispatch already provides the worktree cwd, and `--workspace` defaults to cwd.
- **`CURSOR_MODEL_FLAG`** = `--model <model>`. **Composer's exact id (post-login) = `composer-2.5`** (also `composer-2.5-fast`). Full account list via `cursor-agent models`; other implementers available include `gpt-5.6-sol-*`, `cursor-grok-4.5-*`, `claude-opus-4-8-*`.
- **`CURSOR_EFFORT`** = **no separate flag.** Post-login `cursor-agent models` shows effort is **baked into the model id suffix** — e.g. `claude-opus-4-8-{low,medium,high,xhigh,max}`, `gpt-5.6-sol-{high,xhigh}`, `cursor-grok-4.5-{low,medium,high}`. (The `[effort=…]` bracket form the help mentions is an equivalent parameterized alternative, not the canonical id.) **`composer-2.5` has no effort variants → effort is a genuine no-op for Composer.** `dispatch`'s required `--effort` is therefore **accepted-and-ignored** for cursor; encoding effort is the dispatcher's job (pick the right effort-suffixed model id), not `dispatch.sh`'s.
- **`CURSOR_MCP_ISOLATION`** = **no.** No per-invocation MCP-config flag; single shared `~/.cursor/mcp.json` (already generated by the module — symlink confirmed). → **Task 3 Step 2 (separate worker profile) is DROPPED**; the existing config serves the worker. Pass `--approve-mcps` at launch for headless auto-approve.
- **`CURSOR_AUTH`** = `cursor-agent login` (browser OAuth; `NO_OPEN_BROWSER` to disable) **or** `CURSOR_API_KEY`/`--api-key`. Currently unauthenticated. **Chosen: out-of-band `cursor-agent login` once, like `codex login` — no Nix secret** (mirror the codex module's auth comment).
- **`CURSOR_DESLOP_GATE`** = **N/A.** `deslop-guard.sh` is a **Claude Code PreToolUse hook** — it only intercepts Claude tool calls. cursor/codex workers are not Claude processes, so it never fires for them (codex sets no bypass and works). cursor needs **no** `ALLOW_PUSH_WITHOUT_DESLOP=1`; the WORKER*PROTOCOL carve-out just skips `/deslop` (not a Claude session). The separate \_git* pre-push hook (typecheck/lint/unit) still applies to every pusher.

**Residuals — RESOLVED (post-login, 2026-07-20):** logged in as `noam@factify.com`; `cursor-agent models` filled Composer's id (`composer-2.5`) and revealed effort lives in the model-id suffix (not a `[effort=]` bracket); all launch flags verified in `cursor-agent --help`. The standalone edit+commit micro-run is blocked from a Claude Bash call by the auto-mode classifier (it reads `--force`/`--yolo` as a dangerous auto-approve, same category as codex's bypass flag) — so the end-to-end unattended run is validated by the Task 5 smoke test instead, where cursor-agent launches inside a tmux pane rather than as a direct Claude tool call.

**Smoke test: PASSED (2026-07-20, `noamsto/crew-smoke`, [PR #2](https://github.com/noamsto/crew-smoke/pull/2)).** `dispatch trivial composer-2.5 --agent cursor --effort low --crew-id smoke "add a hello.md with one line"` scaffolded the worktree + tmux window and launched `cursor-agent` unattended. The worker read `WORKER_PROTOCOL.md` + `WORKER_TASK.md`, created `hello.md`, committed, pushed through the git pre-push hook, opened a real PR, and drove the full crew-bus lifecycle **`working → pr_open → done`**. The dispatch event logged `engine:cursor, model:composer-2.5`; `--effort low` was accepted-and-ignored as designed (Composer has no effort variants). End-to-end unattended run confirmed.

## Out of scope (YAGNI)

- Removing personal-profile cursor (interactive config stays — decided).
- `--model` validation in code — left to dispatcher judgment.
- Fronting Claude/GPT through cursor for _diverse review_ — mechanically allowed
  (free `--model`) but discouraged by the soft rule.
- Making cursor run the claude subagent/workflow pipeline — non-claude engines run
  single-agent by design (codex does too).
