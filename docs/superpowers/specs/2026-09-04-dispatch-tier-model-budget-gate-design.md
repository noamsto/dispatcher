# SPEC — dispatch: tier↔model gate and budget-aware rung selection

Issue #89. Branch `feat/89-dispatch-tier-model-gate-and-budget-awar`, base `extract`.

**Revision 2** (final — revision cap) — fixes revision 1's findings: §2/§4
contradicted each other on claude (gate 2's own advice was undispatchable),
the per-engine asymmetry was more complex than needed and still under-
generalized (killed legitimate downward walks at `standard`), cursor's open
alternatives let premium ids leak onto `trivial`, `tests/dispatch.bats:747`
would silently go vacuous, and full `claude-*` id families other than fable
were unreachable. Replaced the asymmetric per-engine design with **one
uniform rule**, verified row-by-row against all nine `(tier, engine)` cells
plus cursor's alternatives.

## 1. Problem

(unchanged) `dispatch` validates `<model>` against `--agent` by shape only,
never tier appropriateness. 24 `gpt-5.6-sol` dispatches logged, 10 on
`standard` tier where the ladder says `gpt-5.6-terra`; codex has since
crossed its 7d 95% hard-refusal threshold. Two mechanisms: a tier↔model gate
(reject an off-row model before scaffolding, explicit override), and a
budget-aware rung refusal (refuse the premium rung specifically once burn
crosses 70%, before the engine goes fully dark at 95%).

Deferred, unchanged: making `gpt-5.6-sol` an escalation rather than the
default `deep` rung — no outcome data yet; PR #92 wired `crew rate`, revisit
once the ratings store has runs. Follow-up issue points at this spec.

## 2. The uniform rule

For `(tier, engine)`, `dispatch` accepts a model iff it is:

1. the tier's **worker** or **execute** cell (Model map, `worker → execute →
escalate`) — execute is always same-or-cheaper burn class than worker in
   every row in the map (verified below), so including it can never leak a
   stronger model onto a cheaper tier; **or**
2. the tier's **escalate** cell, **only** when that cell's burn class is not
   _stronger_ than the worker cell's (i.e. escalate is a same-class or
   cheaper alternative, not a genuine escalation) — this is what excludes
   claude `standard`'s escalate (`opus`, stronger than `standard`'s worker
   `sonnet`) while still admitting every other row's escalate, which never
   strengthens beyond worker; **or**
3. for claude specifically, a full `claude-<alias>-*` id for any alias
   already accepted under (1)/(2) at that tier (`claude-opus-*`,
   `claude-sonnet-*`, `claude-haiku-*`, `claude-fable-*`) — version-pinning
   is a normal operation the shape gate already treats as first-class
   (`claude-*` ids, `dispatch.sh:232`), so the tier gate must not make it
   `--ignore-map`-only; **or**
4. for claude `deep` specifically, `fable`/`claude-fable-*` — the Model
   map's own deep-cell prose ("escalate worker to `claude-fable-5-1`") names
   this as a worker-level escalation, i.e. a legitimate `dispatch` argument,
   pinned by the already-green `tests/dispatch.bats:553`; **or**
5. for codex, on **every** tier, a legacy bare generation (`gpt-5.5`,
   `gpt-5.4`, `gpt-5.4-mini`) — these predate the tier ladder and are not
   tier-scoped anywhere else (`dispatch-orchestration.md:62-67`); pinned by
   `tests/dispatch.bats:594`; **or**
6. for cursor, on **every** tier, `composer-2.5`/`composer-2.5-fast` — no
   effort variant exists, so it is unconditionally the cheapest cursor
   option and can never be a premium leak; **or**
7. for cursor, on **`deep` only**, an effort-suffixed/bracketed cross-vendor
   `claude-*`/`gpt-*` id (the shape the existing dispatchability gate's
   cursor arm already recognizes) — confined to `deep` so
   `claude-opus-5-high`/`gpt-5.6-sol-high` cannot land on `trivial`/
   `standard` (revision 1's bug: it unioned this on _every_ tier, which is
   exactly the failure mode issue #89 reports, just via cursor). The two
   existing tests this must keep green both already use `deep`
   (`tests/dispatch.bats:730`) or `composer-2.5` at `standard`
   (`:486`, `:722`, covered by rule 6, not this rule) — no currently-green
   test needs the effort-suffixed form below `deep`.

**Resulting tables**, rule numbers noted per addition:

| tier     | claude                                                                                                          | codex                                             | cursor                                                                                                                                                     |
| -------- | --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| deep     | `opus`, `claude-opus-*`, `sonnet`, `claude-sonnet-*` (1/2/3) + `fable`, `claude-fable-*` (4)                    | `gpt-5.6-sol`, `gpt-5.6-terra` (1/2) + legacy (5) | `kimi-k3-high`, `cursor-grok-4.6-medium-fast`, `cursor-grok-4.6-high` (1/2) + `composer-2.5[-fast]` (6) + effort-suffixed/bracketed `claude-*`/`gpt-*` (7) |
| standard | `sonnet`, `claude-sonnet-*` (1)                                                                                 | `gpt-5.6-terra`, `gpt-5.6-luna` (1) + legacy (5)  | `cursor-grok-4.6-medium-fast`, `cursor-grok-4.6-low-fast` (1) + `composer-2.5[-fast]` (6)                                                                  |
| trivial  | `sonnet`, `claude-sonnet-*`, `haiku`, `claude-haiku-*` (documented multi-choice, treated as two worker options) | `gpt-5.6-luna` (1) + legacy (5)                   | `cursor-grok-4.6-low-fast` (1) + `composer-2.5[-fast]` (6)                                                                                                 |

**Row-by-row verification that rule 2 never strengthens below `deep`**
(the property the whole design leans on): claude `deep` escalate=`opus`,
same class as worker `opus` → included (redundant, already there via rule
1). claude `standard` escalate=`opus`, **stronger** than worker `sonnet` →
**excluded** by rule 2 — this is the one row where the exclusion bites, and
it is exactly the row that would otherwise leak `opus` onto `standard`.
claude `trivial`: no escalate (no delegation). codex `deep` escalate=`sol`,
same as worker → included, redundant. codex `standard` escalate=`terra`,
same as worker → included, redundant (execute cell `luna` already adds the
cheap rung via rule 1). codex `trivial`: no escalate. cursor `deep`
escalate=`cursor-grok-4.6-high` — `kimi-k3-high` (worker) is not classified
in the Burn classes table, so rule 2's "not stronger than worker" test does
not apply mechanically here; included by direct inspection instead (cursor's
`deep` row is the _only_ row in the entire map whose escalate cell is
premium-class, and it is already `deep`-scoped by construction — there is no
tier below `deep` whose escalate could inherit it). cursor `standard`
escalate=`cursor-grok-4.6-medium-fast`, same as worker → included, redundant
(execute `cursor-grok-4.6-low-fast` already adds the cheap rung via rule 1).
cursor `trivial`: no escalate.

**No remaining asymmetry to justify.** Revision 1 shipped a hand-picked,
per-engine set of allowances with a "cursor is just different" rationale
that revision 1's own critique showed was under- _and_ over-inclusive at the
same time (missing `standard`-tier downward walks for claude/codex, leaking
premium onto cursor `trivial`). The rule above is the same for all three
engines; only the _inputs_ (which cells are premium, which rows have no
delegation) differ, which is data, not a different rule.

## 3. Gate 1 — tier↔model

**Placement.** Unchanged from revision 1: immediately after the existing
dispatchability gate's block (`dispatch.sh`, shape-check `case` closes at
line 289), before the `--effort ultra` check at line 292.

**`DISPATCH_SKIP_MODEL_CHECK` does not bypass this gate** (unchanged
reasoning — that variable is about shape/cache staleness, not tier
appropriateness; conflating them would let one flag silently clear both
concerns). **Test impact, `tests/dispatch.bats`:**

- `:559` and `:615` — both dispatch a shape-invalid-for-its-engine model
  (`kimi-k3-high` under `--agent claude`; bare `gpt-5.6` under `--agent
codex`) specifically to test the skip var. Neither is in §2's table at any
  tier for that engine. Both gain `--ignore-map`; assertions unchanged.
- `:553`, `:594`, `:722`, `:486`, `:730` — need **no** change; covered
  directly by §2 rules 3/4/5/6/7.
- **`:739-762` (`assert_gate_silent` / "every model the docs name passes its
  engine's arm").** This helper dispatches every doc-named model at a fixed
  `standard` tier and asserts only that `$output` excludes the literal
  string `Model gate` — it does not check `$status`. Left as-is, most of its
  rows (e.g. `opus`, `gpt-5.6-sol`, `kimi-k3-high` at `standard`) would now
  exit non-zero via gate 1 while the helper stays green, silently degrading
  the repo's only dispatchability-conformance guard to a no-op for those
  rows. **Fix in scope:** `assert_gate_silent` gains `--ignore-map` (its
  purpose is strictly the shape/dispatchability arm, never tier — this keeps
  it scoped to that). **Not** a `[ "$status" -eq 0 ]` assertion — this
  helper never calls `stub_launch_bins` (it can't: that helper does a
  one-time `git init --bare origin.git` + `remote add origin`, unsuitable
  per-row), so every row already dies downstream at `dispatch.sh:488-492`
  ("could not resolve the default branch via `gh repo view`" — the generic
  no-op `gh` stub from `setup()` prints nothing) regardless of gate 1/gate 2.
  Instead, add a **positive** non-vacuity check: `[[ "$output" ==
*"could not resolve the default branch"* ]]` — reaching that specific
  downstream failure proves the run cleared _both_ the dispatchability gate
  and the new tier gate, which is the property this helper needs and can
  prove without a full launch.

**Logic.** Table from §2, expressed as bash pattern matches keyed on
`$agent`/`$tier`/`$model`. Self-contained — does not reuse `$cursor_base`/
`$cursor_params` from the existing dispatchability gate's cursor arm, since
those are unset on the `DISPATCH_SKIP_MODEL_CHECK` skip path and gate 1 must
still evaluate correctly there.

Reject with a message naming the tier, the model given, the row's expected
model(s) (rendered from the same table), and `--ignore-map`. New boolean CLI
flag, parsed next to `--ignore-budget` in the existing arg loop, and
**silent when set** — mirroring `--ignore-budget` literally (that flag
prints nothing when it takes effect; only `DISPATCH_SKIP_MODEL_CHECK`, a
different mechanism, prints a notice), per WORKER_TASK.md's explicit
instruction to mirror `--ignore-budget`'s existing ergonomics rather than
invent new ones. Framing mirrors `DISPATCHER_PROTOCOL.md`'s "the human's
spend decision" language: using `--ignore-map` is **the human's model
decision** (stated in `dispatch-orchestration.md`'s new subsection, not as
a runtime stderr message).

**Doc anchor and no doc-duplication.** New `dispatch-orchestration.md`
subsection "Tier map", placed after "Model gate". It states **the rule**
(§2's five/seven numbered clauses, in prose) and points back at the existing
Model map table and Burn classes section for concrete ids — it does **not**
restate a fresh table of ids, respecting the doc's own existing invariant
("Model map … is the only place concrete worker model versions appear",
`dispatch-orchestration.md:37-39`) rather than amending it. This also means
the conformance test's sync target stays the _existing_ Model map/Burn
classes text, not a second doc table that could itself drift from the
first.

The rejection message must not contain the literal string `Model gate`
(reserved for the existing dispatchability gate, asserted-absent by
`assert_gate_silent` — now correctly out of scope for gate 1 per the fix
above, but the string is still reserved to keep the two gates' signatures
distinguishable in output).

**`--review`/`--pr` dispatches are not exempt** — structurally true (both
gates sit above every `$pr_number`/`$kind` branch); §6 asserts it.

**Sync with the doc.** Hand-copy §2's table into a bats conformance test,
**plus** `grep -qF` assertions that each literal cell token still appears in
the _existing_ Model map / Burn classes text (no new doc table to drift
against, per above). Precedent: `tests/dispatch.bats:747`'s own comment,
"Copied, so it makes drift loud rather than impossible."

**Operator-facing surfaces:** `usage()` at `dispatch.sh:9` gains
`[--ignore-map]` alongside `[--ignore-budget]`.

**Existing prose that becomes false and must be amended in the same PR**
(found by the third critic pass — these sit inside/near the section this
spec's own "Tier map" subsection is placed under, so leaving them
contradicts the new gate rather than merely omitting it):

1. `dispatch-orchestration.md:125` — "It checks per-engine id _shape_, not
   membership of the table above, **so a model bump needs no `dispatch.sh`
   edit**." True for the _Model gate_ (dispatchability, unchanged); false
   for the new Tier map gate, whose hand-copied table (§3, sync mechanism)
   _does_ need a `dispatch.sh` edit on a ladder bump. Amend to scope the
   claim to the Model gate explicitly and cross-reference the Tier map
   subsection for the contrast.
2. `dispatch-orchestration.md:144-146` — "The gate enforces
   **dispatchability, not tier-appropriateness**. Picking the rung that fits
   the task stays the dispatcher's judgment, and the map above stays the
   source of truth for it." Amend to: the _Model gate_ enforces
   dispatchability; the new _Tier map_ gate enforces tier-appropriateness
   (cross-reference); the map stays the source of truth for _both_, per §3's
   sync mechanism.
3. `dispatch-orchestration.md:148-154` (Override) — currently implies
   `DISPATCH_SKIP_MODEL_CHECK` alone recovers from a stale map. Add a
   sentence: the skip var covers the Model gate (shape) only; a genuinely
   new model that is also a new tier's row additionally needs `--ignore-map`
   until the map (§3's hand-copied table) and `dispatch.sh` are updated,
   consistent with §3's decision that the two overrides are not aliases of
   each other.
4. `dispatch-orchestration.md:93-98` (cursor alternatives prose) — "so a
   cursor worker can still front `composer-2.5`/`composer-2.5-fast` … as an
   alternative, or an effort-suffixed `claude-opus-5-*`/`gpt-5.6-sol-*`."
   Amend to state the tier scope from §2 rule 7: `composer-2.5[-fast]` stays
   open on every tier; the effort-suffixed/bracketed cross-vendor form is
   `deep`-only as of this gate.

None of these four edits introduces a concrete model id into new prose, so
they do not conflict with the "Model map … is the only place concrete
worker model versions appear" invariant at `dispatch-orchestration.md:37-39`
— they narrow/qualify existing claims about _behavior_, which is exactly
what changed.

## 4. Gate 2 — budget-aware rung refusal

**Placement.** Unchanged: directly after the existing `>=95%`
engine-exhaustion gate (~301-318), reusing `$budget_file`/`$ignore_budget`.

**Window scope.** The `7d` window specifically, matching the requirement's
literal wording and avoiding a `5h`-burst false trigger (unchanged from
revision 1). **Caveat, now stated explicitly rather than assumed:**
`refresh-budget.sh`'s codex probe (`probe_codex`, via `wname()`) can key a
window `unknown` or `other` if the account's `windowDurationMins` is null or
drifts outside the 5h/1d/7d bucket boundaries. When the cache has no `7d`
key for the engine, gate 2 is a no-op for it (same fail-open posture as the
exhaustion gate for a missing engine) — this is a known, accepted blind spot
given the literal evidence in `WORKER_TASK.md` keys the live cache `7d`; §6
adds a fixture case for a drifted key to pin the no-op as intentional, not
an untested surprise.

**Logic.** `premium → downgrade target` per engine, from Burn classes
(`dispatch-orchestration.md:46-54`), extended to `fable`/`claude-opus-*`/
`claude-fable-*` (matching §2's full-id-family treatment):

| engine | premium                                            | downgrade target              |
| ------ | -------------------------------------------------- | ----------------------------- |
| claude | `opus`, `claude-opus-*`, `fable`, `claude-fable-*` | `sonnet`                      |
| codex  | `gpt-5.6-sol`                                      | `gpt-5.6-terra`               |
| cursor | `cursor-grok-4.6-high`                             | `cursor-grok-4.6-medium-fast` |

**Stated decision, not an accident:** WORKER_TASK.md's item 2 evidence and
literal wording name codex specifically ("at codex 7d ≥70%"). This spec
applies the same mechanism uniformly to all three engines the burn-classes
table already covers, since the requirement's own rationale ("walk down
burn before you walk down tier") is engine-neutral and claude is already at
61% 7d in the live evidence — narrowing to codex-only would leave the same
failure mode latent on claude. The cursor row is inert in practice today
(`refresh-budget.sh` hardcodes `cursor: null`, unobservable quota per
`DISPATCHER_PROTOCOL.md:91`) and is exercised only by a synthetic fixture in
§6 — included for symmetry and because the table becomes correct
automatically if cursor quota ever becomes observable, not because it does
anything today.

Fires when **both**: `$model` matches a `premium` entry for `$agent`, and
`engines.$agent.windows["7d"].used_pct >= 70` in a fresh cache (2h staleness,
same as the exhaustion gate).

**No tier check needed, and this now genuinely holds.** §2's rule 2
confines every premium cell's reach to `deep` (claude `opus`/`fable`, codex
`gpt-5.6-sol`, cursor `cursor-grok-4.6-high` via the deep-only escalate
inclusion) — verified per-row in §2, not merely asserted.

**No coupling gap.** §2 rule 1 already grants `deep` the downgrade target
directly: claude `deep` includes `sonnet` (execute cell, rule 1); codex
`deep` includes `gpt-5.6-terra` (execute cell, rule 1); cursor `deep`
includes `cursor-grok-4.6-medium-fast` (execute cell, rule 1). Gate 2's
suggestion is always immediately dispatchable at the same tier — this is now
verified against §2's actual final table (revision 1 asserted it without
the claude row supporting it; it does now).

**Gate ordering when a model is both off-row and premium.** Gate 1 runs
first (placement, §3): `dispatch standard gpt-5.6-sol --agent codex` at any
budget level is rejected by gate 1 (`gpt-5.6-sol` not in `standard`'s table)
before gate 2 is ever reached. §6 pins this.

**Message.** Names the engine, `7d` `used_pct`, the model given, the
downgrade target, and `--ignore-budget` ("the human's spend decision").
Cites the "Tier map" doc anchor, which also carries the downgrade-target
table (prose, pointing at Burn classes for the underlying premium/standard
classification — same no-duplication rule as §3).

**Operator-facing surfaces:** `DISPATCHER_PROTOCOL.md`'s existing "Budget is
the fifth lever" bullet list gains a `≥70% on `7d`, premium rung only`
bullet, explicitly scoped as narrower/mechanical versus the existing
`≥85% on any window` bullet, which stays advisory-only and any-window —
stated so the two bullets read as complementary, not contradictory.

## 5. Non-goals

- Item 3 (issue's numbering) — `gpt-5.6-sol` as escalation, not default
  `deep` rung. Deferred; follow-up issue opened pointing at this spec and
  PR #92.
- No change to the existing dispatchability gate's logic — only the two
  `--ignore-map` test additions and `assert_gate_silent`'s fix, per §3.
- No change to `refresh-budget.sh` or the budget cache schema (the codex
  window-key caveat in §4 is documented, not fixed — out of scope).
- No new CLI flag beyond `--ignore-map`; gate 2 reuses `--ignore-budget`.
- No runtime doc parsing; sync is hand-copy + literal-substring tripwire
  against the _existing_ Model map/Burn classes text only — no second doc
  table introduced to drift against the first.
- Claude's `5h`/`7d_opus`/`7d_sonnet` sub-windows are not consulted by gate
  2 — only blended `7d`. No evidence motivates finer-grained scoping yet.
- `adapters/core/dispatcher.sh`'s own `--model` slot (orchestrator session)
  stays out of scope, per the original model-gate spec's carve-out.

## 6. Acceptance criteria (elaborated into bats assertions during planning)

- Gate 1: every cell in §2's table passes for its `(tier, engine)`; an
  off-table model is rejected non-zero naming tier/model/expected row/
  override; `--ignore-map` lets it through silently (no stderr notice,
  mirroring `--ignore-budget`).
- Claude accepts a version-pinned full id (`claude-opus-5-1` on `deep`,
  `claude-sonnet-4-5` on `standard`) per rule 3, not just the alias.
- Claude `standard` rejects `opus` (rule 2's exclusion — the row this whole
  design exists to get right); claude `deep`/`standard` both accept
  `sonnet` (rule 1, closes the gate-2 coupling gap).
- Codex accepts legacy generations on every tier; codex `deep` accepts
  `gpt-5.6-terra` and `standard` accepts `gpt-5.6-luna` (rule 1 downward
  walks).
- Cursor accepts its tier's worker/execute/escalate triple and
  `composer-2.5[-fast]` on every tier; effort-suffixed/bracketed `claude-*`/
  `gpt-*` ids accepted on `deep`, **rejected** on `standard`/`trivial`
  (rule 7's new confinement — the fix for revision 1's premium-leak-via-
  cursor bug).
- The two `DISPATCH_SKIP_MODEL_CHECK` tests pass with `--ignore-map` added;
  `assert_gate_silent` gains `--ignore-map` and the "could not resolve the
  default branch" non-vacuity check (§3); no other existing test changes.
- An off-map `--review --pr N` dispatch is rejected by gate 1, same message
  shape.
- A conformance test fails if `dispatch.sh`'s table and the _existing_
  Model map/Burn classes text disagree (hand-copy + literal-substring
  tripwire, no new doc table).
- Gate 2, fixture budget cache, isolated `XDG_DATA_HOME`: at codex `7d` 70%
  a `deep gpt-5.6-sol` dispatch is refused naming `gpt-5.6-terra`; at 84%
  same refusal; at 95% the pre-existing exhaustion gate fires first
  (ordering — same test also covers "off-row AND premium" from §4); a
  `5h`-only spike with `7d` low does not trigger gate 2; a cache with no
  `7d` key for the engine (drifted `wname()` bucket) does not trigger gate 2
  (documented no-op, not a surprise).
- `bats tests/` green, `shellcheck adapters/core/*.sh scripts/*.sh` clean,
  `nix flake check` green, `scripts/gen-adapters.sh` idempotent.
