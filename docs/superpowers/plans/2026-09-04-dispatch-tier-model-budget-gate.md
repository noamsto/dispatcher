# PLAN — dispatch: tier↔model gate and budget-aware rung selection

Implements SPEC.md (revision 2, finalized after the 2-revision spec-critic
cap — see "Escalated"/PR-body note on the two mechanical fixes folded in
after the final critique instead of a fourth async cycle).

## Step 1: `dispatch.sh` — `--ignore-map` flag + `usage()`

File: `adapters/core/dispatch.sh`

- [ ] In the arg-parsing loop (near the existing `--ignore-budget)` case,
      `dispatch.sh:135-138`), add:

  ```bash
  --ignore-map)
    ignore_map=1
    shift
    ;;
  ```

      and declare `ignore_map=""` alongside the other flag defaults (near
      `ignore_budget=""`, `dispatch.sh:59`).

- [ ] `usage()` (`dispatch.sh:10`): add `[--ignore-map]` next to
      `[--ignore-budget]`.

## Step 2: `dispatch.sh` — Gate 1 (tier↔model)

File: `adapters/core/dispatch.sh`, inserted immediately after the existing
dispatchability gate's closing `fi` (the `if [ "${DISPATCH_SKIP_MODEL_CHECK…"`
block ends at line 289) and before the `--effort ultra` check (currently
line 292).

- [ ] Implement per SPEC.md §2's table, as a `case "$agent" in … esac`
      producing a human-readable `tier_expected` string and a pass/fail
      bash regex/glob test per `$tier`. Suggested shape (adapt to match the
      file's existing style — explicit `if <reject>; then echo …; exit 1; fi`
      per line, not a bare `[[ ]]` as the last statement, per the codebase's
      own convention already visible in the existing gate):

      ```bash
      # Tier↔model gate (issue #89). Enforces tier-appropriateness on top of
      # the dispatchability gate above — see dispatch-orchestration.md
      # "Tier map". DISPATCH_SKIP_MODEL_CHECK does not cover this gate (it is
      # about shape/cache staleness, not tier); --ignore-map does.
      if [ -z "${ignore_map:-}" ]; then
        tier_ok=1
        tier_expected=""
        case "$agent" in
        claude)
          case "$tier" in
          deep)
            tier_expected="opus, claude-opus-*, sonnet, claude-sonnet-*, fable, or claude-fable-*"
            [[ $model =~ ^(opus|claude-opus-.*|sonnet|claude-sonnet-.*|fable|claude-fable-.*)$ ]] || tier_ok=0
            ;;
          standard)
            tier_expected="sonnet or claude-sonnet-*"
            [[ $model =~ ^(sonnet|claude-sonnet-.*)$ ]] || tier_ok=0
            ;;
          trivial)
            tier_expected="sonnet, claude-sonnet-*, haiku, or claude-haiku-*"
            [[ $model =~ ^(sonnet|claude-sonnet-.*|haiku|claude-haiku-.*)$ ]] || tier_ok=0
            ;;
          esac
          ;;
        codex)
          re_codex_legacy='^(gpt-5\.5|gpt-5\.4|gpt-5\.4-mini)$'
          case "$tier" in
          deep)
            tier_expected="gpt-5.6-sol, gpt-5.6-terra, or a legacy generation (gpt-5.5, gpt-5.4, gpt-5.4-mini)"
            [[ $model =~ ^(gpt-5\.6-sol|gpt-5\.6-terra)$ ]] || [[ $model =~ $re_codex_legacy ]] || tier_ok=0
            ;;
          standard)
            tier_expected="gpt-5.6-terra, gpt-5.6-luna, or a legacy generation (gpt-5.5, gpt-5.4, gpt-5.4-mini)"
            [[ $model =~ ^(gpt-5\.6-terra|gpt-5\.6-luna)$ ]] || [[ $model =~ $re_codex_legacy ]] || tier_ok=0
            ;;
          trivial)
            tier_expected="gpt-5.6-luna or a legacy generation (gpt-5.5, gpt-5.4, gpt-5.4-mini)"
            [[ $model =~ ^gpt-5\.6-luna$ ]] || [[ $model =~ $re_codex_legacy ]] || tier_ok=0
            ;;
          esac
          ;;
        cursor)
          # Self-contained for $cursor_base/$cursor_params (unset on the
          # DISPATCH_SKIP_MODEL_CHECK skip path above) — but $re_effort_tail
          # is safe to reuse as-is: it's assigned once, before that skip
          # branch splits, so it's set on both paths.
          g1_re_cursor='^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$'
          g1_base="" g1_params=""
          if [[ $model =~ $g1_re_cursor ]]; then
            g1_base="${BASH_REMATCH[1]}"
            g1_params="${BASH_REMATCH[2]}"
          fi
          g1_is_composer=0
          [[ $g1_base =~ ^composer-2\.5(-fast)?$ ]] && g1_is_composer=1
          g1_is_alt_effort=0
          if [[ $g1_base =~ ^(claude|gpt)- ]] && { [[ $g1_base =~ $re_effort_tail ]] || [[ $g1_params =~ (\[|,)effort= ]]; }; then
            g1_is_alt_effort=1
          fi
          case "$tier" in
          deep)
            tier_expected="kimi-k3-high, cursor-grok-4.6-medium-fast, cursor-grok-4.6-high, composer-2.5[-fast], or an effort-suffixed/bracketed claude-*/gpt-* id"
            [[ $model =~ ^(kimi-k3-high|cursor-grok-4\.6-medium-fast|cursor-grok-4\.6-high)$ ]] ||
              [ "$g1_is_composer" = 1 ] || [ "$g1_is_alt_effort" = 1 ] || tier_ok=0
            ;;
          standard)
            tier_expected="cursor-grok-4.6-medium-fast, cursor-grok-4.6-low-fast, or composer-2.5[-fast]"
            [[ $model =~ ^(cursor-grok-4\.6-medium-fast|cursor-grok-4\.6-low-fast)$ ]] ||
              [ "$g1_is_composer" = 1 ] || tier_ok=0
            ;;
          trivial)
            tier_expected="cursor-grok-4.6-low-fast or composer-2.5[-fast]"
            [[ $model =~ ^cursor-grok-4\.6-low-fast$ ]] || [ "$g1_is_composer" = 1 ] || tier_ok=0
            ;;
          esac
          ;;
        esac
        if [ "$tier_ok" = 0 ]; then
          echo "dispatch: model '$model' is not $tier's row for --agent $agent — expected $tier_expected, or pass --ignore-map (the human's model decision). See dispatch-orchestration.md \"Tier map\"." >&2
          exit 1
        fi
      fi
      ```

- [ ] **Verify the message string does not contain the literal substring
      `Model gate`** (grep the finished file for it — must appear 0 times
      outside the existing dispatchability-gate messages).

## Step 3: `dispatch.sh` — Gate 2 (budget-aware rung refusal)

File: `adapters/core/dispatch.sh`, inserted directly after the existing
`>=95%` exhaustion gate's closing `fi` (currently ends at line 318).

- [ ] Implement per SPEC.md §4:

  ```bash
  # Budget-aware rung refusal (issue #89): once codex/claude/cursor's 7d
  # burn crosses 70%, refuse the premium rung specifically and name the
  # standard-class alternative — before the engine goes fully dark at
  # 95% (the gate above). See dispatch-orchestration.md "Tier map".
  if [ -z "$ignore_budget" ] && [ -f "$budget_file" ]; then
    rung_downgrade=""
    case "$agent:$model" in
    claude:opus | claude:claude-opus-* | claude:fable | claude:claude-fable-*)
      rung_downgrade="sonnet"
      ;;
    codex:gpt-5.6-sol)
      rung_downgrade="gpt-5.6-terra"
      ;;
    cursor:cursor-grok-4.6-high)
      rung_downgrade="cursor-grok-4.6-medium-fast"
      ;;
    esac
    if [ -n "$rung_downgrade" ]; then
      rung_pct=$(jq -r --arg e "$agent" --argjson now "$(date +%s)" '
        if (.fetched_epoch + 7200) < $now then empty
        elif .engines[$e] == null then empty
        elif .engines[$e].windows["7d"] == null then empty
        elif .engines[$e].windows["7d"].used_pct >= 70 then .engines[$e].windows["7d"].used_pct
        else empty end' "$budget_file" 2>/dev/null || true)
      if [ -n "$rung_pct" ]; then
        echo "dispatch: $agent 7d is at ${rung_pct}% — the premium rung ($model) is refused; use the standard rung ($rung_downgrade) instead, or pass --ignore-budget (the human's spend decision). See dispatch-orchestration.md \"Tier map\"." >&2
        exit 1
      fi
    fi
  fi
  ```

- [ ] Confirm ordering by inspection: this block must physically follow the
      `>=95%` block so an exhausted engine reports that message first
      (§4's "gate ordering" note; pinned by tests in step 6).

## Step 4: `dispatch-orchestration.md` — new "Tier map" subsection + doc-text corrections

File: `adapters/core/protocols/dispatch-orchestration.md`

- [ ] Add a new `### Tier map` subsection immediately after the existing
      `### Model gate` subsection (after line 154), stating: - the rule in prose (SPEC.md §2's clauses 1-7), pointing back at the
      Model map table and Burn classes section for concrete ids — no
      fresh id table here (keeps the "Model map is the only place
      concrete worker model versions appear" invariant, line 37-39,
      intact). - the `premium → downgrade target` table from SPEC.md §4 (this is
      the one place gate 2's targets are named in the doc — it is not a
      _worker_-version table, it's a downgrade-pairing table, so it does
      not conflict with the line 37-39 invariant either). - the `--ignore-map` override: silent when set, "the human's model
      decision" framing, mirrors `--ignore-budget`. - the `≥70% on 7d, premium rung only` budget-rung mechanic, cross-
      referencing `DISPATCHER_PROTOCOL.md`'s budget-lever bullets (step 5).
- [ ] Amend the four passages SPEC.md §3 enumerates, in place: 1. Line 125 ("a model bump needs no `dispatch.sh` edit") — scope to
      the Model gate; add "the Tier map gate's table does need editing
      on a ladder bump — see 'Tier map' below." 2. Lines 144-146 ("enforces dispatchability, not tier-appropriateness")
      — split: Model gate = dispatchability; Tier map gate (new) =
      tier-appropriateness; the map is the source of truth for both. 3. Lines 148-154 (Override paragraph) — add: `DISPATCH_SKIP_MODEL_
CHECK` covers the Model gate only; a new model that's also a new
      tier's row additionally needs `--ignore-map` until the Tier map's
      table and `dispatch.sh` are updated. 4. Lines 93-98 (cursor alternatives prose) — tier-scope: `composer-2.5
[-fast]` stays open on every tier; the effort-suffixed/bracketed
      cross-vendor `claude-*`/`gpt-*` alternative is `deep`-only as of
      this gate.
- [ ] `DISPATCHER_PROTOCOL.md:330` — "Don't dispatch trivial work on opus …
      **The model is your call** and it's a real cost lever." (found by the
      plan-critic pass, same class of contradiction as the four amendments
      above, in a file step 5 already touches). Amend to: the model is the
      dispatcher's call _within the tier's row_ (or behind `--ignore-map`),
      not an unconstrained choice.

## Step 5: `DISPATCHER_PROTOCOL.md` — budget-lever bullet

File: `adapters/core/protocols/DISPATCHER_PROTOCOL.md`

- [ ] In the "Budget is the fifth lever" bullet list (~line 79), add a
      bullet: `≥70% on the 7d window` mechanically refuses the premium rung
      specifically (`dispatch` enforces this; see `dispatch-orchestration.md`
      "Tier map") — stated as narrower/complementary to the existing
      `≥85% on any window` bullet (which stays advisory-only, any-window),
      not a replacement for it.

## Step 6: `scripts/gen-adapters.sh` regeneration

- [ ] After steps 4-5's doc edits, run `bash scripts/gen-adapters.sh` and
      commit the regenerated `adapters/claude-code/plugin/protocols/*` and
      `adapters/codex/plugin/protocols/*` copies. Verify with a second run
      that `git diff` is empty (idempotence, per the repo's existing CI
      gate).

## Step 7: `tests/dispatch.bats` — existing-test edits

- [ ] `:559` ("DISPATCH_SKIP_MODEL_CHECK bypasses the gate for that exact
      model") — add `--ignore-map` to the `run_dispatch` invocation.
      Assertions unchanged.
- [ ] `:615` ("DISPATCH_SKIP_MODEL_CHECK lets its own model through to
      launch") — same: add `--ignore-map`. Assertions unchanged.
- [ ] `assert_gate_silent` (`:739-745`) — add `--ignore-map` to the
      `run_dispatch` invocation, and replace the vacuous helper with the
      non-vacuity check from SPEC.md's finalized §3:

  ```bash
  assert_gate_silent() { # <engine> <model>
    DISPATCH_PROFILE=work run run_dispatch standard "$2" --agent "$1" --effort medium --ignore-map --crew-id c1 42 "map row $2"
    if [[ "$output" == *"Model gate"* ]]; then
      printf 'gate rejected %s/%s: %s\n' "$1" "$2" "$output" >&2
      return 1
    fi
    # Non-vacuous: no stub_launch_bins here, so every row already dies
    # downstream at dispatch.sh's `gh repo view` resolution regardless of
    # gate 1/gate 2 — reaching that specific failure proves the run
    # cleared BOTH the dispatchability gate and the new tier gate.
    if [[ "$output" != *"could not resolve the default branch"* ]]; then
      printf 'gate stopped %s/%s before reaching gh repo view: %s\n' "$1" "$2" "$output" >&2
      return 1
    fi
  }
  ```

      No other line in the `@test "every model the docs name passes its

  engine's arm"` block changes.

## Step 8: `tests/dispatch.bats` — new tests, gate 1

- [ ] **Prerequisite (do this before any other bullet in this step):** add
      `codex_budget_json()` — a parallel helper alongside the existing
      claude-only `budget_json()` (`:322`), **not** a change to
      `budget_json()`'s signature (that helper has 5 existing call sites at
      `:330, 340, 349, 370` plus inline JSON at `:359-361`; changing its
      arity would touch all of them, which SPEC.md §6 rules out). Shape:

  ```bash
  # Write a budget cache with one codex 7d window at the given
  # utilization — the gate-1/gate-2 ordering test and step 10's gate 2
  # tests both need this.
  codex_budget_json() { # <pct> <epoch>
    mkdir -p "$XDG_DATA_HOME/crew"
    jq -n --argjson pct "$1" --argjson epoch "$2" \
      '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {"7d": {used_pct: $pct, resets_at: null}}}, cursor: null}}' \
      >"$XDG_DATA_HOME/crew/engine-budget.json"
  }
  ```

  This is defined once in step 8 (before it's first used by the
  ordering test below) and reused by step 10, not duplicated.

- [ ] **Accept-row conventions (apply to every accept-path bullet below):**
      `stub_launch_bins` may be called only **once** per test (it runs `git
remote add origin`, which fails on a second call within the same
      test) — use one `@test` per accept row, or a loop with a fresh `@test`
      per row rather than multiple accept assertions sharing one
      `stub_launch_bins` call; codex/cursor rows need
      `DISPATCH_PROFILE=work` (profile gate, `dispatch.sh:205-212`); titles
      must be distinct per row within the whole file (`:550-551`'s existing
      rationale: the title becomes the branch, a reused one collides).
- [ ] New `@test` block(s) per SPEC.md §6, covering: - Each table cell in SPEC.md §2 accepted directly (loop or explicit
      cases per engine — mirror the existing per-engine accept tests'
      style at `:549`/`:594`/`:720` rather than inventing a new pattern). - claude `standard opus` rejected, naming `standard`/`opus`/expected
      `sonnet or claude-sonnet-*`/`--ignore-map`. - claude `deep`/`standard` both accept `sonnet` (closes the gate-2
      coupling — SPEC.md §4). - A version-pinned full id accepted: `deep claude-opus-5-1
--agent claude`, `standard claude-sonnet-4-5 --agent claude`. - codex `standard` accepts `gpt-5.6-luna` (downward walk); `deep`
      accepts `gpt-5.6-terra`. - codex legacy generations (`gpt-5.5`, `gpt-5.4`, `gpt-5.4-mini`)
      accepted on all three tiers, not just `standard` (existing test
      `:594` only covers `standard`). - cursor `deep` accepts `cursor-grok-4.6-high` (closes the gate-2
      coupling for cursor). - cursor **rejects** `claude-opus-5-high --agent cursor` and
      `gpt-5.6-sol-high --agent cursor` on `standard` and `trivial`
      (the premium-leak-via-cursor case this design fixes) while still
      accepting them on `deep` (pins `:730`'s continued behavior plus the
      new confinement). - `--ignore-map` bypasses an otherwise-rejected pair, silently (no
      stderr notice — assert the specific string `Tier map` / rejection
      text is absent from output, mirroring the `--ignore-budget` silent-
      bypass test's shape at `:368`). - Ordering: gate 1 rejects before gate 2 is reached for a model that
      is both off-row and premium — `codex_budget_json 80
"$(date +%s)"` then `standard gpt-5.6-sol --agent codex` still
      reports the _tier_ message (`standard`, `--ignore-map`), not the
      rung message — mirrors the "model gate outranks the ultra gate"
      precedent test shape at `:540`. - An off-map `--review --pr N` dispatch is rejected by gate 1. Adapt
      the fixture shape from `tests/dispatch.bats:905-909` (**not** `:876`
      — that test has no PR/protocol-contract stub and dies earlier, at
      the missing-`REVIEW_TASK.md` check, `dispatch.sh:180-189`, which
      runs before gate 1 and would leave this criterion unpinned):
      `stub_pr_bins pr-head-review-offmap`; `export
DISPATCHER_PROTOCOL_DIR="$BATS_TEST_DIRNAME/../adapters/core/protocols"`;
      `run run_dispatch standard opus --agent claude --effort medium --pr 99
--review --crew-id c1 "review off map"`; assert `$status -eq 1`, the
      tier-message substrings (`standard`, `opus`, `--ignore-map`), and
      that `$STUB_LOG` has no `switch`.

## Step 9: `tests/dispatch.bats` — conformance test (sync tripwire)

New `@test "tier map conformance: dispatch.sh matches the documented rule"`.

The token set to check is the **concrete, non-glob** ids only — the glob
families (`claude-opus-*`, `claude-sonnet-*`, `claude-haiku-*`,
`claude-fable-*`) have no literal doc occurrence beyond `claude-fable-5-1`
and would make a literal-substring grep fail on day one if included as
globs:

```
opus sonnet haiku fable
gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini
kimi-k3-high cursor-grok-4.6-high cursor-grok-4.6-medium-fast cursor-grok-4.6-low-fast
composer-2.5 claude-fable-5-1
```

For each token: `grep -qF "$token" "$DISPATCHER_PROTOCOL_MD_PATH"` against
`adapters/core/protocols/dispatch-orchestration.md`'s **existing** Model map
/ Burn classes text (no new doc table — per SPEC.md §3's no-doc-duplication
decision), **and** `grep -qF "$token" "$DISPATCH"` against `dispatch.sh`
itself. Note in a comment that the `dispatch.sh` side matches via the
human-readable `tier_expected` strings (step 2's plan), not the bash regexes
(which carry escaped `\.` and would not literal-match `gpt-5.6-sol`) — so a
future edit that removes the `tier_expected` strings without keeping some
literal occurrence of each id silently guts this tripwire; call that out so
it isn't "fixed" by someone who doesn't know why the strings exist. Same
"drift loud, not impossible" tolerance as the precedent test at `:747`.

## Step 10: `tests/dispatch.bats` — new tests, gate 2 (budget rung)

- [ ] Reuses `codex_budget_json()` from step 8 (defined once there; do not
      redefine it here). `$XDG_DATA_HOME` is already isolated per-test by
      `tests/helpers.bash`'s `assert_isolated_xdg_data_home`, inherited from
      `setup()` — no new isolation work needed.
- [ ] `@test`s per SPEC.md §6, each fixture bound explicitly so the
      pre-existing `>=95%` exhaustion gate (`dispatch.sh:307-317`, which
      scans **every** window and fires independently of gate 2) cannot
      silently make a test pass for the wrong reason: - `codex_budget_json 70 "$(date +%s)"` → `deep gpt-5.6-sol --agent
codex` refused, message names `gpt-5.6-terra`. - `codex_budget_json 84 "$(date +%s)"` → same refusal (no
      85%-specific branch exists; 84 is deliberately `<95` so the
      exhaustion gate stays quiet and this is unambiguously gate 2). - `codex_budget_json 95 "$(date +%s)"` → the **existing**
      exhaustion-gate message fires ("quota exhausted"), not the rung
      message (ordering, §4). - A cache with `5h`=90, `7d`=30 (both `<95`, so the exhaustion gate
      stays quiet too) → gate 2 does not fire; `deep opus --agent claude`
      (or the codex equivalent) reaches `send-keys` — `stub_launch_bins` + `[ "$status" -eq 0 ]` (window-scope pin, the finding revision 1
      missed). - A codex cache with `windows: {"other": {used_pct: 80, resets_at:
null}}` (no `7d` key, simulating a drifted `wname()` bucket,
      `used_pct` **bounded at 80** — deliberately `<95` so the
      exhaustion gate cannot fire either and the test is unambiguous) →
      gate 2 does not fire: `stub_launch_bins` +
      `deep gpt-5.6-sol --agent codex` → `[ "$status" -eq 0 ]`,
      `grep -q 'send-keys' "$STUB_LOG"`, and `$output` contains neither
      `quota exhausted` nor the rung-refusal text (documented no-op, §4). - `--ignore-budget` bypasses gate 2 (mirrors the existing
      `--ignore-budget` bypass test's shape at `:368`). - A non-premium model (`deep gpt-5.6-terra --agent codex`) at
      codex `7d`=90 is **not** refused by gate 2 (only premium rungs are
      checked; the exhaustion gate at 90% also does not fire since it's
      `<95`). - claude `7d`=75 (via a parallel `claude_budget_json`-shaped call, or
      reuse the existing `budget_json()` at `:322` which is already
      claude-only and takes exactly `<pct> <epoch>`) → `deep opus --agent
claude` refused, names `sonnet` (confirms the deliberate
      all-three-engines scope decision from SPEC.md §4).

## Step 11: Repo-wide gates

- [ ] `shellcheck adapters/core/*.sh scripts/*.sh` — clean.
- [ ] `bats tests/` — full suite green (not just `dispatch.bats`; the
      protocol doc edits touch files `tests/adapters.bats` may also assert
      about).
- [ ] `nix flake check` — green.
- [ ] `bash scripts/gen-adapters.sh` idempotent (step 6, re-verify after
      all doc/test edits land, not just once mid-way).

## Step 12: PR body

- [ ] `## Plan` heading noting this ran the full deep-tier spec+plan
      pipeline (not `plan: provided`).
- [ ] Note the deferred item 3 (escalation-not-default `gpt-5.6-sol`) and
      link the follow-up issue (create it: "defer making gpt-5.6-sol an
      escalation rung until crew rate has enough outcome data — see PR #92
      and issue #89's SPEC.md/PLAN.md for the reasoning").
- [ ] Note the spec-critic process: 2 full revision cycles, both accepted
      as `revise` with concrete findings applied each time; the final
      (3rd) critique's two blocking findings (a broken test-assertion
      prescription; four doc passages needing amendment) were narrow and
      mechanical — the critic explicitly validated the core §2 design as
      sound — and were folded into SPEC.md directly as finalization rather
      than spawning a 4th async critique cycle beyond the stated 2-revision
      cap. Say this plainly rather than silently exceeding/reinterpreting
      the cap.
- [ ] Note the "gate 2 now applies to all three engines, not codex alone"
      scope decision (SPEC.md §4) and its rationale (claude is already at
      61% 7d in the live evidence; the requirement's own "walk down burn
      before tier" logic is engine-neutral).
