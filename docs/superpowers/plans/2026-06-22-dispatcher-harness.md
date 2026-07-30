# Dispatcher Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the main Claude session into a pure dispatcher that scaffolds isolated worker sessions (worktree + tmux window + baked persona), where each worker runs a tier-scaled `spec→crit→plan→crit→execute` pipeline and pushes through the pre-push gate to a PR — signalling back so the dispatcher never polls.

**Architecture:** Three nested levels — dispatcher (full `claude` on `main`, never edits) → worker (full `claude` in its own worktree, owns the PR) → execute subagents + an inner `Workflow` script for the critic loop. General capability is vendored in `nix-config` (`home/ai/claude-code/`) so it travels to cloud agents; per-project risky-tool _denials_ stay in each repo's `.claude/settings.json`.

**Tech Stack:** Claude Code CLI (`--append-system-prompt-file`, `--permission-mode auto`, `--agents`), the `Workflow` orchestration tool, `worktrunk` (`wt`) + tmux + `tmux send-keys`, fish, Nix / Home Manager, `gh`.

---

## Critical constraint: the worktree symlink dangle (read before executing)

`home/ai/claude-code/{agents,commands,skills}` are wired into `~/.claude/` with **`mkOutOfStoreSymlink` pointing at `~/nix-config/...`** — the _main_ checkout, not whatever worktree you're editing in (`default.nix:129-170`). Consequences that shape every verification step below:

- A new agent / workflow / fish function added on `feat/21-dispatcher-harness` **does not resolve from `~/.claude/` until this branch is merged into `~/nix-config`** and `nh home switch` runs against the main checkout. (Memory: `reference_skill_symlink_worktree_gotcha`.)
- Therefore tasks are verified in two stages: **(a) static/local checks** that run against the worktree file directly (syntax, schema, `nix flake check`, invoking a workflow by absolute `scriptPath`), and **(b) a post-merge live smoke test** (Task 10) once the symlinks resolve.
- Do **not** claim a persona/agent/hook "works end-to-end" from inside the worktree. It can't, by construction.

---

## File Structure

All new vendored files live under `home/ai/claude-code/` (auto-symlinked) except the dispatcher fish function and the plan doc.

| Path                                                                                                                                                            | Responsibility                                                                                                                                  | Travels via                                                |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `home/ai/claude-code/agents/spec-critic.md`                                                                                                                     | Adversarial spec reviewer (opus, structured verdict)                                                                                            | whole `agents/` dir already symlinked → `~/.claude/agents` |
| `home/ai/claude-code/agents/plan-critic.md`                                                                                                                     | Adversarial plan reviewer (opus, structured verdict)                                                                                            | same                                                       |
| `home/ai/claude-code/workflows/spec-plan-critic.js`                                                                                                             | Inner `Workflow`: spec→crit→plan→crit with 2-rev caps + structured verdicts                                                                     | **new** `workflows/` symlink (Task 8)                      |
| `home/ai/claude-code/WORKER_PROTOCOL.md`                                                                                                                        | Worker orchestrator persona: tier dispatch, delegate-don't-free-code, push-through-gate                                                         | referenced by absolute path in dispatch fn                 |
| `home/ai/claude-code/dispatch-notify.sh`                                                                                                                        | Stop/Notification hook: signal worker done/blocked → dispatcher pane                                                                            | nix overlay in `default.nix`                               |
| `home/ai/claude-code/settings.json` (edit)                                                                                                                      | Safe-tool base allowlist (`permissions.allow`)                                                                                                  | already symlinked                                          |
| `home/ai/claude-code/default.nix` (edit)                                                                                                                        | Wire `workflows/` symlink + register `dispatch-notify` hook                                                                                     | —                                                          |
| `dispatch` fish function (via this repo's actual mechanism — `interactiveShellInit` or `xdg.configFile."fish/functions/…"`, **not** a `functions/` source tree) | Dispatcher one-liner: classify tier → issue → worktree → `WORKER_TASK.md` (records dispatcher pane) → `tmux new-window` + baked `claude` launch | nix-managed fish config                                    |
| `docs/superpowers/plans/2026-06-22-dispatcher-harness.md`                                                                                                       | this plan                                                                                                                                       | committed to branch                                        |

**Shared artifact — the verdict schema** (used by both critics and the Workflow; define once, reference everywhere):

```json
{
  "type": "object",
  "required": ["verdict", "blocking", "summary"],
  "properties": {
    "verdict": { "enum": ["accept", "revise", "reject"] },
    "blocking": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["issue", "why", "fix"],
        "properties": {
          "issue": { "type": "string" },
          "why": { "type": "string" },
          "fix": { "type": "string" }
        }
      }
    },
    "nonblocking": { "type": "array", "items": { "type": "string" } },
    "summary": { "type": "string" }
  }
}
```

`accept` → proceed. `revise` → author addresses `blocking[]`, re-submits (max 2 rounds). `reject` → escalate to the PR body, do not loop.

---

## Phase 1 — Correctness core (critics + worker protocol + inner workflow)

### Task 1: `spec-critic` agent

**Files:**

- Create: `home/ai/claude-code/agents/spec-critic.md`

- [ ] **Step 1: Write the agent file**

Match the existing frontmatter shape (`agents/security-reviewer.md:1-6`). The body is an adversarial brief, not a checklist of pleasantries.

```markdown
---
name: spec-critic
description: Adversarial reviewer for feature specs. Use to stress-test a spec before any planning — hunts missing requirements, scope creep, and solving-the-wrong-problem. Returns a structured verdict.
tools: ["Read", "Grep", "Glob"]
model: opus
---

# Spec Critic

You are an adversarial spec reviewer. Your job is to find what is wrong, missing, or out of scope — not to praise. A spec you "mostly like" with a real gap is a `revise`, not an `accept`. Rubber-stamping is failure.

## What you are given

The spec text, plus read access to the repo it targets. Read the surrounding code/conventions before judging — a "missing requirement" that the codebase already enforces is not a finding.

## Attack the spec on these axes

1. **Wrong problem** — does this solve what was actually asked, or an adjacent thing that was easier to spec?
2. **Missing requirements** — unstated inputs, error paths, auth/permission boundaries, concurrency, idempotency, rollback.
3. **Scope creep** — anything in here that isn't needed to satisfy the goal. Name it; recommend cutting it.
4. **Untestable claims** — requirements with no observable acceptance criterion.
5. **Hidden assumptions** — environment, credentials, ordering, or data shape assumed but never stated.

## Discipline

- Every blocking finding needs `issue` / `why` / `fix`. No vague "consider improving X".
- If you cannot find a concrete defect after a genuine read, return `accept` — do not invent filler to look thorough.
- You do NOT write the spec or the fix. You judge.

## Output

Return ONLY the structured verdict object (the StructuredOutput tool enforces the schema).
```

- [ ] **Step 2: Verify frontmatter parses**

Run: `head -6 home/ai/claude-code/agents/spec-critic.md`
Expected: valid YAML between `---` fences, `model: opus`, `name: spec-critic`.

- [ ] **Step 3: Commit**

```bash
git add home/ai/claude-code/agents/spec-critic.md
git commit -m "feat(claude): add adversarial spec-critic agent"
```

### Task 2: `plan-critic` agent

**Files:**

- Create: `home/ai/claude-code/agents/plan-critic.md`

- [ ] **Step 1: Write the agent file**

Same shape as Task 1; different attack axes.

```markdown
---
name: plan-critic
description: Adversarial reviewer for implementation plans. Use to stress-test a plan before execution — hunts wrong sequencing, missed files, untested edges, and placeholder steps. Returns a structured verdict.
tools: ["Read", "Grep", "Glob"]
model: opus
---

# Plan Critic

You are an adversarial implementation-plan reviewer. Find what will break or stall an engineer executing this plan with zero prior context. Approving a plan that strands the engineer is failure.

## Attack the plan on these axes

1. **Sequencing** — does any task depend on something a later task creates? Order errors are blocking.
2. **Missed files / call sites** — a change that the plan applies in one place but the repo needs in three. Grep to confirm.
3. **Untested edges** — steps that ship behavior with no verification command, or verification that can't actually fail.
4. **Placeholders** — "add error handling", "similar to Task N", "TBD". Each is blocking per the writing-plans rules.
5. **Type/name drift** — a symbol defined as one name in an early task and referenced as another later.

## Discipline

- Verify against the actual repo before asserting a gap (read the files the plan names).
- Every blocking finding: `issue` / `why` / `fix`. No filler.
- Clean plan after a genuine read → `accept`.

## Output

Return ONLY the structured verdict object.
```

- [ ] **Step 2: Verify** — `head -6 home/ai/claude-code/agents/plan-critic.md`; valid YAML, `model: opus`.

- [ ] **Step 3: Commit**

```bash
git add home/ai/claude-code/agents/plan-critic.md
git commit -m "feat(claude): add adversarial plan-critic agent"
```

### Task 3: inner-loop `Workflow` script

**Files:**

- Create: `home/ai/claude-code/workflows/spec-plan-critic.js`

This is the standard/deep critic loop as a deterministic Workflow: the revision cap is a real `while` bound, not prose the worker hopes to honor. The worker invokes it (by `scriptPath` until the `workflows/` symlink lands — see Task 8), passing `{ tier, task, repoPath }` as `args`, and ingests the returned `{ spec, plan, escalations }`.

- [ ] **Step 1: Write the workflow script**

```javascript
export const meta = {
  name: "spec-plan-critic",
  description:
    "Spec then plan, each gated by an adversarial critic with a 2-revision cap",
  phases: [
    { title: "Spec" },
    { title: "Spec critique" },
    { title: "Plan" },
    { title: "Plan critique" },
  ],
};

const VERDICT = {
  type: "object",
  required: ["verdict", "blocking", "summary"],
  properties: {
    verdict: { enum: ["accept", "revise", "reject"] },
    blocking: {
      type: "array",
      items: {
        type: "object",
        required: ["issue", "why", "fix"],
        properties: {
          issue: { type: "string" },
          why: { type: "string" },
          fix: { type: "string" },
        },
      },
    },
    nonblocking: { type: "array", items: { type: "string" } },
    summary: { type: "string" },
  },
};

const MAX_REVS = 2;
const { tier, task, repoPath } = args;

// Returns { text, escalation } — escalation set when the critic never accepted.
async function gated(makePrompt, critic, phase, criticPhase, label) {
  let draft = await agent(makePrompt(null), { phase, label: `${label}:draft` });
  for (let rev = 1; rev <= MAX_REVS; rev++) {
    const v = await agent(
      `Adversarially review this ${label}. Repo: ${repoPath}.\n\n${draft}`,
      {
        phase: criticPhase,
        label: `${critic}:rev${rev}`,
        schema: VERDICT,
        agentType: critic,
      },
    );
    if (!v || v.verdict === "accept") return { text: draft, escalation: null };
    if (v.verdict === "reject")
      return {
        text: draft,
        escalation: { stage: label, reason: "critic rejected", detail: v },
      };
    log(`${label} rev ${rev}: ${v.blocking.length} blocking`);
    draft = await agent(
      `Revise this ${label} to resolve the blocking findings.\n\nFINDINGS:\n${JSON.stringify(v.blocking, null, 2)}\n\nCURRENT:\n${draft}`,
      { phase, label: `${label}:rev${rev}` },
    );
  }
  return {
    text: draft,
    escalation: { stage: label, reason: `unresolved after ${MAX_REVS} revs` },
  };
}

const escalations = [];

// deep tier specs; standard/trivial skip straight to (or past) planning.
let spec = null;
if (tier === "deep") {
  const r = await gated(
    () =>
      `Write a spec for this task using brainstorming discipline.\n\nTASK:\n${task}`,
    "spec-critic",
    "Spec",
    "Spec critique",
    "spec",
  );
  spec = r.text;
  if (r.escalation) escalations.push(r.escalation);
}

// standard + deep get a plan; trivial returns immediately (handled by the worker, not here).
const planInput = spec ? `SPEC:\n${spec}` : `TASK:\n${task}`;
const pr = await gated(
  () =>
    `Write a bite-sized implementation plan (writing-plans discipline).\n\n${planInput}`,
  "plan-critic",
  "Plan",
  "Plan critique",
  "plan",
);
if (pr.escalation) escalations.push(pr.escalation);

return { spec, plan: pr.text, escalations };
```

- [ ] **Step 2: Syntax-check the script**

The script is plain JS (no TS). Verify it parses:
Run: `node --check home/ai/claude-code/workflows/spec-plan-critic.js`
Expected: exit 0, no output.

- [ ] **Step 3: Confirm forbidden built-ins are absent**

Workflow scripts must not call `Date.now()` / `Math.random()` / argless `new Date()`.
Run: `rg -n 'Date\.now|Math\.random|new Date\(\)' home/ai/claude-code/workflows/spec-plan-critic.js`
Expected: no matches.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/workflows/spec-plan-critic.js
git commit -m "feat(claude): add spec-plan-critic inner workflow"
```

> **Note for the executing engineer:** a true behavioral run of this workflow requires the `spec-critic`/`plan-critic` agents to resolve, which only happens post-merge (symlink dangle). The post-merge smoke test (Task 10) exercises it on a real throwaway task. Do not mark this task "verified end-to-end" before then.

### Task 4: `WORKER_PROTOCOL.md`

**Files:**

- Create: `home/ai/claude-code/WORKER_PROTOCOL.md`

This is appended to the worker's system prompt via `--append-system-prompt-file`. It encodes: read your task file, run only your tier's stages, delegate execution to subagents, never free-code, push through the gate, open the PR, flag escalations.

- [ ] **Step 1: Write the protocol**

```markdown
# Worker Protocol

You are a **worker** session launched by a dispatcher. You own exactly one task, defined in `WORKER_TASK.md` in your worktree. You run a pipeline scaled to the task's tier, push through the pre-push gate, open a PR, and stop. You do not pick up other work.

## First action

Read `WORKER_TASK.md`. It stamps a `tier:` of `trivial`, `standard`, or `deep`.

## Pipeline by tier

- **trivial** — implement directly, run the gate, open the PR. No spec, no plan, no critics.
- **standard** — run the `spec-plan-critic` workflow with `{ tier: 'standard', ... }` (plan + plan-critic only), then execute the returned plan via subagents, then gate + PR.
- **deep** — run `spec-plan-critic` with `{ tier: 'deep', ... }` (spec + spec-critic, then plan + plan-critic), then execute, then gate + PR.

## Rules

1. **Delegate execution.** For standard/deep, implementation steps run as subagents (subagent-driven-development), capped at 3 concurrent. You orchestrate; you do not hand-write the implementation yourself.
2. **Critics are independent.** Never self-review — the workflow dispatches `spec-critic`/`plan-critic` in fresh contexts. Ingest their verdicts with receiving-code-review discipline: verify the finding, don't perform agreement.
3. **Revision cap is 2.** The workflow enforces it. If it returns `escalations[]`, surface them verbatim in the PR body under "## Escalated" — do not silently proceed as if clean.
4. **Push through the gate.** `git push` triggers the pre-push hook (typecheck/lint/unit/build-num). On failure, fix and re-push; do not bypass.
5. **PR body must include `Closes #<N>`** (N is in your task file) and any escalations.
6. **Never** run `wrangler deploy`, `wrangler ... --remote`, or `wrangler secret`. These are denied; if a step seems to need them, stop and flag it.

## When done

Open the PR. Then read `dispatcher_pane:` from `WORKER_TASK.md` and signal completion to it exactly once:
`tmux display-message -t "$dispatcher_pane" -d 4000 "worker done: <branch> — PR <url>"`. Then stop. (A `SessionEnd` hook is only a backstop for the case where you die before signalling.)
```

- [ ] **Step 2: Verify it's referenced, not symlinked.** This file is passed by absolute path, so no `~/.claude/` wiring is needed. Confirm the dispatch function (Task 6) points at `~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md`.

- [ ] **Step 3: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(claude): add worker orchestrator protocol"
```

---

## Phase 2 — Orchestration shell (allowlist + dispatcher + notification)

### Task 5: ~~per-project risky-tool deny~~ — dropped

> **Dropped (post-merge):** the original plan had each prod-credentialed repo add a `permissions.deny` for `wrangler deploy`/`secret`/`--remote`. We're not doing it. The base allow (`settings.json:13-15`: `Bash(git *)`, `Bash(gh *)`, `Bash(just *)`) was already fine, and a hard deny — especially the over-broad `Bash(* --remote*)` substring — is more nagging friction than protection for a supervised harness. The prod-write fence now lives only as soft guidance in **WORKER_PROTOCOL.md rule 6** (don't run them; stop and flag), backed by `auto` mode still escalating on genuinely novel commands. If a hard guard is ever wanted, add the deny to the specific repo's `.claude/settings.json` then.

### Task 6: dispatcher fish function

> **Rev 1 (skeptic findings #1, #2):** the original body was broken two ways. (a) The dispatcher is itself a Claude session (`CLAUDECODE=1`), and worktrunk's `post-switch` hook bails on `[ -n "$CLAUDECODE" ] && exit 0` (`~/.config/worktrunk/config.toml:18`) — so `wt switch` creates the worktree but **no tmux window**, and `send-keys -t …:$branch` targets nothing. The dispatcher must create the window itself. (b) `git worktree list --porcelain` prints the path with the **sanitized** branch (`feat-21-…`), so matching on the raw `$branch` (`feat/21-…`) never matched — `$wt_path` came back empty.

**Files:**

- Create: a fish function `dispatch` _(via this repo's actual mechanism — see Step 1; the per-file `functions/_.fish` path the table assumed does not exist here)\*

A **fish function, not a `just` recipe**, because you dispatch from inside the _target_ repo (e.g. toddl), not from nix-config. Signature: `dispatch <tier> <model> <title...>`.

- [ ] **Step 1: Find how fish functions are defined in this repo**

Run: `rg -n 'interactiveShellInit|configFile."fish|programs.fish' home/terminal/fish/default.nix`
Expected: this repo defines functions either inline in `programs.fish.interactiveShellInit` or as `xdg.configFile."fish/functions/NAME.fish".text = ''…''` — **not** a `home/terminal/fish/functions/` source tree. Put the body below into whichever exists.

- [ ] **Step 2: Write the function body**

```fish
function dispatch --description 'Scaffold a worker: issue -> worktree -> task file -> baked claude'
    set -l tier $argv[1]
    set -l model $argv[2]
    set -l title (string join ' ' $argv[3..-1])
    if not contains -- $tier trivial standard deep
        echo "usage: dispatch <trivial|standard|deep> <model> <title...>" >&2
        return 1
    end

    # 1. issue (capture number)
    set -l url (gh issue create --assignee @me --title "$title" --body "Dispatched worker task.")
    set -l num (string match -r '/(\d+)$' -- $url)[2]

    # 2. derive branch + its sanitized worktree path (slash -> dash, matches worktrunk's
    #    {{ repo_path }}-worktrees/{{ branch | sanitize }} template).
    set -l slug (string lower (string replace -ra '[^a-zA-Z0-9]+' '-' $title) | string sub -l 40 | string trim -c -)
    set -l branch "feat/$num-$slug"
    set -l sanitized (string replace -a / - $branch)
    set -l wt_path (git rev-parse --show-toplevel)-worktrees/$sanitized

    # 3. create the worktree. The post-switch tmux hook is DISABLED under CLAUDECODE,
    #    so this only makes the worktree — we drive tmux ourselves below.
    wt switch -c $branch -y

    # 4. task file (record the dispatcher's own pane so the worker can signal back — finding #4)
    printf 'tier: %s\ntitle: %s\nCloses #%s\ndispatcher_pane: %s\n' \
        $tier $title $num $TMUX_PANE > $wt_path/WORKER_TASK.md

    # 5. create the worker window ourselves and launch claude in it (capture the pane id we made)
    set -l pane (tmux new-window -d -c $wt_path -n $sanitized -P -F '#{pane_id}')
    tmux send-keys -t $pane \
        "claude --model $model --append-system-prompt-file ~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.'" Enter
end
```

> **Still-open joints:** worktrunk's `sanitize` filter may differ from the fish sanitizer above on trailing-dash / length edges; if `$wt_path` ever mismatches, derive it from `wt list` output instead of reconstructing. The dispatcher's `≤3 concurrent workers` cap (self-review gap #1) belongs here — count `*-worktrees/*/WORKER_TASK.md` and refuse past 3 before step 1.

- [ ] **Step 3: Lint the fish syntax**

Run: `fish --no-execute` against the function file the repo's mechanism produces (or paste into an interactive `fish` to source-check).
Expected: no parse error.

- [ ] **Step 4: Commit**

```bash
git add home/terminal/fish/
git commit -m "feat(fish): add dispatch function for worker scaffolding"
```

### Task 7: notify the dispatcher when a worker finishes

> **Rev 1 (skeptic finding #4):** the original `Stop` hook was wrong twice. (a) `Stop` fires **once per turn** ("when Claude finishes responding"), not at completion — a worker that pauses for input would spam `worker done` every turn; the end signal is `SessionEnd`. (b) `tmux display-message` with no `-t` resolves to the _attached client's active pane_ — i.e. the worker's own pane, never the dispatcher's. The repo's own worktrunk config documents this exact trap (`config.toml:19-20`). Fix: signal deterministically (the worker's last protocol step, gated on PR-created), targeting the **recorded** `dispatcher_pane` from `WORKER_TASK.md`; keep a `SessionEnd` hook only as a backstop.

**Primary signal — worker-driven (deterministic).** Add to `WORKER_PROTOCOL.md` (Task 4) as the final "When done" step: after the PR is open, the worker reads `dispatcher_pane` from its task file and posts one line to it:

```bash
tmux display-message -t "$dispatcher_pane" -d 4000 "worker done: $(git rev-parse --abbrev-ref HEAD) — PR $pr_url"
```

This fires exactly once, at real completion, at the right pane (the dispatcher's, captured at dispatch time). No hook required for the happy path.

**Backstop — `SessionEnd` hook** (catches a worker that dies/exits without signalling):

- [ ] **Step 1: Write the hook script** — `home/ai/claude-code/dispatch-notify.sh`

```bash
#!/usr/bin/env bash
# SessionEnd backstop: if a worker session ends without having signalled, ping the dispatcher.
# Reads the hook JSON on stdin (.cwd is delivered for SessionEnd).
set -euo pipefail

event="$(cat)"
cwd="$(jq -r '.cwd // empty' <<<"$event")"
[[ -f "$cwd/WORKER_TASK.md" ]] || exit 0

pane="$(grep -m1 '^dispatcher_pane:' "$cwd/WORKER_TASK.md" | cut -d' ' -f2)"
[[ -n "$pane" ]] || exit 0
branch="$(git -C "$cwd" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
tmux display-message -t "$pane" -d 4000 "worker exited: $branch — check state" 2>/dev/null || true
```

- [ ] **Step 2: shellcheck it** (required by CLAUDE.md)

Run: `shellcheck home/ai/claude-code/dispatch-notify.sh`
Expected: no warnings or errors.

- [ ] **Step 3: Register the `SessionEnd` hook in the overlay** — `home/ai/claude-code/default.nix`

Add a `writeShellScriptBin` next to the others (`default.nix:13-20`):

```nix
dispatch-notify = pkgs.writeShellScriptBin "claude-dispatch-notify" (builtins.readFile ./dispatch-notify.sh);
```

And a `SessionEnd` block inside `nix-settings-json.hooks` (sibling of `SessionStart`/`PreToolUse`):

```nix
SessionEnd = [
  {
    hooks = [
      {
        type = "command";
        command = "${dispatch-notify}/bin/claude-dispatch-notify";
      }
    ];
  }
];
```

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/dispatch-notify.sh home/ai/claude-code/default.nix
git commit -m "feat(claude): SessionEnd backstop notifying dispatcher"
```

---

## Phase 3 — Wiring, rebuild, and live smoke test

### Task 8: wire the `workflows/` directory

**Files:**

- Modify: `home/ai/claude-code/default.nix`

`agents/` and `commands/` are symlinked as whole dirs (`default.nix:167-170`); add `workflows/` the same way so `Workflow({name: 'spec-plan-critic'})` resolves. (Until this lands in the main checkout, the worker invokes by absolute `scriptPath` — see Task 3.)

- [ ] **Step 1: Read the home.file block**

Run: read `home/ai/claude-code/default.nix:157-172`.

- [ ] **Step 2: Add the workflows symlink** next to `agents`/`commands`:

```nix
".claude/workflows".source =
  config.lib.file.mkOutOfStoreSymlink "${config.home.homeDirectory}/nix-config/home/ai/claude-code/workflows";
```

- [ ] **Step 3: Verify the flake still evaluates**

Run: `nix flake check` _(invoke the `nix-rebuild` skill first — it documents the `git add` gotcha: untracked files are invisible to the flake, so `git add` the new files before building)._
Expected: no evaluation errors.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/default.nix
git commit -m "feat(claude): symlink workflows dir into ~/.claude"
```

### Task 9: open the PR

- [ ] **Step 1: Push and open the PR**

```bash
git push -u origin feat/21-dispatcher-harness
gh pr create --assignee @me --title "feat(claude): dispatcher → worker harness" --body "Closes #21"
```

Expected: PR created; pre-push/CI hooks pass.

### Task 10: post-merge live smoke test

> Cannot run before merge — the symlink dangle means new agents/workflows/fish-fn don't resolve from `~/.claude` until the branch is in `~/nix-config` and `nh home switch` runs there.

- [ ] **Step 1: Merge and rebuild on the main checkout**

```bash
wt merge          # merge feat/21-dispatcher-harness into main
# in ~/nix-config:
nh home switch    # invoke the nix-rebuild skill first
```

- [ ] **Step 2: Verify the symlinks resolved**

Run: `ls -l ~/.claude/agents/spec-critic.md ~/.claude/agents/plan-critic.md ~/.claude/workflows/spec-plan-critic.js`
Expected: all three resolve to the main checkout (no dangling link).

- [ ] **Step 3: Dispatch one real throwaway `standard` task** (e.g. a doc tweak in a scratch repo) and watch:
  - worktree + tmux window created with the right branch name,
  - `WORKER_TASK.md` written with `tier: standard` and `Closes #N`,
  - worker launches, runs `spec-plan-critic` (plan + plan-critic only), executes, pushes, opens a PR,
  - the Stop hook fires a `worker done` message to the dispatcher pane.

- [ ] **Step 4: Trigger the escalation path** — give a worker a deliberately under-specified task; confirm the critic loops at most twice then the PR body carries a `## Escalated` section. (Validates the revision cap and that escalations aren't silently dropped.)

### Task 11 (housekeeping, independent): toddl worktree cleanup

- [ ] Re-attach the toddl main checkout to `main` (it's in detached HEAD).
- [ ] Prune stale worktrees (16 → ~6 active) with `wt remove` per merged branch.

_(This is unrelated to the harness code and can be done any time; listed so it isn't forgotten.)_

---

## Self-Review

**Spec coverage** (issue #21 → task):

- Tiered pipeline → Task 4 (protocol) + Task 3 (workflow branches on `tier`). ✓
- `auto` permission model + safe allowlist → Task 5. ✓
- Independent adversarial critics + 2-rev cap → Tasks 1–3. ✓
- Workflow-vs-nested-session boundary → Task 3 (inner loop is a Workflow; dispatcher/worker stay sessions). ✓
- Notification hook (core) → Task 7. ✓
- Fan-out caps → Task 4 rule 1 (≤3 subagents); dispatcher ≤3 workers is a dispatcher-runtime concern, **not yet a coded limit** — see gap below.
- Split location (general vs project) → File Structure + Task 5. ✓
- `just dispatch` → reassigned to a fish function (Task 6) with rationale. ✓
- Housekeeping → Task 11. ✓

**Gaps surfaced by self-review:**

1. **Dispatcher ≤3-worker cap and the `just usage` budget gate are not implemented anywhere.** The protocol governs the _worker_; nothing enforces the _dispatcher_ fan-out. Either add a guard in `dispatch.fish` (count active worker worktrees, refuse past 3) or accept it as a manual discipline and say so. Flagged for decision.
2. **Tier classification is fully manual** (`dispatch <tier>`). The issue lists auto-classification as open; this plan does not close it. Acceptable for v1, but state it.
3. **The risky `Workflow` assumption is `scriptPath`, not name-resolution** (corrected per skeptic). `~/.claude/workflows/` resolution _is_ documented (so Task 8's by-name path is sound). What's undocumented is the pre-merge `scriptPath` fallback in Task 3 — invoking a worktree-absolute script before the symlink lands. Also undocumented in public refs: the exact `agent()` signature and the `{schema, agentType}` combination the inner script leans on. **Smoke-test the workflow (Task 10) before trusting either; don't claim the inner loop works until then.**

**Placeholder scan:** no "TBD"/"handle edge cases"/"similar to Task N" — concrete content in every code step. ✓

**Type/name consistency:** verdict schema identical in shared section and Task 3; agent names `spec-critic`/`plan-critic` consistent across Tasks 1–4 and the workflow `agentType`. ✓

---

## Execution Handoff

Plan saved to `docs/superpowers/plans/2026-06-22-dispatcher-harness.md`. Two execution options:

1. **Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks.
2. **Inline Execution** — execute in this session with checkpoints.

Decide after the skeptic review below.
