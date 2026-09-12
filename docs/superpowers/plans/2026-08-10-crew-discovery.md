# Plan — crew discovery (#29)

**Spec:** `docs/superpowers/specs/2026-08-10-crew-discovery-design.md`. The spec is
authoritative for every message string, guard, and rationale; this plan sequences the
edits and pins the verification after each one.

**Orchestration consult:** not run. The survey did not trip — the change is one shell
module (`adapters/core/crew.sh`) plus two single-line call-site edits, a doc, and a test
file; the one shared interface (`crew id`) is enumerated exhaustively in spec §4 and was
independently re-verified by two critic passes. There is no cross-module decomposition
for a consultant to contribute.

**Sequencing constraint:** steps 1-3 all edit `adapters/core/crew.sh` and must run
**serially** — never as concurrent subagents against the same file. Steps 4-5 and 6 are
independent of each other; step 7 depends on 1-6 being in place to run green.

---

- [ ] **Step 1: `crew new`, and `crew id` stops minting** — `adapters/core/crew.sh`

  In the pre-repo block (currently lines 204-224, above the `--git-common-dir` guard at
  228-232, because `dispatcher.sh` mints before its own `git rev-parse` guard):
  - Replace the `id` arm's `printf '%s\n' "${CREW_ID:-$(date +%s)-$$}"` with
    `id=$(_crew_id)`; print it via `printf '%s\n' "$id"` when non-empty, else die with
    spec §7's bare-`crew id` message and exit 1. Capture-then-print normalises the
    trailing newline `_crew_id` is inconsistent about (spec §3).
  - Add a `new` arm beside it: `printf '%s\n' "$(date +%s)-$$"` — the old bare-`id`
    behaviour verbatim, and a pure mint that does **not** register.
  - `_crew_id` itself is unchanged.
  - Add `new`, `crews` and `adopt [--force] <id> [pid]` to the usage string (line 1475).

  `$PPID` must reach the terminal **unexpanded** (spec §7) — a plain double-quoted
  `echo` prints the throwaway shell's pid instead. But the message body already contains
  `'crew crews'` / `'crew adopt <id> $PPID'` / `'crew new'` in single quotes, so the
  outer string cannot itself be single-quoted without `'"'"'` gymnastics: use spec §7's
  other option and write `\$PPID` inside the double-quoted string. (Single-quoting is
  the right form only for `dispatch.sh:173`, which is already single-quoted — step 4.)

  Verify: `bash -n`, `shellcheck adapters/core/crew.sh`, and by hand —
  `CREW_ID=x crew id` prints `x`; `CREW_ID= crew id` exits 1 naming all three commands;
  two consecutive `CREW_ID= crew new` differ.

- [ ] **Step 2: `crew crews`** — `adapters/core/crew.sh`, below the repo guard

  TSV with header (`crew_id last_event_s first_event_s workers pid alive`), per spec §5:
  union `crews/*/` dir names with distinct `.crew_id` from `events.jsonl`, dedup, sort
  newest-activity first with no-event crews last.

  **Four** guards, each of which is a `set -euo pipefail` death or a wrong row if missed:
  - filter `select(.crew_id != null and .crew_id != "")` — `reap` (crew.sh:1447-1449)
    and `release` (1327-1330) emit crew-less lines and would otherwise show as a phantom
    `null` crew;
  - a `crews/<id>/` dir need not contain `pid` (`watch` does `mkdir -p` at 502-503), so
    read it as `cat … 2>/dev/null || true` and render `—`;
  - the crew dir and `events.jsonl` may each be absent independently — read each source
    only when its file exists;
  - **`crews/` may exist and be empty — and that is the _normal_ state of a repo whose
    dispatcher has exited.** `dispatcher.sh:144-146` runs `crew deregister` on every
    launcher exit and `deregister` is `rm -rf "$cdir"` (crew.sh:418), which removes
    `crews/<id>/` but leaves `crews/`. With nullglob off, `for d in "$dir"/crews/*/`
    then yields the unexpanded literal and invents a crew named `*`. Enumerate with a
    per-entry check — `for d in "$dir"/crews/*/; do [ -d "$d" ] || continue` — because
    this phantom would land in the primary deliverable in exactly the post-restart
    scenario the feature exists for, _and_ would make step 5's notice fire on every
    ordinary relaunch.

  `alive` is `kill -0`; `—` when there is no pid.

  Verify: `shellcheck`; by hand in a scratch repo with an events-only crew, a
  dir-only crew, a crew in both, and a register-then-deregister repo (empty `crews/`).

- [ ] **Step 3: `crew adopt`, and the `register` refusal message** — `adapters/core/crew.sh`

  `crew adopt [--force] <id> [pid]` per spec §6: strip `--force` from anywhere in the
  args _before_ reading positionals, then id `=$1`, pid `=${2:-$PPID}` — **not**
  `${1:-$PPID}`, which is `register`'s correct form and would write the crew id into the
  pid file. Registers `crews/<id>/pid` and prints `export CREW_ID=<id>` on stdout.

  Two refusals, both exactly as spec §6 words them: unknown id (no dir **and** no event),
  and live registered pid — the latter **must not print the pid**, and there is **no**
  same-pid idempotency carve-out; `--force` is the only override.

  Then update only the `register | deregister` arm's refusal (line 410) to spec §7's
  text. Leave the identical strings in the `status | msg`, `reply`, and `watch` arms
  **untouched** — `crew msg` is fenced to live worker #46.

  Verify: `shellcheck`; by hand — unknown id refused, live-pid crew refused without
  leaking the pid, `--force` overrides, `adopt <id> --force` does not write `--force` as
  the pid, events-only crew adoptable.

- [ ] **Step 4: the `dispatch` abort message** — `adapters/core/dispatch.sh:171-175`

  Replace the `echo` with spec §7's text. **Keep** the `# shellcheck disable=SC2016` on
  line 172: SC2016 fires on any `$name` in single quotes and the new string still
  contains `$PPID` (verified by running shellcheck on the replacement). Touch nothing
  else in this file — the codex/cursor launch env (#46) and the pre-reap call site (#32)
  are fenced.

  Verify: `shellcheck adapters/core/dispatch.sh`.

- [ ] **Step 5: launcher mint + discovery notice** — `adapters/core/dispatcher.sh:99-108`

  `crew id` → `crew new` on line 102. Then, inside the existing
  `if git rev-parse --git-common-dir` block and after `crew register $$`, emit spec
  §4.1's notice — but **only** when the id was minted (i.e. `CREW_ID` was unset on
  entry, so capture that before `: "${CREW_ID:=…}"`) **and** `crew crews` holds at least
  one row whose id `!= $CREW_ID`. Excluding self is required: `crew register $$` has
  already created this crew's dir, so a naive count fires on every ordinary first launch.
  The whole notice is `|| true`-tolerant.

  The notice names `CREW_ID=<id> dispatcher …`, **not** `crew adopt` — on this entrance
  the agent has already inherited the new id in its env and adopt's export line lands in
  the wrong shell (spec §4.1).

  Also update the comment at lines 99-101, which currently says "Mint + export the crew
  id … `crew id` needs no git repo" — it would otherwise point at a command that no
  longer mints.

  Unlike `crew.sh` and `dispatch.sh`, **`dispatcher.sh` is not excluded from treefmt**
  (flake.nix:42-49, comment: "dispatcher.sh is NOT excluded: that one is ours"), so the
  new block must be shfmt-clean at `indent_size = 2`.

  Verify: `shellcheck adapters/core/dispatcher.sh`; `nix develop -c treefmt
--fail-on-change`; `bats tests/dispatcher.bats` stays green (every test there sets
  `CREW_ID`, which suppresses the notice).

- [ ] **Step 6: the `/dispatcher` command body, then regenerate adapters** — `adapters/core/commands/dispatcher.md`

  Replace the unconditional mint on line 9 with spec §6's three-step discover-first
  block. Both branches end in `echo "crew id: $CREW_ID"` — lines 17-21/35-38/63-64 tell
  the agent to note and reuse that literal, so dropping it strands the session. The
  re-attach branch uses the capture form `out=$(crew adopt <id> $PPID) && eval "$out" &&
echo …`; `eval "$(…)" && …` does **not** guard (verified: `eval "$(false)" && echo x`
  prints `x`).

  **The lead-in on line 6 must change with it.** It currently reads "First, mint a crew
  id and register this dispatcher, in one Bash call (plain bash — NOT `fish -c`):" —
  leave it and the file ships an imperative ordering the exact behaviour #29 removes, on
  the in-session restart entrance, and regeneration propagates the contradiction to all
  three engine copies. These files are model instructions; the bytes _are_ the behaviour
  (which is why flake.nix:74-82 keeps prettier off `^adapters/`). Replace with wording
  that names discovery first and drops the now-false "one Bash call", e.g. "First,
  discover this repo's crews and either re-attach to one or mint a new one, then
  register this dispatcher (plain bash — NOT `fish -c`):".

  Check the registration paragraph at lines 12-15 in the same pass: the adopt branch
  deliberately does **not** call `crew register` (spec §6), so either give it a clause or
  record in the PR body why it stands unchanged. The three paragraphs at 17-21, 35-38 and
  63-64 stay as they are.

  Then add `crews`, `new` and `adopt` to `README.md:90-92`'s "Full surface" list — it
  enumerates every subcommand, and a discovery primitive missing from the discovery
  documentation is its own small version of this bug.

  Then `bash scripts/gen-adapters.sh` and commit the regenerated trees.

  Verify: `git diff --stat` shows the three generated copies updated; re-running
  `gen-adapters.sh` produces no further diff (CI's drift gate asserts this);
  `bats tests/adapters.bats` green.

- [ ] **Step 7: tests** — new `tests/crews.bats`, plus the approved deletion

  New file `tests/crews.bats` following `tests/crew.bats`'s `setup()` shape
  (`load helpers`, `run_crew`, `setup_repo`). It needs a **local** `crew` stub for the
  launcher/dispatch rows — the shared `stub_bin` prints nothing and exits 0, so
  `crew new` would return an empty id. The `dispatch` row additionally needs
  `tests/dispatch.bats:10-11`'s env hygiene (`export HOME="$TEST_REPO"`, `unset
DISPATCH_PROFILE CREW_ID …`); a developer shell exports `CREW_ID`, and without the
  unset the abort under test never fires.

  Cover every row of spec §8, plus the two regressions this plan's own review added:
  - `crew register` then `crew deregister`, then `crew crews` → header only, no `*` row;
  - the launcher notice stays silent in that same empty-`crews/` state.

  Assert row presence and field values, never whole-table order (ordering among no-event
  crews is undefined).

  Then delete the two `tests/crew.bats` tests `id: mints <epoch>-<pid> when CREW_ID is
unset` and `id: works outside a git repo`, which encode the removed contract.
  **Dispatcher-approved on the bus** (ts 1786383132583). Delete **lines 40-51**, not
  40-50: lines 39 and 51 are both blank, so cutting 40-50 leaves two consecutive blank
  lines. One contiguous hunk.

  Verify: `nix develop -c bats tests/` green except the two known-red reap-release tests
  in `tests/crew.bats` (#54, #58 — owned by #49, do not fix). Then the full CI set:
  `shellcheck`, the `gen-adapters.sh` drift gate, `bats tests/`, and `nix flake check`
  (treefmt + pre-commit) — the last is the one a per-step shellcheck does not cover.

- [ ] **Step 8: capture the PR-body deliverables** — no source changes

  Three of the task's acceptance criteria are satisfied _in the PR body_, not in code, so
  a plan that ends at green tests is complete and still fails acceptance. Produce:
  1. **The literal recovery transcript** (`WORKER_TASK.md:140-142`). Run the whole path
     for real in a scratch repo with no `CREW_ID` — `dispatch …` aborting → `crew crews`
     → `crew adopt <id> $PPID` → `eval` → `dispatch --crew-id …` succeeding — and paste
     the actual terminal output. Pasted, not paraphrased: the criterion is that the
     printed messages alone get you there, and only a real transcript demonstrates it.
  2. **The `crew id` call-site enumeration** (`WORKER_TASK.md:110`) — spec §4's table.
  3. **The test deletion, made legible** (the dispatcher's bus reply, note 1) — name both
     deleted tests, say they encoded the old contract, and point at their `crews.bats`
     replacements, so a reviewer does not read the diff as coverage loss.

  Plus, under `## Escalated`, the spec-phase escalation (below) and the deliberately
  unchanged `status | msg` / `reply` / `watch` refusals as a named follow-up.

---

## Implementation notes (from the accepting plan-critic pass)

- **Do not hand-check `CREW_ID= crew id` from this worktree.** This repo root holds a
  `WORKER_TASK.md` with `crew_id: 1786381776-25460`, and `_crew_id` resolves it — so the
  command correctly exits **0** printing that id. That is spec §3's intended contract,
  not a bug: check the exit-1 path from a scratch repo or `cd /`, and do not "fix" the
  code to make it fail here.
- **Escaping applies to all three messages.** Step 3's `register` refusal (crew.sh:410)
  is a double-quoted `echo` and needs `\$PPID` exactly like step 1's. `shellcheck` cannot
  see an expanded `$PPID` inside double quotes; only the spec §8 test that asserts the
  literal `crew adopt <id> $PPID` appears in all three will catch it.
- **The launcher test row needs `tests/dispatcher.bats:6-12`'s full setup**, not just a
  richer `crew` stub: without the `claude` stub, `dispatcher.sh:115` execs a real agent
  (or dies 127 in CI); without the `tmux` stub and `unset TMUX`, lines 84-93 stamp the
  developer's live tmux window.
- **Keep step 5's notice in pure bash.** `dispatcher`'s `runtimeInputs` (flake.nix:122-126)
  are `git jq coreutils tmux` + `crew` — `awk`/`grep` resolve only from the inherited
  PATH. Use a `while IFS= read -r` loop over `crew crews`.
- **Step 8's transcript ends at the gate, not at a real dispatch.** A real
  `dispatch --crew-id …` scaffolds a worktree and tmux window and claims a GitHub issue
  via `gh`; it cannot succeed in a scratch repo with no origin and must not be run
  against the live repo. The final line only needs to show the crew-id gate passing —
  i.e. the abort no longer firing.
- **Run the generator through the devShell:** `nix develop -c ./scripts/gen-adapters.sh`
  (it needs `yq-go`, supplied only there — flake.nix:159), which is how CI invokes it.
- If step 6 records _why_ the lines 12-15 registration paragraph stands unchanged rather
  than editing it, that sentence belongs in step 8's PR-body list so it isn't dropped.
