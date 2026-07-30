# cursor-agent as a third dispatch engine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `cursor` as a third work-profile-gated `dispatch --agent` engine, alongside `claude` and `codex`.

**Architecture:** Mirror the **codex integration surface** (not the claude pipeline — non-claude engines run single-agent). Enable the existing-but-disabled `home/ai/cursor/default.nix` package, add a `cursor` branch to `dispatch.sh` that launches `cursor-agent` headless to a PR, and give it a `WORKER_PROTOCOL.md` carve-out + orchestration-doc entries. The interactive `~/.cursor/mcp.json` and the package stay on all profiles; only the dispatch _engine_ is work-gated (in `dispatch.sh`).

**Tech Stack:** Nix / Home Manager (flake-parts), bash (`writeShellApplication` PATH binaries), tmux, `cursor-cli` (nixpkgs `2026.06.19`, provides the `cursor-agent` binary), the file-based `crew` bus.

**Spec:** `docs/superpowers/specs/2026-07-15-cursor-dispatch-engine-design.md`

## Global Constraints

- **Spike-gated:** Task 1 must complete and record findings **before** Tasks 2–4 finalize their concrete strings. Tasks 2–4 reference "the value recorded in Task 1 finding N"; do not fabricate cursor-agent flags — measure them.
- **Work-profile gate lives in `dispatch.sh` only.** `home/ai/default.nix` does **not** change; `./cursor` stays unconditional (interactive editor + CLI on all profiles).
- **nixpkgs attr is `pkgs.cursor-cli`** (provides the `cursor-agent` binary). The module's commented `pkgs.cursor-agent` is a stale ref — correct it.
- **Parity = same integration surface as codex, single-agent.** No opus/sonnet subagent split, no codex-diverse reviewer, no claude slash commands for cursor.
- **Deploys via `nh home switch`** (PATH binary + package). Orchestration/protocol docs are read-by-path (deploy on merge to main), but rebuild anyway to exercise the whole change.
- **Shell hygiene:** every `dispatch.sh` edit ends with `shellcheck` + `shfmt -i 2` clean (repo pre-commit runs treefmt; `nix flake check` shfmt is already red on `dispatch.sh` per the harness memory — do not widen that, keep your hunks `shfmt -i 2`-clean).
- **Commit style:** conventional commits, scope `dispatch`, reference `#85`.

---

## Task 1: Spike — resolve the build blocker and measure cursor-agent's surface

**This is a discovery task, not TDD.** Its deliverable is (a) `cursor-cli` installing on this machine and (b) a "KEY FINDINGS" block appended to the spec. Every later task consumes these findings.

**Files:**

- Modify: `home/ai/cursor/default.nix` (correct package name, enable package, apply the collision fix)
- Modify: `docs/superpowers/specs/2026-07-15-cursor-dispatch-engine-design.md` (append `## KEY FINDINGS (measured)` block)

**Interfaces:**

- Produces (recorded in the findings block, consumed by Tasks 2–4):
  - `CURSOR_HEADLESS_INVOCATION` — the exact `cursor-agent` command template for an unattended run to a PR (analogous to codex's `codex --profile worker -m $model -c model_reasoning_effort=$effort --dangerously-bypass-approvals-and-sandbox '<prompt>'`).
  - `CURSOR_MODEL_FLAG` — the `--model` flag form + valid model IDs (`composer`, `auto`, …).
  - `CURSOR_EFFORT` — whether a reasoning-effort knob exists; if not, the string "none (—effort is a documented no-op)".
  - `CURSOR_MCP_ISOLATION` — `yes:<mechanism>` or `no:single-shared-~/.cursor/mcp.json`.
  - `CURSOR_AUTH` — `login` (out-of-band) or `CURSOR_API_KEY` (+ whether an agenix secret is needed).
  - `CURSOR_DESLOP_GATE` — how codex satisfies the pre-push `/deslop` guard today, and the equivalent for cursor (likely `ALLOW_PUSH_WITHOUT_DESLOP=1` inline).

- [ ] **Step 1: Reproduce and locate the build collision**

Do NOT trust the module's one-line comment. Uncomment the package with the corrected name and capture the real error:

```bash
cd /home/noams/nix-config-worktrees/feat-85-cursor-dispatch-engine
# temporarily set home.packages = [pkgs.cursor-cli]; in home/ai/cursor/default.nix
git add -A   # nix needs tracked files to see the change
nh home switch 2>&1 | tee /tmp/claude-status/cursor-build.log
```

Expected: a build failure. Read the log to find the _actual_ colliding paths (the recorded "playwright-core at $out/index.js" claim is suspect — `playwright-mcp` is referenced by `lib.getExe` store path in `mcp-servers.nix`, not `home.packages`). Record the real diagnosis.

- [ ] **Step 2: Apply the smallest fix that makes `nh home switch` green**

Based on the real error, trial in order of least-invasive:

1. `home.packages = [ (pkgs.cursor-cli.overrideAttrs (o: { ... })) ];` to drop the duplicate file, OR
2. `lib.hiPrio pkgs.cursor-cli` (if it's a pure home-file collision), OR
3. exclude the colliding sibling from the cursor closure.

```bash
git add -A
nh home switch 2>&1 | tail -20
command -v cursor-agent
```

Expected: switch succeeds; `cursor-agent` resolves on PATH.

- [ ] **Step 3: Measure the headless surface**

```bash
cursor-agent --help 2>&1 | tee /tmp/claude-status/cursor-help.log
cursor-agent --version
```

Determine, from `--help` and a scratch trial run in a throwaway git dir: the non-interactive/auto-apply flags, `--model` form + IDs, any reasoning-effort knob, `--output-format`, and the auth mechanism. Trial one real end-to-end micro-run (e.g. "create a file and commit") in a scratch repo to confirm it edits+commits unattended.

- [ ] **Step 4: Answer the MCP-isolation and deslop questions**

Check whether `cursor-agent` accepts a per-invocation config/MCP path (env var or flag) that isolates a worker MCP file from the interactive `~/.cursor/mcp.json`. Then inspect how the current codex worker satisfies the `/deslop` pre-push guard:

```bash
rg -n 'ALLOW_PUSH_WITHOUT_DESLOP|deslop' /home/noams/nix-config/home/ai/claude-code/dispatch.sh /home/noams/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md
```

- [ ] **Step 5: Record findings + finalize the module**

Append a `## KEY FINDINGS (measured, cursor-cli <version>)` block to the spec with all six `Produces` values filled in verbatim. If `CURSOR_MCP_ISOLATION` is `no`, note that the worker uses the existing `~/.cursor/mcp.json` and the separate worker-profile deliverable in Task 3 is **dropped**. Leave `home/ai/cursor/default.nix` with the package enabled + collision fix.

- [ ] **Step 6: Commit**

```bash
git add home/ai/cursor/default.nix docs/superpowers/specs/2026-07-15-cursor-dispatch-engine-design.md
git commit -m "feat(dispatch): enable cursor-cli package + record spike findings (#85)"
```

---

## Task 2: `dispatch.sh` — add the `cursor` engine

**Files:**

- Modify: `home/ai/claude-code/dispatch.sh` (usage string, `--agent` validation, `--mcp` guard, work-profile gate, launch branch)

**Interfaces:**

- Consumes: `CURSOR_HEADLESS_INVOCATION`, `CURSOR_MODEL_FLAG`, `CURSOR_DESLOP_GATE`, `CURSOR_EFFORT` from Task 1.
- Produces: `dispatch <tier> <model> --agent cursor [--model X] --effort <e> [id] <title>` launching a real cursor worker.

- [ ] **Step 1: Extend `--agent` validation + usage string**

In the usage `echo` (line ~10) change `[--agent claude|codex]` → `[--agent claude|codex|cursor]`. In the `--agent` case (line ~42) add `cursor` to the accepted set and update the error to `dispatch: --agent must be claude, codex, or cursor`.

- [ ] **Step 2: Generalize the `--mcp` guard to non-claude engines**

Replace the codex-only guard (line ~114):

```bash
if [ "$agent" != claude ] && [ -n "$mcp_profile" ]; then
  echo "dispatch: --mcp is claude-only; codex/cursor base MCP comes from their own profile" >&2
  exit 1
fi
```

- [ ] **Step 3: Extend the work-profile gate to cursor**

Alongside the codex gate (line ~110), refuse cursor off the work profile:

```bash
if [ "$agent" = cursor ] && [ "$profile" != work ]; then
  echo "dispatch: --agent cursor is work-profile only" >&2
  exit 1
fi
```

- [ ] **Step 4: Add the cursor launch branch**

Extend the `if [ "$agent" = codex ]` / `else` block (lines ~219-225) into an `elif [ "$agent" = cursor ]` using the **exact invocation recorded in Task 1** (`CURSOR_HEADLESS_INVOCATION`), passing `--model` when set and applying `CURSOR_DESLOP_GATE`. Template (finalize from findings — this mirrors the codex branch):

```bash
elif [ "$agent" = cursor ]; then
  tmux send-keys -t "$pane" \
    "<CURSOR_HEADLESS_INVOCATION with $model / optional --model / prompt: 'Read ~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.'>" Enter
```

- [ ] **Step 5: Verify shell hygiene**

```bash
shellcheck home/ai/claude-code/dispatch.sh
shfmt -d -i 2 home/ai/claude-code/dispatch.sh
```

Expected: shellcheck clean; `shfmt -d` shows no diff on your hunks.

- [ ] **Step 6: Rebuild so the updated `dispatch` is on PATH**

```bash
git add -A
nh home switch 2>&1 | tail -3
```

Expected: green. (`dispatch` is a `writeShellApplication` PATH binary — the guard checks below run the _rebuilt_ binary, not the source.)

- [ ] **Step 7: Verify the guards fire (dry, off the launch path)**

The parse order matters: `--effort` (line 92) and crew-id (line 98) are checked **before** the `--mcp` guard (line 114), so the `--mcp` check must supply both to reach the guard. Run on the **work profile** so the cursor work-gate (line 110) passes first.

```bash
# bad agent rejected inside the parse loop (before effort/crew gates)
dispatch standard sonnet --agent bogus --crew-id t --effort low test 2>&1 | rg 'must be claude, codex, or cursor'
# --mcp rejected for cursor (needs --crew-id + --effort to reach the guard)
dispatch standard composer --agent cursor --mcp analytics --crew-id t --effort low test 2>&1 | rg 'claude-only'
```

Expected: both matched.

- [ ] **Step 8: Commit**

```bash
git add home/ai/claude-code/dispatch.sh
git commit -m "feat(dispatch): add cursor engine branch + guards (#85)"
```

---

## Task 3: `cursor/default.nix` — auth + conditional worker MCP profile

**Files:**

- Modify: `home/ai/cursor/default.nix`

**Interfaces:**

- Consumes: `CURSOR_AUTH`, `CURSOR_MCP_ISOLATION` from Task 1.

- [ ] **Step 1: Wire auth**

If `CURSOR_AUTH` is `login` (out-of-band, like codex): add a comment mirroring the codex module's "Auth is out of band — run `cursor-agent login` once; no secret in Nix." If `CURSOR_API_KEY`: add the agenix secret + expose it in the worker's environment (follow the existing agenix pattern in `secrets/`).

- [ ] **Step 2: Add a worker MCP profile ONLY if isolation exists**

If `CURSOR_MCP_ISOLATION` is `yes:<mechanism>`: generate a worker-scoped MCP file from `mcpServers.base` (mirror codex's `home.file.".codex/worker.config.toml"`) and point the Task 2 launch at it. If `no`: **skip this step** — the existing `~/.cursor/mcp.json` already serves the worker; note it in a comment.

- [ ] **Step 3: Rebuild + verify**

```bash
git add -A
nh home switch 2>&1 | tail -5
```

Expected: green. If a worker profile was added, confirm the generated file exists.

- [ ] **Step 4: Commit**

```bash
git add home/ai/cursor/default.nix
git commit -m "feat(dispatch): cursor worker auth + MCP profile (#85)"
```

---

## Task 4: Docs — orchestration, dispatcher protocol, worker carve-out

**Files:**

- Modify: `home/ai/claude-code/dispatch-orchestration.md`
- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md`
- Modify: `home/ai/claude-code/commands/dispatcher.md`
- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md`

**Interfaces:**

- Consumes: `CURSOR_MODEL_FLAG`, `CURSOR_EFFORT`, `CURSOR_DESLOP_GATE` from Task 1.

- [ ] **Step 1: `dispatch-orchestration.md` — engine axis, model map, MCP section**

- Mermaid ENG node: `Engine? (judge per task: claude ⇄ codex ⇄ cursor — no default)` and add a `CURSOR["Cursor"]` branch (`Composer models / distinct 3rd perspective on deep tasks`) feeding the same `dispatch` terminal.
- Model map table: add a **cursor ladder** column. Deep/standard/trivial → Composer-family model IDs from `CURSOR_MODEL_FLAG`; note `--model` is free but Composer is the default distinct-implementer.
- Update the usage-string node to `[--agent claude|codex|cursor]`.
- "Three orthogonal levers" → Engine prose: add the soft rule "don't front Claude-through-cursor when the point is an independent 3rd perspective (a cursor-fronted sonnet isn't independent review of a claude worker)".
- "MCP is no longer a routing factor" → add a cursor bullet reflecting `CURSOR_MCP_ISOLATION` / defer behavior.

- [ ] **Step 2: `DISPATCHER_PROTOCOL.md` — cursor at every site (not just one paragraph)**

This file references the two-engine set in **six** places; a literal one-paragraph edit leaves it half-updated. Update all of them:

- Line ~20 co-equal weighing ("Weigh **claude** and **codex** as equal") → add cursor as a co-equal option (work profile).
- Line ~23 engine leans → add a cursor leans line (Composer models, distinct 3rd perspective on deep tasks).
- Lines ~33-35 **Profile constraint** block → extend "codex is work-profile only … `dispatch` aborts `--agent codex` off the work profile" to also cover cursor (both are work-only; personal collapses to claude).
- Line ~40 usage node → `[--agent claude|codex|cursor]`.
- Line ~46 **Engine** bullet → add cursor to "Pass `--agent claude` or `--agent codex`", the model-slot-must-match note, and the work-only note (cursor needs a one-time `cursor-agent login` or `CURSOR_API_KEY` per `CURSOR_AUTH`).
- Line ~47 **MCP** bullet → note cursor's base MCP source per `CURSOR_MCP_ISOLATION` and that `--mcp` stays claude-only for cursor too.

- [ ] **Step 3: `commands/dispatcher.md` — usage string**

Line ~45 carries the in-session `/dispatcher` usage string `dispatch --crew-id <id> <tier> <model> --effort … [--agent claude|codex] <title…>`. Update to `[--agent claude|codex|cursor]` so cursor is discoverable via the slash-command path (not only the launcher).

- [ ] **Step 4: `WORKER_PROTOCOL.md` — cursor carve-out**

Add a cursor carve-out parallel to the existing codex carve-outs (rule 1 + review-gate bullet). State: cursor runs **single-agent** (no opus/sonnet execute-subagent split); the codex-diverse reviewer is dropped (claude implementers only, already noted); effort/reasoning per `CURSOR_EFFORT`; `/deslop` handled per `CURSOR_DESLOP_GATE`.

- [ ] **Step 5: Verify prettier + mermaid**

```bash
git add -A
DOCS="home/ai/claude-code/dispatch-orchestration.md home/ai/claude-code/DISPATCHER_PROTOCOL.md home/ai/claude-code/commands/dispatcher.md home/ai/claude-code/WORKER_PROTOCOL.md"
prettier --check $DOCS 2>&1 || prettier --write $DOCS
```

Expected: clean (or written then re-staged). Eyeball the mermaid renders (no syntax break).

- [ ] **Step 6: Commit**

```bash
git add home/ai/claude-code/dispatch-orchestration.md home/ai/claude-code/DISPATCHER_PROTOCOL.md home/ai/claude-code/commands/dispatcher.md home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "docs(dispatch): document cursor engine (orchestration, protocol, worker carve-out) (#85)"
```

---

## Task 5: Live smoke test — dispatch a real cursor worker to a PR

**This is the harness's canonical verification** (same as the codex/crew "Task 6 smoke test"). No unit tests exist for a tmux dispatcher; the proof is a real worker opening a real PR and reporting on the bus.

**Files:** none (throwaway target repo, scratchpad artifacts).

**Interfaces:**

- Consumes: the full deployed change (all prior tasks + `nh home switch`).

- [ ] **Step 1: Ensure deployed**

```bash
nh home switch 2>&1 | tail -3
command -v dispatch cursor-agent
```

- [ ] **Step 2: Dispatch a trivial cursor worker against a throwaway repo**

Use an existing throwaway (e.g. `noamsto/crew-smoke`) or create one. From a dispatcher/scratch session:

```bash
dispatch trivial composer --agent cursor --effort low "add a hello.md with one line"
```

Expected: worktree + tmux window created; `cursor-agent` launches in the pane and runs unattended.

- [ ] **Step 3: Verify the bus lifecycle + PR**

```bash
crew roster            # cursor worker present, codename+color, state advancing
crew watch --timeout 300   # wakes on working→pr_open→done
```

Expected: the worker reaches `pr_open`/`done` and a real PR exists on the throwaway repo. Confirm `--effort low` was accepted (no-op or honored per findings) and `--model composer` fronted a Cursor model.

- [ ] **Step 4: Record the result in the spec**

Append a one-paragraph "Smoke test: PASSED (date, repo, PR link)" note to the spec's findings block, matching the harness's live-verification convention. Commit.

```bash
git add docs/superpowers/specs/2026-07-15-cursor-dispatch-engine-design.md
git commit -m "test(dispatch): live smoke of cursor worker to PR (#85)"
```

- [ ] **Step 5: Open the PR for this branch**

```bash
gh pr create --assignee @me --title "dispatch: add cursor-agent as a third work-profile engine (#85)" --body "Closes #85. <summary + spike findings + smoke result>"
```

---

## Self-Review

**Spec coverage:** every spec component maps to a task — spike (T1), `dispatch.sh` engine+guards+gate+launch (T2), `cursor/default.nix` package/auth/conditional-profile (T1 package, T3 auth+profile), all four docs — orchestration, DISPATCHER_PROTOCOL, `commands/dispatcher.md`, WORKER_PROTOCOL (T4), live verification (T5). The `--mcp` guard regression (spec-critic Minor 1) is T2 Step 2. The collision "reproduce-first" note (Minor 2) is T1 Step 1. Plan-critic blockers folded in: `commands/dispatcher.md` (T4 Step 3), the parse-order-correct guard check + explicit rebuild (T2 Steps 6-7), full DISPATCHER_PROTOCOL coverage (T4 Step 2), `exit 1` alignment.

**Placeholder scan:** the only deferred values are the six spike-measured strings, which are _legitimately_ unknown until T1 and are named interface outputs consumed explicitly — not hand-waves. Every other step has concrete commands.

**Type/name consistency:** the six `Produces` names from T1 are referenced verbatim in T2–T5. Engine value `cursor`, package `pkgs.cursor-cli`, and the work-gate-in-dispatch.sh decision are consistent across tasks and match the spec.
