# Dispatch orchestration — the choosing tree

Canonical reference for how the dispatcher judges a task into **tier**, **engine**, and **model**. Keep it in sync with `DISPATCHER_PROTOCOL.md` (the baked rubric) and `dispatch.sh` (the mechanism).

```mermaid
flowchart TD
    T(["Incoming task"]) --> TIER{"Tier? (pipeline depth)"}
    T --> ENG{"Engine? (judge per task: claude ⇄ codex ⇄ cursor — no default)"}

    TIER -->|"ambiguous / architectural / security / wide blast"| DEEP["deep — spec + plan critics"]
    TIER -->|"bounded, clear, few files"| STD["standard — plan-critic"]
    TIER -->|"mechanical / single-file / rename"| TRIV["trivial — no critics"]

    ENG -->|"large mechanical refactor, wide sweep, 2nd-engine perspective"| CODEX["Codex"]
    ENG -->|"UI/frontend, ambiguous spec, security-adjacent"| CLAUDE["Claude"]
    ENG -->|"Grok 4.5 (or Composer), distinct 3rd perspective on deep tasks"| CURSOR["Cursor"]

    CLAUDE --> OPUS["opus — deep"]
    CLAUDE --> SONNET["sonnet — standard/trivial"]
    CLAUDE --> HAIKU["haiku — truly trivial"]
    CODEX --> GH["codex · deep → model map"]
    CODEX --> GM["codex · standard → model map"]
    CODEX --> GL["codex · trivial → model map"]
    CURSOR --> CU["cursor · tier → model map"]

    OPUS --> D[["dispatch &lt;tier&gt; &lt;model&gt; [--agent claude|codex|cursor] [id] &lt;title&gt;"]]
    SONNET --> D
    HAIKU --> D
    GH --> D
    GM --> D
    GL --> D
    CU --> D
```

## Model map (single source of truth)

The dispatcher picks the tier-appropriate model for the chosen engine from this
table. This table is the only place concrete **worker** model versions appear —
prose elsewhere says "the tier-appropriate model from the model map".
Orchestrator (dispatcher-session) defaults live in "Orchestrator engines" below;
consult-roster versions live in `WORKER_PROTOCOL.md` → "Orchestration consult".
Bump the matching table when a new model ships.

| Tier       | claude (worker → execute → escalate)                                                                                                                                                                                                                                     | codex (worker → execute → escalate)                         | cursor (worker → execute → escalate)                                                          |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `deep`     | **opus** → **sonnet** → escalated **opus** — escalate worker to **`claude-fable-5`** only for a genuinely hard, well-specified, long-horizon task where opus is demonstrably not enough (≈2× opus cost, refusal-classifier risk on security-adjacent code, minutes-long turns; most expensive lever, used rarely) | **`gpt-5.6-sol`** → **terra** → escalated **sol**           | **`kimi-k3-high`** → **`cursor-grok-4.5-medium-fast`** → escalated **`cursor-grok-4.5-high`** |
| `standard` | **sonnet** → **sonnet** → escalated **opus**                                                                                                                                                                                                                             | **`gpt-5.6-terra`** → **luna** → escalated **terra**        | **`cursor-grok-4.5-medium-fast`** → **`cursor-grok-4.5-low-fast`** → escalated **medium-fast** |
| `trivial`  | **sonnet** (or **haiku** if truly trivial) — no delegation                                                                                                                                                                                                               | **`gpt-5.6-luna`** — no delegation                          | **`cursor-grok-4.5-low-fast`** — no delegation                                                |

Codex model ids carry a **variant suffix** — the 5.6 family ships as
`-sol` (frontier) / `-terra` (balanced everyday) / `-luna` (fast + affordable),
and there is **no bare `gpt-5.6`** — dispatching one dies on a 400, "model is not
supported when using Codex with a ChatGPT account". Authoritative list for this
account is `jq -r '.models[].slug' ~/.codex/models_cache.json` (also `gpt-5.5`,
`gpt-5.4`, `gpt-5.4-mini` — previous generations, no longer a rung here).

Codex reasoning effort still scales with tier automatically (deep→high,
standard→medium, trivial→low), independent of which gpt model is chosen. Above
`xhigh` the ladder continues with `max` (both engines) and codex-only `ultra`
(maximum reasoning with automatic task delegation) — `ultra` exists on `-sol` /
`-terra` only, `-luna` caps at `max`. Session effort `ultra` is itself an
orchestration layer: `dispatch` still enables `agents.*` but pins
`agents.default_subagent_reasoning_effort` one rung below the session (floor
`low`, **never** `ultra` — an ultra subagent would nest automatic delegation).
The worker must not add a second harness execute-subagent orchestration on top
of ultra's built-in delegation. See `WORKER_PROTOCOL.md` rule 1.

Both codex launch paths (`dispatch` workers and the `dispatcher --agent codex`
orchestrator) pin `service_tier="default"` — the interactive `/fast` toggle
persists locally and would otherwise leak into unattended runs, burning ChatGPT
credits at 2.5x for latency nobody is watching.
Cursor has **no reasoning-effort flag** — effort is baked into the model id
suffix and `dispatch`'s `--effort` is accepted-and-ignored for cursor. Grok
exposes both an effort suffix (`-low`/`-medium`/`-high`) and a throughput
suffix (`-fast`), so the dispatcher expresses effort by _picking the id_: the
standard/trivial rows use `-fast` for cheap, high-throughput turns; cursor `deep`
uses **`kimi-k3-high`** as the worker (plans) and Grok as the execute ladder
(implements) — escalate to `cursor-grok-4.5-high`, not back to Kimi. **`kimi-k3`
has no lower-effort Cursor slug** (only `kimi-k3-high`). **Grok 4.5 remains the
default cursor distinct-implementer** — a genuinely non-Claude perspective, which
is the point of reaching for cursor. `--model` is open across cursor's whole
multi-vendor id space (the gate checks id _shape_, not membership of this
table), so a cursor worker can still front **`composer-2.5`** /
**`composer-2.5-fast`** (no effort variants) as an alternative, or an
effort-suffixed `claude-opus-4-8-*` / `gpt-5.6-sol-*`. `dispatch` validates the
model slot against `--agent` before scaffolding — see **Model gate** below.

**Worker-session model vs execute-subagent model.** The first model in each map
cell is the **worker session** — it does spec / plan / reconcile / judging. The
second is the **default execute subagent**; the third is the escalated execute
rung for plan-tagged high-risk steps. All three engines follow this split on
standard/deep (trivial does not delegate). See `WORKER_PROTOCOL.md` rule 1 (and
the fast deterministic gate + parallel review gate it describes). `dispatch.sh`
sets codex `agents.*` guardrails and a process-authority spawn clause only —
never model slugs for execute subagents (those stay in this table / rule 1).
Cursor has no CLI concurrency cap; the cap of 3 is protocol-only.

**Shape-tag vocabulary.** The outcome log's `shape` field is a closed set:
`mechanical`, `ui`, `ambiguous`, `security`, `wide`.

**Orchestration consult (worker-side, deep).** Decomposition help from a top-tier consultant — **fable** (default), **gpt-5.6-sol** via the read-only codex MCP, or **cursor-grok-4.5-high** via a `cursor-agent -p` one-shot — is decided **in the worker's worktree** at the plan seam (whether *and* which), not by the dispatcher — the dispatcher's only lever is tiering the task `deep` (its existing "architectural / wide-blast" signal). Codex/cursor consults are work-profile only. See `WORKER_PROTOCOL.md` → "Orchestration consult". Every deep worker emits an outcome-metrics record to the bus at finish:
`crew msg worker:<branch> metrics:<crew_id> '{"consulted":…,"consult_engine":…,"plan_critic_first_pass":…,"rework_count":…,"review_high":…}'`.
It rides `crew msg` (no `crew.sh` change) and never wakes the dispatcher. Consulted vs non-consulted deep workers are the A/B for whether the consult lever pays — `consult_engine` splits it by consultant — the counterfactual #86's oracle gate needs. Read it offline: `crew log <crew> | jq 'select(.to|startswith("metrics:"))'`.

### Model gate

`dispatch` validates `<model>` against `--agent` **before** it scaffolds
anything — no issue, no branch, no worktree, no window. It checks per-engine id
_shape_, not membership of the table above, so a model bump needs no
`dispatch.sh` edit:

- **claude** — an alias (`opus`, `sonnet`, `haiku`, `fable`) or a full
  `claude-*` id. An effort suffix is rejected: `claude-opus-4-8-high` is a
  _cursor_ id, and claude takes intensity through `--effort`.
- **codex** — `gpt-<gen>-<variant>`, variant mandatory, which is what rejects a
  bare `gpt-5.6`; `gpt-5.5` / `gpt-5.4` pass as legacy bare generations. When
  `$HOME/.codex/models_cache.json` is readable and holds a non-empty `.models`
  array, the slug must also appear in it — an absent or unusable cache is
  skipped, never fatal.
- **cursor** — an open multi-vendor id space, so shape only: claude CLI aliases
  are rejected, and a `claude-*` / `gpt-*` id must carry an effort suffix
  (`gpt-5.6-sol-high`) or name one in a bracket block
  (`claude-opus-4-8[context=1m,effort=high,fast=false]`). The bracket rule is a
  conservative guess — `cursor-agent` calls the pairs "overrides", so a block
  that omits `effort=` may well be legitimate and still get rejected. That is
  what the override below is for.

The gate enforces **dispatchability, not tier-appropriateness**. Picking the
rung that fits the task stays the dispatcher's judgment, and the map above stays
the source of truth for it.

**Override.** `DISPATCH_SKIP_MODEL_CHECK=<the exact model id>` skips the gate for
that one id and warns on stderr. Truthiness is exact string equality with the
model, not "is set" — exporting it for a session still gates every _other_
model. Reaching for it means **the map above is stale**: update the map in the
same session. The map is a protocol file, hot-reloadable through
`DISPATCHER_PROTOCOL_DIR`, so the doc fix lands immediately; the `dispatch.sh`
grammar follows on the next rebuild.

## Orchestrator engines (dispatcher session)

The dispatcher itself can run on any engine — `dispatcher --agent claude|codex|cursor`
(work-profile gated, same as workers). Orchestrator defaults — bump this table when a
model ships:

| engine | model | effort |
| ------ | ----- | ------ |
| claude | opus (settings default) | settings default |
| codex | **gpt-5.6-sol** | **high** — not xhigh: blocked workers wait on a bounded ~300s in-band window |
| cursor | **kimi-k3-high** | fixed in the model id (no knob; `--model` overrides: composer-2.5, cursor-grok-4.5-*) |

claude bakes `DISPATCHER_PROTOCOL.md` as a system prompt; codex/cursor inject it as
the first prompt (neither CLI has an append-system-prompt flag). The judging rubric
is identical across engines; the crew-watch park primitive is not — see
`DISPATCHER_PROTOCOL.md` → "Read the bus".

## Three orthogonal levers

- **Tier = pipeline depth (who reviews).** Driven by risk/ambiguity/blast-radius, not size. A one-line security change is still `standard`/`deep`. Pipeline depth also flexes **down** when the target repo self-reviews: a repo with its own automated PR-review gauntlet (Cursor Bugbot / Codex / a review GitHub App) on top of CI makes the worker's internal model review largely redundant, so the worker downgrades it (see `WORKER_PROTOCOL.md` → Code review gate, "Repo-aware scaling"). `mono` is such a repo. Tier still sets *planning* depth regardless — the downgrade only touches the post-execute review.
- **Engine = who implements.** Judged per task (claude ⇄ codex ⇄ cursor) — no default, and **on neutral fit rotate to the least-recently-dispatched engine** rather than drifting back to claude (see `DISPATCHER_PROTOCOL.md` engine lever). Codex (work profile only) for large mechanical refactors, wide sweeps, or a deliberate second-engine perspective; cursor (work profile only) for a distinct third-engine perspective on deep tasks — default the worker to **`kimi-k3-high`** with **Grok 4.5** execute subagents (`cursor-grok-4.5-*`), genuinely non-Claude; Composer stays available as an alternative; claude for UI/frontend work, security-adjacent code, or _genuinely_ underspecified work (mild ambiguity alone isn't a claude ticket). Soft rule: don't front Claude _through_ cursor when the point is an independent third perspective — a cursor-fronted sonnet isn't independent review of a claude worker; use a Grok (or Composer) model for that.
- **Model/effort = how strong / how hard it thinks.** All engines pick the tier-appropriate model from the model map; codex reasoning effort scales with tier (deep→high, standard→medium, trivial→low) independent of which model is chosen, while cursor folds effort into the model id (no separate knob).

## MCP is no longer a routing factor

All engines defer MCP tool schemas, so the base stack (context7, playwright, firefox-devtools) is ~free until a tool is used:

- **Claude** — deferred by default via tool-search (haiku is the one eager exception).
- **Codex** — schemas deferred (baked-in `always_defer_mcp_tools`; measured ~0 token cost), browsers launch lazily on first use, and the base stack is provisioned from the same `mcp-servers.nix` source via the nix-generated `--profile worker`.
- **Cursor** — base stack comes from the single shared `~/.cursor/mcp.json` (same `mcp-servers.nix` source); there's no per-invocation MCP-config flag, so there's no separate worker profile. The dispatch launch passes `--approve-mcps` for unattended auto-approval. Codebase **indexing is disabled** (`--disable-indexing --disable-codebase-ref`, `CURSOR_CLI_INDEXED_GREP=0`) for parity with claude/codex (read + grep, no semantic index) and to skip a merkle index build over a large monorepo — not as a stall fix. The worker runs **cursor's interactive TUI** (a bare prompt argument, no `-p`), like the claude and codex launches: it repaints as it works, so the pane stays a truthful liveness signal for the stall watchdog and legible to a human. Headless `-p` is wrong for a worker — `--output-format text` prints only the final message, so a running worker reads as a wedge (the real #103), and `stream-json` only cures that by relaying events through a formatter. `-p` is still right for one-shot consults, where stdout is the product.

The additive `--mcp analytics` (posthog, work) profile stays claude-only — for codex _and_ cursor `--mcp` is rejected; their base MCP comes from their own profile. `cad`/freecad was dropped.
