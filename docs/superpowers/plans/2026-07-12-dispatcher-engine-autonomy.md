# Dispatcher Engine Autonomy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the dispatcher judge the worker engine (claude ⇄ codex) autonomously, log each dispatch decision + outcome to the crew bus, stop pinning the codex model to a version, and add claude→codex cross-model review at the deep tier.

**Architecture:** Docs-first. The dispatcher's routing behavior lives in instruction artifacts (`DISPATCHER_PROTOCOL.md`, `dispatch-orchestration.md`, `commands/dispatcher.md`) — changing them changes behavior. Two mechanism changes back the docs: `dispatch` (fish) appends a `kind:"dispatch"` event to the existing crew `events.jsonl`, and `crew.sh` gains a read-only `crew report` that joins those events to terminal worker status. Cross-model review is provisioned in nix (work-profile-gated plugin + read-only codex MCP server) and specified as a scoped _should_ in `WORKER_PROTOCOL.md`.

**Tech Stack:** Nix flake (flake-parts, Home Manager), fish (the `dispatch` function in `home/terminal/fish/default.nix`), POSIX sh + jq (`home/ai/claude-code/crew.sh`), markdown instruction artifacts, Claude Code plugins + MCP.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-07-12-dispatcher-engine-autonomy-design.md` — this plan implements its **v1 scope**. Deferred (do NOT implement): codex→claude review direction, `xreview` column, merge-state detection, cross-repo log rollup.
- **Settings architecture (from `~/.claude/CLAUDE.md`):** plain-JSON keys (permissions, mcpServers, plugins) → `home/ai/claude-code/settings.json`; nix-store-path or profile-gated keys (hooks, profile-locked `enabledPlugins`) → the `--settings` overlay in `home/ai/claude-code/default.nix`. Object-valued keys deep-merge across layers with the overlay winning per-key.
- **codex is work-profile only.** Cross-review, the codex MCP server, and the `pr-reviewers` plugin are gated to `osConfig.profile == "work"`. This host (g5) is `work`; g6/mbp are `personal`.
- **Rebuild:** invoke the `nix-rebuild` skill before any `nh`/`nix` command. On this Linux host the fast path is `nh home switch`.
- **Shell scripts:** run `shellcheck` on `crew.sh` after editing (per `~/.claude/CLAUDE.md`).
- **Commits:** conventional-commit prefixes (`feat`/`fix`/`chore`/`docs`). Pre-commit hooks (alejandra/deadnix/statix/prettier) reformat on first commit — re-`git add` and re-run the same commit if the first attempt fails on reformatting (expected, not an error).
- **Model IDs are authoritative as written in the model map** (`gpt-5.6`, `gpt-5.5`, `opus`/`sonnet`/`haiku`, `claude-fable-5`). Do not append date suffixes.
- **No version strings in prose** outside the model-map table in `dispatch-orchestration.md`.

## File Structure

| File                                            | Responsibility                                                                                                                              |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `home/ai/claude-code/dispatch-orchestration.md` | The model-map table (both ladders + Fable rung) and the choosing diagram — the single source of truth for tier→model and the engine branch. |
| `home/ai/claude-code/DISPATCHER_PROTOCOL.md`    | The baked dispatcher role: engine as a co-equal judged lever, model-map reference, profile constraint, Fable escalation.                    |
| `home/ai/claude-code/commands/dispatcher.md`    | In-session `/dispatcher` rubric — must match the baked protocol; also fix stale `crew`-is-a-fish-function drift.                            |
| `home/ai/claude-code/WORKER_PROTOCOL.md`        | Deep-tier cross-model review as a scoped should + same-engine fallback.                                                                     |
| `home/ai/claude-code/crew.sh`                   | Add the read-only `crew report` subcommand + usage string.                                                                                  |
| `home/terminal/fish/default.nix`                | `dispatch`: create the crew dir, append the `kind:"dispatch"` event, read `DISPATCH_SHAPE`.                                                 |
| `home/ai/claude-code/settings.json`             | codex MCP server entry + its tool-prefix allow-list entry (plain JSON).                                                                     |
| `home/ai/claude-code/default.nix`               | Work-profile-gated `pr-reviewers` marketplace + `enabledPlugins` in the `--settings` overlay.                                               |

---

## Phase 0 — Verification spikes (resolve gating open risks)

These are investigation tasks. Each ends by **recording a decision** in the plan's Findings log (append to the bottom of this file under `## Findings`). They gate later tasks; do them first.

### Task 0a: Verify Fable CLI access

**Files:** none (investigation). Records: Finding F-FABLE.

- [ ] **Step 1: Probe Fable availability**

Run: `claude --model claude-fable-5 -p "reply with the single word: ok"`
Expected (access): a short reply containing `ok`.
Expected (no access): an error naming the model / a 4xx.

- [ ] **Step 2: Record the decision**

Append to `## Findings`: `F-FABLE: <available|unavailable> — <verbatim CLI output snippet>`. If unavailable, Task 2 keeps the Fable rung in the model map but marks it "unverified — do not dispatch until access confirmed"; if available, the rung ships as written.

### Task 0b: Register the codex MCP server and capture the resolved tool prefix

**Files:** none yet (throwaway registration; the durable version lands in Task 6). Records: Finding F-MCP.

- [ ] **Step 1: Register the server at user scope (temporary)**

Run: `claude mcp add codex -s user -- codex mcp-server -c sandbox_mode="read-only" -c approval_policy="never" -c model_reasoning_effort="high"`
Expected: confirmation the server was added.

- [ ] **Step 2: Confirm it connects and read the exact tool prefix**

Run: `claude mcp list`
Expected: a line `codex ... ✓ Connected`.
Then, in a `claude` session on this repo, run `/mcp` (or inspect an available-tools listing) and note the exact tool name prefix the codex server exposes (expected shape: `mcp__codex__*`, e.g. `mcp__codex__codex`).

- [ ] **Step 3: Record and clean up**

Append to `## Findings`: `F-MCP: prefix=<exact prefix>, connected=<yes|no>`.
Run: `claude mcp remove codex -s user` (the durable, nix-managed registration lands in Task 6).

### Task 0c: Confirm the pr-reviewers marketplace + reviewer agent shape

**Files:** none (investigation). Records: Finding F-PLUGIN.

- [ ] **Step 1: Confirm the marketplace source and plugin id**

Run: `gh api repos/factify-inc/claude-pr-reviewers/contents/INSTALL.md -q .content | base64 -d | head -60`
Expected: confirms `MARKETPLACE = factify-inc/claude-pr-reviewers` and plugin id `pr-reviewers@factify-pr-reviewers`.

- [ ] **Step 2: Note the reviewer agent to reuse**

Run: `gh api repos/factify-inc/claude-pr-reviewers/contents/plugins/pr-reviewers/agents/codex-reviewer.md -q .content | base64 -d`
Expected: an agent named `codex-reviewer` with tools including `mcp__codex__codex` that reconciles codex findings against prior claude findings.

- [ ] **Step 3: Record the decision**

Append to `## Findings`: `F-PLUGIN: marketplace=factify-inc/claude-pr-reviewers, plugin=pr-reviewers@factify-pr-reviewers, reviewer-agent=codex-reviewer (reuse|adapt)`.

---

## Phase 1 — Model map + routing (docs; profile-agnostic, no runtime risk)

### Task 1: Add the model map + engine branch to `dispatch-orchestration.md`

**Files:**

- Modify: `home/ai/claude-code/dispatch-orchestration.md`

**Interfaces:**

- Produces: the canonical **model-map table** referenced by name from `DISPATCHER_PROTOCOL.md` (Task 2) and `commands/dispatcher.md` (Task 3).

- [ ] **Step 1: Read the current file to place the edits**

Run: `cat home/ai/claude-code/dispatch-orchestration.md`
Note the current CODEX/OPUS diagram nodes (the engine branch and the `gpt-5.5` effort nodes) and where a new table fits (after the diagram).

- [ ] **Step 2: Update the choosing diagram so engine is a co-equal judged branch**

In the mermaid/diagram block, ensure the engine decision reads as a first-class judgment with **no default** — replace any "default claude; codex for X" framing with "judge engine per task (claude ⇄ codex)". Replace the three `gpt-5.5 · effort=…` codex leaf labels with map references: `codex · <deep|standard|trivial> → model map`. Do not hardcode `gpt-5.5` in the diagram.

- [ ] **Step 3: Add the model-map table (drop-in, immediately after the diagram)**

```markdown
## Model map (single source of truth)

The dispatcher picks the tier-appropriate model for the chosen engine from this
table. This is the ONLY place concrete model versions appear — prose elsewhere
says "the tier-appropriate model from the model map". Bump this one table when a
new model ships.

| Tier       | claude ladder                                                                                                                                                                                                                                                            | codex ladder      |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------- |
| `deep`     | **opus** — escalate to **`claude-fable-5`** only for a genuinely hard, well-specified, long-horizon task where opus is demonstrably not enough (≈2× opus cost, refusal-classifier risk on security-adjacent code, minutes-long turns; most expensive lever, used rarely) | **`gpt-5.6`**     |
| `standard` | **sonnet**                                                                                                                                                                                                                                                               | **`gpt-5.5`**     |
| `trivial`  | **sonnet** (or **haiku** if truly trivial)                                                                                                                                                                                                                               | current light gpt |

Codex reasoning effort still scales with tier automatically (deep→high,
standard→medium, trivial→low), independent of which gpt model is chosen.
`dispatch` does not validate the model slot — the map is enforced by the
dispatcher's judgment, not by code.
```

If Finding F-FABLE was `unavailable`, append to the `deep` claude cell: ` (⚠ Fable access unverified on this account — do not dispatch until confirmed)`.

- [ ] **Step 4: Verify no stray version pins remain outside the table**

Run: `grep -nE 'gpt-5\.[0-9]|claude-fable' home/ai/claude-code/dispatch-orchestration.md`
Expected: matches ONLY inside the model-map table (and the diagram's map-reference labels carry no version). If a version appears in prose, replace it with "the tier-appropriate model from the model map".

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/dispatch-orchestration.md
git commit -m "feat(dispatcher): model map + co-equal engine branch in orchestration doc"
```

### Task 2: Rewrite the engine framing in `DISPATCHER_PROTOCOL.md`

**Files:**

- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md` (the engine paragraph after the tier/model table, ~line 19)

**Interfaces:**

- Consumes: the model map from Task 1.

- [ ] **Step 1: Read the current engine paragraph**

Run: `sed -n '7,35p' home/ai/claude-code/DISPATCHER_PROTOCOL.md`
Locate the "Then pick the engine" paragraph and the tier/model table.

- [ ] **Step 2: Replace the "pick the engine" paragraph with the co-equal framing (drop-in)**

```markdown
**Engine is a third, co-equal lever — judge it, don't default it.** Every task
resolves to `{tier, engine, model}`. Weigh **claude** and **codex** as equal
candidates by task fit, not as default-plus-exception:

- claude leans: UI/frontend, nuanced reasoning, ambiguous/underspecified work.
- codex leans: large mechanical refactors, wide-but-shallow multi-file sweeps,
  or a deliberate second-engine perspective on a hard problem.
- Neutral fit → pick either and say why; a worker can escalate via the bus.

Pick the **model** from the model map in `dispatch-orchestration.md` for the tier
and engine you chose — that table is the only place model versions live. Codex
effort scales with tier automatically (deep→high, standard→medium, trivial→low).

**Profile constraint:** codex is work-profile only — `dispatch` aborts
`--agent codex` off the work profile. On a personal-profile host the engine
lever collapses to claude-only; co-equal routing applies on the work profile.
```

- [ ] **Step 3: Adjust the tier/model table so it no longer hardcodes models**

In the tier table (~lines 11-15), change the **model** column entries to reference the map (e.g. "per model map — opus, ↑Fable to escalate" / "per model map — sonnet" / "per model map — sonnet|haiku") rather than restating them, so there is one source of truth.

- [ ] **Step 4: Verify the rubric reads coherently and the dispatch signature still matches**

Run: `grep -nE 'default claude|--agent codex .*only|gpt-5' home/ai/claude-code/DISPATCHER_PROTOCOL.md`
Expected: no "default claude" framing remains; the `--agent codex` mention is the profile-gate note only; no `gpt-5` version in prose. The `dispatch <tier> <model> [--agent claude|codex] …` signature line is unchanged.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/DISPATCHER_PROTOCOL.md
git commit -m "feat(dispatcher): engine as co-equal judged lever, model-map reference"
```

### Task 3: Sync `commands/dispatcher.md` + fix stale crew drift

**Files:**

- Modify: `home/ai/claude-code/commands/dispatcher.md`

- [ ] **Step 1: Read the file**

Run: `cat home/ai/claude-code/commands/dispatcher.md`

- [ ] **Step 2: Update the first-task instruction to include the engine lever**

In the `$ARGUMENTS` handling bullet, change "judge its tier + model per the protocol's rubric" to "judge its tier + **engine** + model per the protocol's rubric" and `dispatch <tier> <model> …` to `dispatch <tier> <model> [--agent claude|codex] …` to match the protocol.

- [ ] **Step 3: Fix the stale `crew`-is-a-fish-function line (drift)**

Replace the sentence claiming `dispatch`/`crew` are fish functions invoked via `fish -c` so that only `dispatch` is described as a fish function and `crew` is described as a PATH CLI (matching `WORKER_PROTOCOL.md:6` and the `crew.sh` header). Keep `dispatch` as `fish -c '…'`.

- [ ] **Step 4: Verify**

Run: `grep -nE 'crew.*fish function|fish -c .*crew' home/ai/claude-code/commands/dispatcher.md`
Expected: no line calls `crew` a fish function.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/commands/dispatcher.md
git commit -m "docs(dispatcher): sync /dispatcher rubric with engine lever; fix crew CLI drift"
```

---

## Phase 2 — Outcome logging (mechanism)

### Task 4: `crew report` subcommand in `crew.sh`

Do this before Task 5 so the report consumer exists before the producer is wired — the report is testable in isolation with a fixture.

**Files:**

- Modify: `home/ai/claude-code/crew.sh` (add a `report)` case before the `*)` usage case ~line 289; extend the usage string ~line 290)

**Interfaces:**

- Consumes: `kind:"dispatch"` events (produced in Task 5) and `kind:"status"` events (existing, `crew.sh:77-83`).
- Produces: `crew report [crew]` — prints a TSV table `engine model tier shape outcome duration_s`, joining `dispatch.branch` to `status.from | ltrimstr("worker:")`.

- [ ] **Step 1: Write the failing fixture test**

Create `/tmp/claude-1000/.../scratchpad/crew-report-test.sh` (use the session scratchpad dir):

```bash
#!/usr/bin/env bash
set -euo pipefail
CREW_SH="$(git rev-parse --show-toplevel)/home/ai/claude-code/crew.sh"
tmp="$(mktemp -d)"; cd "$tmp"; git init -q
mkdir -p .git/crew
cat > .git/crew/events.jsonl <<'JSONL'
{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"eng-1-foo","engine":"codex","model":"gpt-5.6","tier":"deep","shape":"mechanical"}
{"ts":2000,"crew_id":"c1","from":"worker:eng-1-foo","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":9000,"crew_id":"c1","from":"worker:eng-1-foo","to":"dispatcher:c1","kind":"status","body":{"state":"pr_open","pr_url":"http://x"}}
{"ts":9500,"crew_id":"c1","from":"worker:eng-1-foo","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
JSONL
out="$(CREW_ID=c1 sh "$CREW_SH" report c1)"
echo "$out"
echo "$out" | grep -q $'codex\tgpt-5.6\tdeep\tmechanical\tdone\t7' \
  && echo "PASS" || { echo "FAIL"; exit 1; }
```

- [ ] **Step 2: Run it to verify it fails**

Run: `bash <scratchpad>/crew-report-test.sh`
Expected: FAIL — `crew report` is an unknown subcommand (usage error to stderr).

- [ ] **Step 3: Add the `report)` case to `crew.sh`**

Insert immediately before the `*)` case (~line 289):

```sh
report)
  crew="${1:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  printf 'engine\tmodel\ttier\tshape\toutcome\tduration_s\n'
  jq -s -r --arg crew "$crew" '
    map(select(.crew_id == $crew)) as $all
    | ($all | map(select(.kind == "dispatch")))[]
    | .branch as $b
    | ($all | map(select(.kind == "status" and ((.from // "") | ltrimstr("worker:")) == $b))) as $st
    | ($st | map(select(.body.state == "working")) | sort_by(.ts) | (.[0].ts // null)) as $start
    | ($st | sort_by(.ts) | (.[-1] // null)) as $last
    | [ .engine, .model, .tier, (.shape // "—"),
        ($last.body.state // "—"),
        (if ($start != null and $last != null) then (($last.ts - $start) / 1000 | floor | tostring) else "—" end)
      ] | @tsv' "$log"
  ;;
```

- [ ] **Step 4: Extend the usage string**

In the `*)` case usage line (~line 290), append ` | report [crew]` to the list of subcommands.

- [ ] **Step 5: Run the test to verify it passes**

Run: `bash <scratchpad>/crew-report-test.sh`
Expected: prints the row and `PASS`.

- [ ] **Step 6: Shellcheck**

Run: `shellcheck home/ai/claude-code/crew.sh`
Expected: no new warnings/errors (fix any the change introduced).

- [ ] **Step 7: Commit**

```bash
git add home/ai/claude-code/crew.sh
git commit -m "feat(crew): add read-only 'crew report' joining dispatch decisions to outcomes"
```

### Task 5: `dispatch` appends the `kind:"dispatch"` event

**Files:**

- Modify: `home/terminal/fish/default.nix` (the `dispatch` function; add the append after `$crew_dir`/`$CREW_ID` are set at ~line 322-323 and `$branch`/`$tier`/`$model`/`$agent` are known, i.e. after the branch/closes block ~line 311)

**Interfaces:**

- Consumes: `$agent` (engine), `$model`, `$tier`, `$branch`, `$crew_dir`, `$CREW_ID` — all in scope by ~line 322-337.
- Produces: one `kind:"dispatch"` JSONL line per dispatch, consumed by `crew report` (Task 4).

- [ ] **Step 1: Validate the exact jq shape produces valid JSON (standalone, before editing)**

Run (bash is fine for the jq check — jq is jq):

```bash
jq -nc --arg crew c1 --arg branch eng-1-foo --arg engine codex \
  --arg model gpt-5.6 --arg tier deep --arg shape mechanical \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, engine:$engine, model:$model, tier:$tier, shape:$shape}' | jq -e .
```

Expected: a single-line object echoed back by the validating `jq -e .` (exit 0).

- [ ] **Step 2: Read the region and add the `DISPATCH_SHAPE` read + append**

Run: `sed -n '316,342p' home/terminal/fish/default.nix` to confirm line numbers.
After the `$sanitized`/`$wt_path` block and the `set -q CREW_ID; or set -gx CREW_ID (crew id)` line (~line 322), add:

```fish
          # Log the dispatch decision to the crew bus for later `crew report`.
          # dispatch may run before the worker's first `crew status`, so create
          # the dir here (crew status does its own mkdir; dispatch does not).
          set -l dispatch_shape ""
          set -q DISPATCH_SHAPE; and set dispatch_shape "$DISPATCH_SHAPE"
          mkdir -p $crew_dir
          jq -nc --arg crew "$CREW_ID" --arg branch "$branch" \
              --arg engine "$agent" --arg model "$model" --arg tier "$tier" \
              --arg shape "$dispatch_shape" \
              '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, engine:$engine, model:$model, tier:$tier, shape:$shape}' \
              >> $crew_dir/events.jsonl
```

(Place it after `$crew_dir` is set at ~line 323 and after `$branch` exists (~line 311); confirm both are in scope at the insertion point.)

- [ ] **Step 3: Rebuild so the fish function updates**

Invoke the `nix-rebuild` skill, then run: `nh home switch`
Expected: build succeeds; the `dispatch` function is updated in the running shell config.

- [ ] **Step 4: Verify the append end-to-end in a throwaway repo**

In a scratch git repo, set `CREW_ID`, source/emulate the append with sample vars, and confirm a valid `kind:"dispatch"` line lands in `.git/crew/events.jsonl`, then that `crew report` renders it:

```bash
cd "$(mktemp -d)"; git init -q; mkdir -p .git/crew
set -gx CREW_ID c1   # fish
jq -nc --arg crew "$CREW_ID" --arg branch b1 --arg engine claude --arg model opus --arg tier deep --arg shape ui '{ts:(now*1000|floor),crew_id:$crew,kind:"dispatch",branch:$branch,engine:$engine,model:$model,tier:$tier,shape:$shape}' >> .git/crew/events.jsonl
crew report c1   # expect a row: claude opus deep ui — —
```

Expected: `crew report` prints the dispatched row (outcome/duration `—` since no worker status yet).

- [ ] **Step 5: Commit**

```bash
git add home/terminal/fish/default.nix
git commit -m "feat(dispatch): log dispatch decision (engine/model/tier/shape) to the crew bus"
```

---

## Phase 3 — Cross-model review (claude→codex, work profile)

Gated on Findings F-MCP (tool prefix) and F-PLUGIN (marketplace/agent). If F-FABLE is unavailable, Phase 3 is unaffected (Fable is a claude model choice, not the reviewer).

### Task 6: Provision the codex MCP server + pr-reviewers plugin (nix, work-gated)

**Files:**

- Modify: `home/ai/claude-code/settings.json` — add the `codex` mcpServers entry and its tool-prefix allow entry (plain JSON).
- Modify: `home/ai/claude-code/default.nix` — add the `pr-reviewers` marketplace + profile-gated `enabledPlugins` in the `--settings` overlay.

**Interfaces:**

- Consumes: F-MCP tool prefix, F-PLUGIN marketplace/plugin ids.
- Produces: a connected read-only `codex` MCP server whose tools are allow-listed under `--permission-mode auto`, and the `pr-reviewers` plugin available on the work profile.

- [ ] **Step 1: Add the codex MCP server to `settings.json`**

In `mcpServers`, add (matching the existing entries' shape):

```json
"codex": {
  "command": "codex",
  "args": ["mcp-server", "-c", "sandbox_mode=\"read-only\"", "-c", "approval_policy=\"never\"", "-c", "model_reasoning_effort=\"high\""]
}
```

- [ ] **Step 2: Allow-list the codex tool prefix in `settings.json`**

In `permissions.allow`, add the exact prefix from Finding F-MCP (expected `mcp__codex__*`) alongside the other `mcp__…__*` entries, so an auto-mode worker's reviewer subagent isn't denied.

- [ ] **Step 3: Add the marketplace + profile-gated plugin in the overlay**

In `home/ai/claude-code/default.nix`, in the `--settings` overlay (the `nix-settings-json` block), under the work-profile `lib.optionalAttrs (osConfig.profile == "work")` section: add the `pr-reviewers` marketplace to `extraKnownMarketplaces` (source `factify-inc/claude-pr-reviewers`) and add `pr-reviewers@factify-pr-reviewers` to the profile-locked `enabledPlugins`. Follow the existing figma/pg/cc-skills-golang gating pattern.

- [ ] **Step 4: Rebuild**

Invoke the `nix-rebuild` skill, then run: `nh home switch`
Expected: build succeeds.

- [ ] **Step 5: Verify the server connects and the plugin is present**

Run: `claude mcp list`
Expected: `codex ... ✓ Connected`.
Then in a `claude` session: `/help` lists the `pr-reviewers` commands (`/codex-review` etc.), and `/mcp` shows the codex tools available (not denied).

- [ ] **Step 6: Commit**

```bash
git add home/ai/claude-code/settings.json home/ai/claude-code/default.nix
git commit -m "feat(claude-code): work-gated pr-reviewers plugin + read-only codex MCP for cross-review"
```

### Task 7: Deep-tier cross-model review in `WORKER_PROTOCOL.md`

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (the "Code review gate" section, ~lines 15-21)

**Interfaces:**

- Consumes: the allow-listed codex MCP tools + `codex-reviewer` agent from Task 6 / F-PLUGIN.

- [ ] **Step 1: Read the current gate section**

Run: `sed -n '15,21p' home/ai/claude-code/WORKER_PROTOCOL.md`

- [ ] **Step 2: Add the scoped cross-model requirement (drop-in, after the existing "Scale by tier" bullet)**

```markdown
- **Deep tier — cross-model review (work profile, claude implementers):** in
  addition to the language-matched reviewer, dispatch a **codex-diverse**
  reviewer subagent (reuse/adapt the `pr-reviewers` `codex-reviewer` agent) that
  carries the `mcp__codex__*` tools and drives the read-only `codex` MCP server,
  then reconcile its findings against the same-model pass (both / codex-only /
  claude-only). This is a **should, not a blocker**: if the codex MCP server is
  unavailable, you are a codex implementer (codex→claude is not yet supported),
  or you are off the work profile, fall back to same-engine adversarial review
  and proceed — never stall the gate on a missing diverse engine.
```

- [ ] **Step 3: Verify the requirement can't be read as an unconditional MUST**

Run: `grep -nE 'MUST|different from the implementer' home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: the cross-model text reads as a should with an explicit fallback; no unconditional MUST that a personal-profile or codex worker cannot satisfy.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): scoped deep-tier claude→codex cross-model review with same-engine fallback"
```

---

## Phase 4 — Integration verification

### Task 8: End-to-end smoke of the routing + logging path

**Files:** none (verification).

- [ ] **Step 1: Confirm the docs cohere**

Run: `grep -nE 'model map' home/ai/claude-code/DISPATCHER_PROTOCOL.md home/ai/claude-code/commands/dispatcher.md`
Expected: both reference the model map rather than restating versions.

- [ ] **Step 2: Dispatch one real trivial task and confirm the event lands**

From a dispatcher shell in a scratch/throwaway GitHub repo (or a safe branch), run a single `DISPATCH_SHAPE=mechanical dispatch trivial sonnet <ISSUE-or-title>` and confirm:

```bash
crew report
```

Expected: a row for the new branch with `engine=claude model=sonnet tier=trivial shape=mechanical`, `outcome` progressing to `pr_open`/`done` as the worker runs, and a numeric `duration_s` once terminal.

- [ ] **Step 3: (work profile) Dispatch one deep claude task and confirm cross-review runs**

Dispatch a small `deep` claude task; confirm in the worker's window that the gate dispatches both the language reviewer and the codex-diverse reviewer, and that codex findings are reconciled (not denied). If denied, revisit the F-MCP allow-list prefix in Task 6 Step 2.

- [ ] **Step 4: Update the memory pointer**

Add/update the dispatcher-harness project memory with: engine-autonomy + logging + claude→codex cross-review shipped on branch `feat/dispatcher-engine-autonomy` (issue #72); codex→claude, `xreview`, merge-state deferred.

---

## Self-review (against the spec)

- **Spec §1 (routing):** Tasks 1–3. ✓
- **Spec §2 (model map, no version pin):** Task 1 (table + grep guard) + Task 2 Step 3. ✓
- **Spec §3 (logging: dispatch event + crew report):** Task 5 (producer) + Task 4 (consumer, with the `worker:` prefix-strip join and the `mkdir`/`DISPATCH_SHAPE` fixes). ✓
- **Spec §4 (deep-tier claude→codex cross-review):** Tasks 6–7; the allow-list blocker is Task 6 Step 2 / open-risk #1; subagent (not slash) mechanism in Task 7. ✓
- **Spec §5 (Fable escalation):** Task 0a (access) + Task 1 Step 3 (rung, with unverified marker if F-FABLE fails). ✓
- **Open risks:** #1 allow-list → Task 6 Step 2; #2 codex→claude → deferred (not tasked, per v1 scope); #3 reviewer subagent → Task 7 + F-PLUGIN; #4 Fable → Task 0a; #5 shape vocab → the `shape` values used across Tasks 4/5/8 come from the closed set `mechanical|ui|ambiguous|security|wide` (documented in the model-map/orchestration doc as the allowed tags — add that line in Task 1 if not already present); #6 crew scope/merge → `crew report` defaults to a single `crew_id` per the existing `crew log` convention (Task 4), merge-state deferred; #7 xreview → deferred. ✓
- **Deferred items NOT tasked:** codex→claude, xreview, merge-state, cross-repo rollup. ✓
- **Type/name consistency:** the dispatch event fields (`engine/model/tier/branch/shape/ts/crew_id/kind`) are identical in Task 5 (producer) and Task 4 (consumer test fixture + jq). `crew report` column order (`engine model tier shape outcome duration_s`) matches between Task 4 Step 3 and the Task 4 test assertion. ✓

## Findings

_(Phase 0 tasks append here.)_

- F-FABLE: **available** — `claude --model claude-fable-5 -p "reply with the single word: ok"` returned `ok` (exit 0). Fable rung ships as written; no unverified marker in Task 1.
- F-MCP: prefix=`mcp__codex__*` (server named `codex`; the `codex-reviewer` agent's toolset uses `mcp__codex__codex`), connected=yes (`claude mcp list` → `codex … ✔ Connected` with the read-only args). Temp user-scope registration removed; durable entry lands in `settings.json` at Task 6.
- F-PLUGIN: marketplace=`factify-inc/claude-pr-reviewers`, plugin=`pr-reviewers@factify-pr-reviewers`, reviewer-agent=`codex-reviewer` (reuse — tools include `mcp__codex__codex`, reconciles codex findings vs prior claude findings, read-only).
