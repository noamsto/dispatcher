# SPEC — dispatch: validate the model slot against the engine before scaffolding

Issue #2. Branch `feat/2-dispatch-validate-the-model-slot-against`, base `extract`.

## 1. Decision

**Adopt a per-engine positive grammar as the mandatory gate, plus an opportunistic
authoritative cross-check for codex only, plus a documented env override.**

Three arms, one gate, hard fail, no soft pass:

| engine | mandatory floor                                                                                                                              | additional check                                      |
| ------ | -------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------- |
| claude | alias set ∪ `claude-*` id shape, **effort suffix forbidden**                                                                                 | none (no listing CLI exists)                          |
| codex  | `gpt-<gen>-<variant>` — **variant suffix mandatory** ∪ legacy bare set                                                                       | slug ∈ `~/.codex/models_cache.json` **when readable** |
| cursor | open multi-vendor id shape; **claude CLI aliases rejected**; `claude-*`/`gpt-*` ids must carry an effort suffix unless bracket-parameterized | none — see §1.2                                       |

The grammar is the invariant. The codex cache is a tightening, never a
prerequisite: if it is missing or unparseable the floor still rejects bare
`gpt-5.6`, which is the named failure.

### 1.1 Trade-off table

Failures referenced: **F1** = bare `gpt-5.6` on `--agent codex` (400 at launch,
after scaffolding). **F1m** = engine/model mismatch (claude model with
`--agent codex`, cursor id with `--agent claude`, …). **F2** = `composer-2.5`
has no effort rung for #1's execute ladder.

| option                                                           | F1                                     | F1m     | F2                 | verdict                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ---------------------------------------------------------------- | -------------------------------------- | ------- | ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1. family check only                                             | ✗ — `gpt-5.6` is in the `gpt-*` family | partial | ✗                  | **Rejected.** Misses the one failure that actually happened, and a naive family-prefix rule is _wrong_ for cursor, which legitimately fronts `claude-opus-4-8-high` and `gpt-5.6-sol-high`. A family check that special-cases cursor is already this spec's grammar, minus the part that catches F1.                                                                                                                                                                                                                                                                                                                                                                          |
| 2. strict canonical-map membership                               | ✓                                      | ✓       | ✓                  | **Rejected.** Every model bump becomes a `dispatch.sh` edit _and_ a nix rebuild, on a repo whose entire model-versioning discipline is "bump one table in a hot-reloadable protocol file". It also over-rejects: the map lists worker rungs, not the dispatchable set — `composer-2.5-fast`, `claude-opus-4-8-*`, `gemini-*`, `glm-*` are all legitimate cursor dispatches the map does not enumerate.                                                                                                                                                                                                                                                                        |
| 3. deny-list known-bad ids                                       | ✓ (only this one)                      | ✗       | ✗                  | **Rejected.** Catches exactly the failure already observed and nothing adjacent; `gpt-5.7` bare next quarter reproduces it verbatim.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| 4. query each engine's authoritative vocabulary at dispatch time | ✓                                      | ✓       | ✗                  | **Rejected as the primary mechanism, adopted for codex only.** For cursor it costs ~1.1s of network on every dispatch and **cannot express the bracket form** `claude-opus-4-8[context=1m,effort=high,fast=false]`, which is not a listed id — so membership would reject a legitimate dispatch. Its offline behaviour is a fork: hard-fail makes a list endpoint a dispatch dependency; soft-fail is a hole. For codex the source is a **local file**, zero-latency, zero-network, and present on every box where `--agent codex` is even reachable (work profile + `codex login`) — so it is worth taking as a _second_ check behind a floor that already holds without it. |
| **chosen — grammar floor + codex cache**                         | ✓                                      | ✓       | ✗ (deliberate, §5) | Catches both named failure classes with no per-model code churn, no network, and a closed-by-default posture.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |

### 1.2 Why cursor gets no membership check, stated plainly

Cursor's id space is **open by construction** — it fronts Anthropic, OpenAI,
Google, Moonshot, Zhipu and its own models, and adds vendors continuously. Any
membership rule there is either stale within weeks or a network call on the hot
path. The cursor arm is therefore a **mismatch + missing-effort-suffix gate**,
not a membership gate. That is the honest ceiling, and it is enough: it rejects
`opus`/`sonnet` under `--agent cursor`, and it rejects bare `gpt-5.6` and bare
`claude-opus-4-8` under `--agent cursor` (verified: cursor's `claude-*`/`gpt-*`
ids are effort-suffixed).

## 2. Placement

Insert the gate in `adapters/core/dispatch.sh` **immediately after the profile
gate** (after the `fi` closing at line 134) and **before the `--effort ultra`
gate at line 137**.

Rationale: it depends only on `$agent` and `$model`, both settled when the arg
loop exits at line 106; it joins the existing engine-consistency gate block
(profile 127-134, `ultra` 137-140, `--mcp` 141-161) rather than starting a
second one; the profile gate stays _ahead_ of it, so a personal-profile
`--agent codex` still reports the profile error instead of a confusing
missing-cache message; and it sits _above_ the `ultra` gate so that
`dispatch deep gpt-5.6-sol --agent claude --effort ultra` reports the wrong
engine/model pairing (the actual mistake) instead of masking it behind
"effort ultra is codex-only".

It precedes every side effect. Enumerated, in file order:

1. `crew reap --quiet` (173) — mutates _other_ worktrees
2. `gh issue create` (189) — mints a tracker issue
3. `wt switch -c "$branch"` (208) — creates branch + worktree on disk
4. `mkdir -p "$crew_dir"` (222)
5. append to `$crew_dir/events.jsonl` (226-230) — bus record
6. `crew identity` (233)
7. write `$wt_path/WORKER_TASK.md` (239-246)
8. `tmux new-window` (248) + `set-window-option` ×5 (252-256)
9. `tmux send-keys` worker launch (304 / 320 / 323)
10. `nohup crew stall-watch` (335) — detached background process

## 3. Acceptance rule (implementable as written)

Shared patterns (bash `[[ =~ ]]`, pattern held in a variable so it is not
glob-quoted — the file is bash-only: `# shellcheck shell=bash`, process
substitution at 248, `${branch//\//-}` at 199):

```
re_effort_tail='-(none|low|medium|high|xhigh|max)(-fast)?$'
claude_aliases: opus | sonnet | haiku | fable
```

`re_effort_tail` is deliberately left-unanchored: `gpt-5.5-extra-high` is a real
cursor id and matches on its trailing `-high`.

**Structure each arm as an explicit `if <reject condition>; then echo …; exit 1; fi`.**
A bare `[[ ]]` as the last statement of a function or branch returns 1 under
`set -e` and kills dispatch with no message.

Paths in code are `"$HOME/.codex/models_cache.json"`, never `~/…` — a tilde does
not expand inside a quoted string or a variable, and the tests set `$HOME`.

### claude

Accept iff:

- `$model` ∈ {`opus`, `sonnet`, `haiku`, `fable`}; **or**
- `$model` matches `^claude-[a-z0-9]+(-[a-z0-9]+)*$` **and** does **not** match
  `$re_effort_tail`.

Source: `claude --help` — "an alias for the latest model (e.g. 'fable', 'opus',
or 'sonnet') or a model's full name (e.g. 'claude-fable-5')". There is no
listing CLI; the `claude-*` prefix rule is the widest correct statement.

Accepts: `opus`, `sonnet`, `haiku`, `fable`, `claude-fable-5`, `claude-opus-5`,
`claude-haiku-4-5-20251001`.
Rejects: `gpt-5.6-sol`, `kimi-k3-high`, `cursor-grok-4.5-high`, `composer-2.5`,
`gemini-3.1-pro`, `claude-opus-4-8-high` (effort-suffixed → that is a _cursor_
id; claude takes `--effort` separately).

The effort-suffix exclusion is what makes the claude and cursor id spaces
disjoint at exactly the point a dispatcher would confuse them. Known
over-accept: `claude-<anything>` that does not exist upstream is accepted — no
authoritative source exists to tighten it, and the failure mode (a `claude
--model` error at launch) is the pre-existing one. Known **under**-accept: a
bracket-parameterized form (`claude-opus-5[1m]`) is rejected — `claude --help`
does not document one and it could not be verified without launching a session,
so it stays out of the grammar; the override in §5 covers it if it turns out to
be real.

### codex

Accept iff **both**:

1. **Grammar (mandatory).** `$model` matches `^gpt-[0-9]+\.[0-9]+-[a-z0-9]+$`
   (exactly one variant token, mandatory) **or** `$model` ∈ {`gpt-5.5`,
   `gpt-5.4`} — the legacy bare generations. That set is closed: older
   generations are never _added_.
2. **Cache cross-check (when available).** Two steps, never one — conflating
   them makes a corrupt or rotated cache block _all_ codex dispatch behind a file
   the user never edits, which is the availability failure §5 exists to prevent.

   a. **Usability probe.** `jq -e '.models|arrays|length > 0' "$cache" >/dev/null 2>&1`.
   Absent, unreadable, not JSON, or no non-empty `.models` array → **skip the
   whole cross-check** and pass on the grammar alone.
   b. **Membership.** Only if (a) succeeded:
   `jq -e --arg m "$model" 'any(.models[]; .slug == $m)' "$cache" >/dev/null`.
   Non-zero → **reject**.

   Splitting on the probe means jq's own exit statuses are never overloaded: (a)
   collapses every "cannot use this file" case to one branch, so (b)'s non-zero
   can only mean "the slug is genuinely not on this account".

Accepts: `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.4-mini`,
`gpt-5.5`, `gpt-5.4`.
Rejects **structurally, cache or no cache**: `gpt-5.6` (no variant — F1),
`codex-auto-review` (does not match `^gpt-` — the internal review model falls
out of the positive grammar with no deny-list entry), `opus`, `kimi-k3-high`,
`gpt-5.6-sol-high` (two suffix tokens = a cursor id pasted into codex).
Rejects **only with the cache**: `gpt-5.7-sol`, `gpt-5.6-nova` — right shape,
not on this account.

Mandatory-suffix is the structural answer to F1: it rejects bare `gpt-5.6`
_because of its shape_, not because it is on a list, so `gpt-5.7` bare is
rejected too.

### cursor

Parse once:

```
re_cursor='^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$'
```

`BASH_REMATCH[1]` = base id, `BASH_REMATCH[2]` = optional bracket block. The
value class includes `-` because cursor ships `gpt-5.5-extra-high`, so
`effort=extra-high` is a plausible override value.

Reject if the whole id does not match `$re_cursor`. Then, on the base:

- base ∈ {`opus`, `sonnet`, `haiku`, `fable`} → **reject** (claude CLI alias, never a cursor id).
- base == `auto` → accept.
- base matches `^(claude|gpt)-` and matches neither `$re_effort_tail` **nor**
  carries an `effort=` pair in the bracket block → **reject**.
- otherwise → accept.

The bracket block is an exemption from the effort-suffix rule only, not a blanket
accept: `claude-opus-4-8[context=1m,effort=high,fast=false]` (cursor's own
documented example) passes because it names an effort, while `gpt-5.6[detail=x]`
still fails for having no effort anywhere.

"Carries an `effort=` pair" means `[[ ${BASH_REMATCH[2]} =~ (\[|,)effort= ]]` —
anchored to a pair boundary, so `gpt-5.6[noeffort=x]` does not sneak through a
naive substring test.

**This rule is a conservative guess, not a verified one.** `cursor-agent --help`
calls the pairs "overrides", which implies unnamed parameters take defaults — so
`claude-opus-4-8[context=1m]` may well be legitimate and the gate rejects it.
That is unverifiable without launching a cursor session; §5's override is the
release valve, and the protocol text must not present the rule as grounded.

Accepts: `auto`, `composer-2.5`, `composer-2.5-fast`, `kimi-k3-low`,
`kimi-k3-high`, `kimi-k3-max`, `kimi-k2.7-code`,
`cursor-grok-4.5-medium-fast`, `cursor-grok-4.5-low-fast`,
`cursor-grok-4.5-high`, `gemini-3.1-pro`, `gemini-3.5-flash`, `glm-5.2-max`,
`claude-opus-5-high`, `claude-opus-4-8-high`, `claude-fable-5-thinking-xhigh`,
`gpt-5.6-sol-high`, `gpt-5.6-sol-high-fast`, `gpt-5.5-high`,
`claude-opus-4-8[context=1m,effort=high,fast=false]`.
Rejects: `opus`, `sonnet`, `Opus` (uppercase fails the shape),
`claude-opus-4-8` (no suffix, no brackets), `gpt-5.6-sol` (no suffix),
`gpt-5.6` (no suffix), `gpt-5.6[detail=x]` (bracket block names no effort).

**Launch-string consequence.** The bracket form is glob-active in the worker's
shell, and `dispatch.sh:320` interpolates `$model` unquoted into the string
`tmux send-keys` types there. Blessing the form obliges single-quoting `$model`
in the cursor launch string (`--model '$model'`); the existing cursor launch test
asserts `--model kimi-k3-high` and must be updated to the quoted form.

`composer-2.5` does **not** start with `claude-`/`gpt-`, so the suffix rule
correctly leaves it alone — it genuinely has no effort variants.

## 4. Error messages

Shape: `dispatch: model '<model>' <what is wrong> — <what would be valid>`,
one line to stderr, `exit 1`. The `dispatch: ` prefix and the exit path match the
existing gates; the "Did you mean" clause and the doc pointer are new — the
incumbent gates are bare one-liners.

Literal strings:

```
dispatch: model 'gpt-5.6' is not a codex slug — the 5.6 family ships only as variants (gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna); there is no bare gpt-5.6. See dispatch-orchestration.md "Model gate".

dispatch: model 'gpt-5.7-sol' is not in this account's codex model list (~/.codex/models_cache.json: gpt-5.6-sol, gpt-5.6-terra, codex-auto-review, gpt-5.6-luna, gpt-5.5, gpt-5.4, gpt-5.4-mini). If it is genuinely new, set DISPATCH_SKIP_MODEL_CHECK=gpt-5.7-sol and update the model map. See dispatch-orchestration.md "Model gate".

dispatch: model 'opus' does not match --agent codex — codex takes gpt-* variant slugs (e.g. gpt-5.6-sol). Did you mean --agent claude? See dispatch-orchestration.md "Model gate".

dispatch: model 'kimi-k3-high' does not match --agent claude — claude takes an alias (opus, sonnet, haiku, fable) or a full claude-* id (e.g. claude-fable-5). Did you mean --agent cursor? See dispatch-orchestration.md "Model gate".

dispatch: model 'claude-opus-4-8-high' is an effort-suffixed cursor id — on --agent claude pass the bare id and set intensity with --effort. Did you mean --agent cursor? See dispatch-orchestration.md "Model gate".

dispatch: model 'sonnet' does not match --agent cursor — cursor needs a full model id (e.g. kimi-k3-high, cursor-grok-4.5-medium-fast, composer-2.5, claude-opus-4-8-high). Did you mean --agent claude? See dispatch-orchestration.md "Model gate".

dispatch: model 'gpt-5.6-sol' is not a cursor id — cursor's claude-*/gpt-* ids carry an effort suffix (gpt-5.6-sol-high, gpt-5.6-sol-high-fast) because cursor has no --effort knob. Live list: cursor-agent --list-models. See dispatch-orchestration.md "Model gate".
```

The account slug list in the cache-miss message is **generated from the cache**,
not hardcoded — hardcoding it would reintroduce option 2 in the error text. It is
filtered to slugs the grammar actually accepts
(`jq -r '[.models[].slug|select(startswith("gpt-"))]|join(", ")'`); an unfiltered
list advertises `codex-auto-review`, an internal review model the gate rejects.

Every message ends with `See dispatch-orchestration.md "Model gate".`

Only the cache-miss message names `DISPATCH_SKIP_MODEL_CHECK` inline: that is
the one rejection whose _likely_ cause is a genuinely new model (the shape was
right). Grammar rejections are typos or engine mix-ups; naming a one-variable
bypass there trains the dispatcher to paper over them. Every message signposts
the protocol section, which documents the override — discoverable without being
one keystroke away.

## 5. Escape hatch

**`DISPATCH_SKIP_MODEL_CHECK=<the exact model id>`** — an env var, matching the
existing env surface (`DISPATCH_PROFILE`, `DISPATCH_SHAPE`, `DISPATCH_SPEC`,
`DISPATCHER_PROTOCOL_DIR`); no new CLI flag.

**Truthiness is exact string equality with `$model`**, not "is set". `=1`,
empty, and any other value do **not** bypass — an unset-or-mismatched value
takes the normal path with no warning. So the bypass is per-model by
construction: a dispatcher that `export`s it (the way `$CREW_ID` already flows
through a session) still gets the gate on every _other_ model, which is what
kills the "set once and forgotten" hole.

On a match the gate is skipped entirely and dispatch prints to stderr:

```
dispatch: model check skipped (DISPATCH_SKIP_MODEL_CHECK) — 'gpt-5.7-sol' on --agent codex is unverified
```

**Why an override must exist.** `dispatch.sh` is baked into a nix store path;
unlike the protocol files it has no `DISPATCHER_PROTOCOL_DIR`-style hot-reload.
A gate that is wrong about a brand-new model would block _all_ dispatch of that
model behind an edit + `nh home switch`, at exactly the moment the fleet is most
useful. That is an unacceptable single point of failure for a tool whose job is
to keep work moving.

**Why it does not re-open the hole.** The hole was _silence_: any string was
accepted with no signal, so a wrong model surfaced as a 400 in a tmux pane after
a worktree existed. The override changes the default from open to closed and
makes opening it (a) explicit — a variable the dispatcher must consciously set,
(b) **narrow** — it names one model id, so it can never blanket-disable the gate
even if exported for a session, and (c) loud — a stderr warning naming the
engine/model. It also cannot cause a _silent_ wrong dispatch: the warning
precedes the scaffold. The residual risk is habituation, mitigated by the
protocol text below.

**Obligation.** `dispatch-orchestration.md` states that using the override means
the model map is stale, and the map must be updated in the same session. The map
is a protocol file, hot-reloadable via `DISPATCHER_PROTOCOL_DIR` — so the doc fix
lands immediately and the `dispatch.sh` grammar fix follows on the next rebuild.

## 6. Docs

Claims that become **false** and must change:

1. **`adapters/core/protocols/dispatch-orchestration.md` line 81** — "`--model`
   is free, so a cursor worker can still front **`composer-2.5`** /
   **`composer-2.5-fast`** (no effort variants) as an alternative, or an
   effort-suffixed `claude-opus-4-8-*` / `gpt-5.6-sol-*`." Only **"is free"**
   becomes false; the rest of the sentence stays true and is precisely what the
   cursor arm is shaped to keep passing. Reword to "`--model` is open across
   cursor's whole multi-vendor id space (the gate checks id _shape_, not
   membership of this table), so …" and keep the enumeration verbatim.
2. **`adapters/core/protocols/dispatch-orchestration.md` lines 84-85** —
   "`dispatch` does not validate the model slot — the map is enforced by the
   dispatcher's judgment, not by code." Replace with a **"Model gate"**
   subsection stating: what is enforced per engine (the three arms of §3), that
   codex additionally cross-checks `$HOME/.codex/models_cache.json` when usable,
   that **ladder membership is still judgment** (the gate enforces
   dispatchability, not tier-appropriateness), and the override plus its
   update-the-map obligation.
3. **`adapters/core/protocols/DISPATCHER_PROTOCOL.md` line 76 ("Engine." bullet)** —
   "The `<model>` slot must match the engine: …" currently reads as a norm. It
   becomes an enforced rule: add that `dispatch` rejects a mismatched or
   unsupported model **before scaffolding**, phrased in parallel with the
   bullet's existing "`dispatch` rejects `--agent codex`/`--agent cursor` there
   before scaffolding" clause.

Explicitly **unchanged**:

- **`adapters/cursor/rules/dispatcher.mdc`** — verified: 21 lines, mentions
  "tier + engine + model + effort" only as what a dispatcher judges. It asserts
  nothing about validation, so it needs no edit. Recorded here so nobody hunts
  for the claim.
- **`docs/superpowers/specs/*.md`** and `docs/superpowers/plans/*.md` — dated
  historical design records that also say "does not validate the model slot".
  They are snapshots of what was true when written; do not rewrite history.

**Regeneration.** After editing `adapters/core/protocols/*`, run
`bash scripts/gen-adapters.sh`. It copies `adapters/core/protocols` verbatim
into `adapters/claude-code/plugin/protocols/` and
`adapters/codex/plugin/protocols/`. CI regenerates and asserts no diff, and
`tests/adapters.bats` asserts idempotence — a missed regeneration fails both.

## 7. Acceptance criteria

Each line is one bats assertion in `tests/dispatch.bats` unless marked.

### 7.1 Hermeticity prerequisite (do this first — every row below depends on it)

- [ ] In `setup()`, **after** `setup_repo` (which is what creates `$TEST_REPO`):
      `export HOME="$TEST_REPO"` and
      `unset DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE`.
      `DISPATCH_PROFILE=work` is exported in a real work shell and `CREW_ID` in any
      dispatcher session; bats inherits both, so without the `unset` these tests
      pass on a work box and fail on a personal one. `TMUX_PANE` is inherited from
      any tmux session and lands in `WORKER_TASK.md` (dispatch.sh:241). `HOME`
      isolates the codex cache fixture and the `--mcp` config paths from the
      developer's real `~`. (Full env read-set of dispatch.sh, all covered:
      `DISPATCHER_PROTOCOL_DIR`, `CREW_ID`, `DISPATCH_PROFILE`, `HOME`,
      `DISPATCH_SHAPE`, `TMUX_PANE`, `DISPATCH_SPEC`.)

- [ ] **`setup_repo` in `tests/helpers.bash` must export `GIT_CONFIG_GLOBAL=/dev/null`**
      before `git init`. Overriding `HOME` alone does _not_ detach git from the
      developer's global config: `XDG_CONFIG_HOME` is exported in a real shell, so
      git still reads `~/.config/git/config`, whose `commit.gpgSign = true` and
      tilde-relative `user.signingkey` now re-expand against the _new_ `HOME`.
      Every exit-0 row then dies at `stub_launch_bins`'s
      `git -C "$TEST_REPO" commit --allow-empty` with status 128
      ("Couldn't load public key … No such file or directory"). This is a
      prerequisite of §7.1's `HOME` export, not an optional hardening — without it
      **all** acceptance rows fail. Git isolation belongs in `setup_repo`, so every
      bats file gets it, not just `dispatch.bats`.

### 7.2 Invocation shape — every row is a full command

The gate sits _below_ the crew-id check (dispatch.sh:116) and the profile gate
(127-134), so **every** row must supply `--crew-id c1`, and every codex/cursor
row must set `DISPATCH_PROFILE=work`. Every **exit-0** row must additionally call
`stub_launch_bins` and pass an existing issue number (`42`) plus a title — the
generic `gh` stub prints nothing, so an omitted issue number makes `$num` empty
and dispatch exits 1 at dispatch.sh:191 for an unrelated reason.

Reject rows: `DISPATCH_PROFILE=work run run_dispatch <tier> <model> --agent <e> --effort <x> --crew-id c1 42 "title"`
Accept rows: same, preceded by `stub_launch_bins`.

(claude rows may omit `DISPATCH_PROFILE`, but set it anyway for uniformity.)

### 7.3 Gate behaviour

Rejections — each exits 1 with the named stderr fragment:

- [ ] `deep gpt-5.6 --agent codex --effort high` → `there is no bare gpt-5.6`.
- [ ] Same invocation scaffolds nothing: `[ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"`.
      Mirrors the existing profile-gate test. Non-vacuous for _this_ gate only
      because the row reaches it (profile is `work`, crew-id supplied) — moving
      the gate below dispatch.sh:208 makes it fail.
- [ ] `standard opus --agent codex --effort medium` → `does not match --agent codex`.
- [ ] `standard kimi-k3-high --agent claude --effort medium` → `does not match --agent claude`.
- [ ] `standard claude-opus-4-8-high --agent claude --effort medium` → `effort-suffixed cursor id`.
- [ ] `standard sonnet --agent cursor --effort medium` → `does not match --agent cursor`.
- [ ] `standard gpt-5.6-sol --agent cursor --effort medium` → `effort suffix`.
- [ ] `deep gpt-5.6-sol --agent claude --effort ultra` → the **model** message, not
      `effort ultra is codex-only` (pins the §2 ordering against the `ultra` gate).

Codex cache arm — fixture written to `$HOME/.codex/models_cache.json`
(`mkdir -p "$HOME/.codex"` first):

- [ ] With a fixture holding the verified slug set, `deep gpt-5.7-sol --agent codex --effort high`
      exits 1; stderr names `models_cache.json` and lists `gpt-5.6-sol`.
- [ ] With a fixture, `deep gpt-5.6-sol --agent codex --effort high` exits 0 (`stub_launch_bins`).
- [ ] With **no** fixture, `deep gpt-5.6 --agent codex --effort high` still exits 1 —
      the grammar floor holds without the authoritative source.
- [ ] With **no** fixture, `standard gpt-5.6-terra --agent codex --effort high` exits 0
      and reaches `send-keys` — an absent cache is a skip, not a hard fail.
- [ ] With a fixture that is **not valid JSON** (`printf 'not json'`),
      `standard gpt-5.6-terra --agent codex --effort high` exits 0 — an unusable
      cache must never block all codex dispatch (B4).

Acceptances (all with `stub_launch_bins`, all exit 0):

- [ ] `standard composer-2.5 --agent cursor --effort medium` reaches `send-keys` with `--model 'composer-2.5'`.
- [ ] `deep 'claude-opus-4-8[context=1m,effort=high,fast=false]' --agent cursor --effort high`.
- [ ] `deep claude-fable-5 --agent claude --effort high` and `trivial haiku --agent claude --effort low`.

Override:

- [ ] `DISPATCH_SKIP_MODEL_CHECK=gpt-5.6` with `deep gpt-5.6 --agent codex --effort high`
      exits 0, reaches `send-keys`, stderr contains `model check skipped`.
- [ ] `DISPATCH_SKIP_MODEL_CHECK=1` with the same invocation still exits 1 — the
      override is exact-match on the model id, not a boolean.
- [ ] `DISPATCH_SKIP_MODEL_CHECK=gpt-5.6` with `standard opus --agent codex --effort medium`
      still exits 1 — an exported override does not blanket-disable the gate.

Map conformance:

- [ ] One table-driven test asserting every model named in `dispatch-orchestration.md`'s
      model map, its cursor-alternatives prose (lines 81-83), its codex legacy
      generations (line 55), and the orchestrator table passes its engine's arm:
      `opus`, `sonnet`, `haiku`, `claude-fable-5`;
      `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`, `gpt-5.4`, `gpt-5.4-mini`;
      `kimi-k3-high`, `cursor-grok-4.5-high`, `cursor-grok-4.5-medium-fast`,
      `cursor-grok-4.5-low-fast`, `composer-2.5`, `composer-2.5-fast`,
      `claude-opus-4-8-high`, `gpt-5.6-sol-high`.
      The list is **hand-copied**, so it does not make contradiction impossible —
      it makes the common drift loud. Do not claim more than that.
- [ ] The three existing launch tests still pass; the cursor one updates its
      `--model kimi-k3-high` assertion to the single-quoted form.

Repo gates:

- [ ] `shellcheck adapters/core/*.sh scripts/*.sh` clean.
- [ ] `bats tests/` green.
- [ ] `bash scripts/gen-adapters.sh` run and its output committed; `git diff` after
      a second run is empty.

## 8. Non-goals

- **No scaffolding redesign.** The gate is an insert into the existing
  pre-side-effect block; nothing in the worktree/tmux/launch path moves. The one
  launch-path edit is single-quoting `$model` in the cursor `send-keys` string —
  in scope only because this spec is what formally blesses the glob-active
  bracket form (§3, cursor).
- **No model-selection redesign.** The gate enforces _dispatchability_, not
  ladder membership. **F2 is deliberately not caught**: `composer-2.5` is a
  legitimate cursor id that `dispatch-orchestration.md` explicitly sanctions as
  a worker alternative. Whether it has a lower execute rung is #1's
  `WORKER_PROTOCOL.md` rule-1 concern, not dispatch's.
- **No bus-schema change.** No `model_checked` field on the `events.jsonl`
  dispatch record; the stderr warning is the audit trail. Schema changes are
  outside "validation and its docs/tests".
- **No new CLI flag.** The override is env-only.
- **`adapters/core/dispatcher.sh`'s own `--model` slot stays unvalidated.** The
  orchestrator session picks its model from the "Orchestrator engines" table, not
  the worker model map, and issue #2 names `dispatch <tier> <model>`. Recorded so
  the next reader sees an omission that is deliberate.
- **No `cursor-agent --list-models` call.** Rejected in §1.1/§1.2.
- **No fix for the `kimi-k3` doc drift** (line 78-79 claims `kimi-k3-high` is the
  only slug; `kimi-k3-low`/`-max` also exist). Out of scope — but the rewritten
  "Model gate" text must not re-assert it.
