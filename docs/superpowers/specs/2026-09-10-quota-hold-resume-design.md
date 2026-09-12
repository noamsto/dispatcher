# Spec — resume a quota hold without a human (#120)

Revision 3. Revisions 1 and 2 were adversarially gated; **revision 3 was not** —
the two-revision cap was spent and the third pass still returned four verified
defects, so they are folded in here un-gated and the escalation is carried to the PR.
The plan-critic is the next independent pass over this material.

## Problem

The budget lever can tell a dispatcher to stop. It cannot tell it to start again.
`DISPATCHER_PROTOCOL.md` → "Budget is the fifth lever" ends its hold rule with
"re-check on the first wake past it", which is passive: nothing records _what_ is
held, nothing guarantees a wake happens past the reset, and nothing re-runs
`refresh-budget` when it does. Every quota hold therefore ends by handing work back
to a human.

Two things landed since the issue was filed and change the answer:

- **#131** made the lever pace-aware. The judgment half is done, and the derived
  last-15% inequality stays the only near-reset notion in the system.
- **#137** moved the claude lane off the one-shot park onto `crew stream` under
  `Monitor`, armed once per session. The issue's central question — park-chaining
  vs. a scheduled wake, both framed against the 3300s park and the bounded-park
  invariant — no longer applies to that lane at all.

## The trigger, stated precisely

The hold rule fires on the protocol's **"Every fitting engine ≥95%"** bullet
(`DISPATCHER_PROTOCOL.md:123-125`), where _fitting_ means an engine this profile can
actually dispatch: `dispatch` aborts `--agent codex` / `--agent cursor` off the work
profile, so on a personal-profile host the fitting set is claude alone. An engine
that cannot be dispatched is not a fallback and must not be counted as one.

At ≥95% the alternatives are narrower than at ≥85%. `dispatch`'s gate
(`dispatch.sh:441-453`) selects **any** window at `used_pct >= 95` and exits 1
**regardless of model, rung or tier** — shedding burn class does not make the
dispatch possible. So the only moves are: rotate to a fitting engine that is not
full, `--ignore-budget` (a human spend decision), wait, or hand the task back. This
spec decides between the last two.

### Which window binds

Because the gate refuses on _any_ window, an engine with several exhausted windows
is gated by the **last one to reset**, not the first. claude routinely reports four
(`5h`, `7d`, `7d_opus`, `7d_sonnet` — `refresh-budget.sh:67-70`), so this is the
common case, not a corner:

- **Within an engine** — the binding deadline is the **latest** `resets_at` among
  that engine's ≥95% windows, and the floor is judged on that window. Judging `5h`
  at 40 minutes out while `7d_opus` is exhausted for three more days would wake into
  a dispatch that is still refused.
- **Across fitting engines** — take the **earliest** binding deadline, since the
  first engine to come back is the one that can run the work.
- **Release** — `wait.engine` has **no** window at ≥95%, matching the gate's own
  predicate (`dispatch.sh:446`). Not "the recorded window reset".

## Answers to the open questions

### Q1 — Where does the held task live? On the bus.

Context-only fails the exact thing "unattended" means: a dispatcher survives
compaction, `--resume`, and outright restart, and its context does not.

Holds go to a **synthetic sink**, `hold:<crew>`, as retro notes go to `retro:<crew>`
— but **crew-scoped on read**, unlike `crew retro`, which deliberately applies no
crew filter because notes are cross-run evidence (`crew.sh:2042`). A hold belongs to
one crew's queue.

Verified by running it, not by reading: `watch` matches `.kind=="msg" and
(.to==$me or .to=="*")` with `$me = "dispatcher:<crew>"` (`crew.sh:914-920`,
predicate on :917) and `inbox` filters identically (`crew.sh:1421-1426`). A msg to
`hold:<crew>` matches neither, so writing a hold cannot wake the dispatcher that
wrote it and cannot pollute its inbox, while `crew log` still shows it.

**The record carries the whole dispatch**, because a successor session has nothing
else to reconstruct it from. Two engine fields, kept distinct — the engine whose
quota is being waited on is not necessarily the engine the task was judged for:

```json
{
  "id": "1789059830351-31337",
  "wait": { "engine": "claude", "window": "7d_opus", "resets_at": 1789070400 },
  "task": {
    "ref": "#142",
    "branch": "feat/142-resume-a-quota-hold",
    "tier": "deep",
    "engine": "claude",
    "model": "opus",
    "effort": "high",
    "plan": "required",
    "mcp": null,
    "draft": false,
    "shape": null,
    "title": "…",
    "spec": ".git/crew/holds/1789059830351-31337.md"
  }
}
```

`id` is `<ms>-<pid>`. The `<epoch>-<pid>` _shape_ is the repo's idiom (`crew new`,
`crew.sh:311`; `dispatch` session ids), but those mint **seconds** — the hold's id
must be **milliseconds**, because the duplicate guard below compares it against the
ms `ts` fields `dispatch` writes (`dispatch.sh:603`, `:1017`). An implementer copying
`crew new` would break that comparison by a factor of 1000.

**`task.spec` is a file, not a field.** `DISPATCH_SPEC` is the inlined task body, and
without it "the worker only gets the title" (`DISPATCHER_PROTOCOL.md:167`) — which is
precisely the failure Q1's durable record exists to prevent. It is a caller-supplied
path, typically a temp file that will not survive a restart, and a bus line must fit
`_LINE_MAX` (4096 bytes, `crew.sh:180`), so it cannot be inlined either. `crew hold
add --spec <file>` therefore copies the body to `<crew_dir>/holds/<id>.md` and records
that path. The bus row itself still goes through `_fit_line` like `status` and `msg`,
so a long title shrinks rather than landing an oversized row every reader renders.

**`task.branch` is recorded at add time**, because the duplicate guard needs it and
nothing else exposes it: `dispatch` derives `feat/<issue>-<slug>` internally
(`dispatch.sh:552-563`) and `DISPATCH_PRECHECK` exits at `:548`, _above_ the slug, so
a resuming dispatcher cannot ask for it. The slug rule — lowercase, non-alphanumeric
runs to a single dash, first 40 characters, strip edge dashes — currently lives only
in a `dispatch.sh` comment; this spec writes it into `DISPATCHER_PROTOCOL.md` so the
dispatcher computes the same branch `dispatch` will.

**`task.ref` is required.** The dispatcher mints the tracker item _before_ placing the
hold, rather than letting `dispatch` mint it later. That costs nothing — the item has
to exist anyway — and buys two things: the human sees the queued issue without reading
the bus, and the guard gets a stable key.

**Out of the record, deliberately:** `--pr N` / `--review`. A review worker attaches
to a live PR head, and a PR that sat behind a quota hold for hours should be re-judged
against its current state rather than auto-attached to a stale SHA.

**Release is an event, because the bus is append-only** (`crew.sh:191`).
`crew hold release <id>` appends `{"id": <id>, "released": true}` to the same sink;
outstanding is adds minus releases, folded per crew.

**Release happens after the dispatch, and the guard is bus evidence, not the
roster.** A session that dies between the wake and the dispatch leaves the hold
outstanding, so it is re-announced — at-least-once, which is the right direction: a
lost task is invisible, a duplicate is not. But the roster cannot suppress the
duplicate: `crew roster` folds only `kind:"status"` rows from `worker:` senders
(`crew.sh:1303-1312`), and the first such row is the worker's own `working` — so a
crash after scaffolding but before the worker starts leaves _no_ roster row. Before
re-dispatching a hold, check instead for evidence `dispatch` writes at the start:

1. a `kind:"claim-issue"` row for `task.ref` newer than the hold's `id` — written
   **before any scaffolding**, precisely so a failure in between is not silent
   (`dispatch.sh:600-605`); or
2. a `kind:"dispatch"` row whose `branch` equals `task.branch`, newer than the
   hold's `id` (`dispatch.sh:1014-1018`, written at the end); or
3. an existing worktree for `task.branch`.

Any of the three means the dispatch already started. **Do not dispatch a second
worker, and do not blindly release**: inspect that worker, `dispatch resume` it if it
is dead, and release the hold once it is running. Releasing on a false positive would
drop the task, which is the failure this whole record exists to prevent.

**Named limitation:** `claim-issue` is GitHub-only — Linear-tracked dispatches write
no claim row (`DISPATCHER_PROTOCOL.md:162`). On those repos the guard is checks 2
and 3 only, so a crash between scaffolding and the final `dispatch` row is caught by
the worktree check alone. That is a real narrowing, stated rather than hidden.

### Q2 — Release one held task per wake, never a whole fan-out.

Auto-resuming a six-worker fan-out into a freshly refilled window exhausts it again
in twenty minutes, and the pace rule from #131 would then correctly refuse the
premium rung for the rest of the window — the resume would have _cost_ the crew its
rung.

So a wake releases **one** hold, at **full** strength, and stops. The queue drains
through the loop the protocol already has: terminal worker states are budget-freeing
and "the same wakeup tells you when to dispatch the next queued task"
(`DISPATCHER_PROTOCOL.md:330`). That loop does not consult holds today, so this spec
makes it explicit — and deliberately does **not** scope the check to terminal
batches: **a dispatcher checks `crew hold due` on every stream notification and
every park wake**, terminal or not. Scoping it to terminal batches would invent a
gap on an active roster, then require code to close it.

### Q3 — The floor is the derived last-15%, and it lives in exactly one place.

The issue asks whether the pace rule already implies the floor. It does: the
protocol already states the wait boundary per window in last-15% terms — `5h`
"Inside its last 15% (~45m to reset), hold and wake past the reset… **Longer than
that, shed as before**"; `7d` "Inside its last 15% (~25h to reset), prefer waiting".
That is the same boundary #131 derived, since `remaining <= 0.15 × L` is
`elapsed_pct >= 85`.

**Where it lives is the load-bearing decision.** `wsecs` and `elapsed_pct` are
`local` jq defs inside `refresh-budget.sh`'s `main` (`:296-307`); there is no shared
library in `adapters/core/`, each script is its own `writeShellApplication`
(`flake.nix:104-156`), and `crew.sh` reads `engine-budget.json` nowhere today (zero
grep hits). Implementing the floor inside `crew hold add` would inline a third copy
of the 85 threshold and a second window-length table into `crew.sh` — precisely the
"second near-reset constant" the task forbids.

So: **the floor is computed in `refresh-budget.sh`, beside the helpers it already
owns, and nowhere else.** Its `budget lever:` renderer already selects every window
at `used_pct >= 85` and already has `$L`, `$rem` and `elapsed_pct` in scope; it
gains a hold verdict for the ≥95% rows. This also fixes a live inconsistency: the
current `5h` advice, "prefer waiting past the reset to shedding burn class", is
unconditional on remaining time and so contradicts the protocol's own "longer than
that, shed as before" (`refresh-budget.sh:321`).

The verdicts are `warn` lines (`refresh-budget.sh:327-329`), so a test has exact
strings to assert on. **The renderer folds per engine before judging**, because the
gate refuses on any window: only the binding window (the latest-resetting ≥95% one)
carries a holdable verdict, and its siblings say which window actually binds.
Emitting a verdict per row would reintroduce the wrong-window bug at the layer the
acceptance criteria assert on.

```
budget lever: claude 7d_opus at 96% (resets in 4d 2h) — binding window; not holdable: 4d 2h is outside the window's last 15%, hand the task back
budget lever: claude 5h at 97% (resets in 38m) — not binding: claude is gated until 7d_opus resets
budget lever: codex 5h at 99% (resets in 38m) — binding window; holdable: inside the window's last 15%, wait past the reset
budget lever: codex 1d at 99% (resets in 6h 12m) — not holdable: window has no nominal length, hand the task back
budget lever: claude 5h at 97% — not holdable: no reset time, hand the task back
```

**Two separate exclusions, two separate predicates.** Either one makes the whole
engine unholdable, since either leaves a window that gates the dispatch and cannot be
waited out:

- **No nominal length** — `wsecs` is partial by design (`refresh-budget.sh:303-305`):
  codex buckets a window it cannot size to `1d`, `unknown` or `other` (`:242-244`),
  and such a window still carries a real `resets_at` (`:251-252`). A window that
  cannot be placed on its own timeline cannot be judged short-enough-to-wait.
- **No usable deadline** — a ≥95% window whose `resets_at` is null, or not in the
  future, bounds nothing. This is live, not hypothetical: `probe_claude_pane_scrape`
  emits `resets_at: null` for a _sized_ `5h`/`7d` window when the countdown does not
  parse (`refresh-budget.sh:210-218`, and `tests/refresh-budget.bats:308` pins it),
  and `dispatch.sh:492` already carries its own null branch. Keying only on `wsecs`
  would let such a window through, and "latest `resets_at`" would then silently drop
  it, since null sorts below every number — so the hold would key on a deadline that
  does not unblock the gate.

Both are the task's "unknown is neutral, never a wake target" rule applied to a
_window_, and neither needs a new constant. codex is fully served regardless: its
real windows are 5h and 7d, and the unsized buckets are defensive fallbacks for
backend shape drift.

`crew hold add` records what it is told and validates **shape only** — a future
`resets_at`, a named engine, a window key, a `task.ref`, a dispatch tuple. It never
re-derives the floor.

**When the floor refuses, the task goes back to the human** — with the deadline, so
the refusal is a stated decision rather than a silent non-hold. "Shed burn class" is
not available at ≥95% and this spec does not claim it is.

## Per-lane wake, because the lanes genuinely differ

### claude — the stream is already the carrier; give it the deadline

`crew stream` is a long-lived process that loops an inner `crew watch --timeout
$park` (default 300s) and emits a heartbeat after `--heartbeat` seconds of quiet
(default 3300s). It is armed once per session, is not bounded by a park, and needs
no re-arming. **The claude lane therefore already has an unattended clock**, and
neither park-chaining nor a scheduled wake is needed. The stream is the right
carrier.

With the Q2 rule widened to every notification, the batch path already covers an
active roster. What remains is the quiet path, and that is where the argument for a
third notification kind actually lives:

**A quota hold drains the roster by construction.** Holding means not dispatching,
so the held crew stops producing batches and the quiet path _is_ the normal path for
exactly the state this feature exists to serve. Its clock is `--heartbeat`, 3300s.
The longest legal wait for a `5h` window is its last 15%, 2700s. **The quiet path's
resolution is coarser than the entire wait it must measure** — the wake can arrive
after more time than the hold was ever meant to last. That is a broken common case,
not a latency preference.

So `crew stream` emits a third notification kind while any outstanding hold has
matured:

```
{"stream":"hold_due","crew":"<id>","holds":[{"id":…,"wait":{…},"task":{…}}],"ts":<ms>}
```

- Checked at the top of every stream iteration — where the tick is already written
  — so it fires on the batch path as well as the quiet path. The gap between two
  checks is `--park` on the quiet path and `--park + --coalesce` after a batch (305s
  at the defaults), not `--heartbeat`.
- **Level-triggered, not edge-triggered**, so a queue cannot strand its tail.
  Re-emission is suppressed by the idiom the error path already uses
  (`crew.sh:1252-1264`): a key over the matured id set, re-emitted when the set
  changes or after `--heartbeat`. Releasing one hold changes the set, so the
  remaining holds announce on the next iteration rather than at the next stream
  restart.
- A stream restart may re-announce a still-outstanding hold. Correct: the earlier
  announcement died with the process.
- The bus read is guarded so a missing or unreadable log cannot abort the loop under
  `set -e`, the same care the existing batch and error paths take.

### cursor — park-chaining, with the one bound it cannot close

The cursor lane is still the re-armed one-shot with the ACTIVE/DRAINED table, so
park-chaining is the right shape. The park-length table gains a hold branch: with an
outstanding, not-yet-matured hold, park to the deadline instead of the branch
default — `min(branch, seconds until the earliest deadline)`.

**Matured means `resets_at <= now`.** No clamp and no floor: a short park is
self-limiting because a matured hold is released and stops shortening anything, and
`crew hold park <default>` returns the branch default when there is no outstanding
hold or the earliest has already matured. It **never prints below 1** — `crew watch`
rejects `--timeout 0` outright (`crew.sh:869-872`), and a 0 here would fail the
re-arm and break INV-1's "never zero" until the next completion. This keeps `crew.sh` free of both bounds — it needs only `resets_at - now`
— which is what the acceptance criterion below asks for. Clamping above by the
branch default leaves the finite-timeout guarantee `watch` enforces for itself
(`crew.sh:869-872`) untouched; a hold further out simply chains parks.

INV-1 ("exactly one outstanding watch: never two, never zero",
`DISPATCHER_PROTOCOL.md:256`) is unaffected — this changes a park's length, not the
number of outstanding watches.

**The bound this lane cannot close.** The park table is consulted **only at
re-arm**, and a human turn must not re-arm (`DISPATCHER_PROTOCOL.md:258-270`). A
hold is placed in a dispatch or human turn, so a park already outstanding keeps its
original length: on a DRAINED roster that is 3300s, and the deadline park applies
only from the next re-arm. **A hold placed on a drained cursor crew is therefore
woken up to one park (≤3300s) late.** On an ACTIVE roster the outstanding park is
270s and the overshoot is ≤4.5 minutes. This is named rather than closed — the task
sanctions naming a lane's limit over leaving it silent, and closing it would mean
killing the outstanding watch from a human turn, which is more fragile than the
overshoot it would fix.

### codex — no change, and that is the finding

The codex park **is** the turn, and the protocol already refuses the 3300s drained
park there for exactly that reason: always 270. That gives codex 4.5-minute wake
resolution — better than either other lane — for free. **The codex lane needs no
park change at all**, and this spec makes none. It adds only the hold check on each
wake, which the lane already performs every 270s regardless.

A multi-hour hold does not make codex worse than it already is: a codex dispatcher
with a drained roster loops 270s foreground parks either way, and human input typed
during a park queues and is delivered when the turn ends.

## An `unknown` engine is neutral in both directions

- **Never held on.** A hold needs a future `resets_at`, and an `unknown` engine
  reports no window at all, so there is nothing to pass. `crew hold add` rejects an
  absent or past `resets_at`; it does not otherwise validate the engine, and this
  spec does not claim it does.
- **Never woken into.** Release requires `wait.engine` observed with no window ≥95%
  after a fresh `refresh-budget` — not "something is unknown, so go".
- **Never counted as a fallback at the trigger**, per the trigger section.

cursor is permanently `unknown` (its quota is unobservable); codex is `unknown` on a
personal-profile host, where `dispatch` refuses `--agent codex` outright. Where the
codex CLI exists, `refresh-budget` already reads codex quota and its `resets_at`, so
codex participates with no extra parsing.

## Telling the human, at all three ends

- **On placing a hold** — what is held (with its tracker ref), which engine and
  window supplies the binding deadline, and the deadline in both forms. Reuse
  `refresh-budget`'s existing `resets <ISO>, in <4h 19m>` wording rather than
  inventing a second phrasing.
- **On resuming** — which hold resumed, that it resumed at full strength, and what
  it was waiting on.
- **On refusing a hold** — that the deadline is outside the window's last 15% (or
  the window has no computable length), the deadline itself, and that the task is
  going back to them.

## Release sequence

On a `hold_due` notification (claude) or a wake past the deadline (cursor/codex):

1. Re-run `refresh-budget` — the cached quota is stale by definition, the hold
   outlasted it.
2. Re-check `wait.engine`. **No** window ≥95% → release.
3. Run the three-way duplicate guard (Q1). Evidence of a started dispatch → release
   the hold and inspect that worker instead.
4. Otherwise dispatch **one** hold at its recorded `task` tuple, and only then
   `crew hold release <id>`.
5. Still ≥95% → record a fresh hold against the new binding deadline and release the
   old one. **A re-hold is not always available**: at the boundary the probe can
   report a window still ≥95% whose `resets_at` has just passed
   (`tests/refresh-budget.bats:112` pins that shape), and `crew hold add` rejects a
   past deadline. Re-probe once; if the deadline is still past, hand back to the human
   and say the reset had not landed yet. **This is a judgement rule, not a mechanical guarantee**: nothing in the
   code bounds a hold→wake→re-hold cycle, because the floor is a human-readable
   verdict from `refresh-budget` that `crew hold add` never re-derives. The
   dispatcher applies it; a window that keeps failing it goes back to the human.

## Sequencing

The task doc says #136 is in flight on `DISPATCHER_PROTOCOL.md` and to rebase rather
than race it. Checked at the time of writing: **#136 was an open issue with no PR and no branch**
(`gh pr list` empty; `gh pr view 136` unresolvable), and this branch sat at the tip of
`extract`, so there was nothing to rebase onto. Re-check before pushing. Either way
its "Roster diagram" section is untouched by this spec.

## Scope

**In:** a `crew hold` subcommand (`add` / `list` / `due` / `park` / `release`); a
level-triggered `hold_due` notification in `crew stream`; a hold verdict in
`refresh-budget`'s advice renderer, where the floor lives; protocol prose for the
hold rule, the binding-window rule, the per-lane wake, the park-table hold branch,
and the every-notification hold check; tests; regenerated adapters; the `crew`
surface list in `README.md`.

**Out:** any change to the `5h`/`7d` thresholds or the pace inequality (#131 owns
them); any change to the codex park length; any daemon or external scheduler; any
change to how `refresh-budget` probes an engine; any change to `dispatch`'s two
gates.

## Acceptance

- [ ] A hold survives a dispatcher restart: written to `hold:<crew>`, crew-scoped on
      read, carrying `task.ref`, `task.branch`, the dispatch tuple, and a `task.spec`
      path whose file holds the inlined task body.
- [ ] A resumed hold dispatches with its spec body, not just its title.
- [ ] Hold rows shrink through `_fit_line` like `status` and `msg`.
- [ ] `wait.engine` and `task.engine` are distinct fields; the release path reads the
      former and requires **no** window of it at ≥95%.
- [ ] An engine exhausted on two windows holds against the **later**-resetting one,
      and the earlier window's row reads `not binding`, never `holdable`.
- [ ] A ≥95% window with a null or already-past `resets_at` makes its engine
      unholdable, on a sized window as much as an unsized one, each with its own
      quoted verdict string.
- [ ] Writing a hold does not wake the dispatcher that wrote it and does not appear
      in its inbox.
- [ ] Release is an append-only event; outstanding folds as adds minus releases.
- [ ] A crash between the wake and the dispatch leaves the hold outstanding, and the
      three-way guard — a `claim-issue` row for `task.ref`, a `dispatch` row for
      `task.branch`, or a worktree for `task.branch` — suppresses the second dispatch
      without dropping the task.
- [ ] **Two holds maturing together both resume**: releasing the first re-announces
      the second on the next stream iteration, without a stream restart.
- [ ] Every lane has a named wake past the deadline: claude via `hold_due` within
      `--park + --coalesce`; cursor via a deadline-sized park, **woken up to one park
      late when the hold is placed against an already-outstanding park**; codex via
      its existing 270s park.
- [ ] `crew hold park` never returns 0, so a re-arm cannot fail INV-1's "never zero".
- [ ] The codex park length is unchanged; no 3300s park is introduced there.
- [ ] The last-15% test appears in exactly one place in the diff
      (`refresh-budget.sh`); `crew.sh` gains no threshold, no window-length table,
      and neither park bound.
- [ ] `crew hold add` refuses an absent or past `resets_at`, so an engine reporting
      no window can never be a wake target.
- [ ] A release re-runs `refresh-budget`, dispatches exactly one hold at full
      strength, and releases only after the dispatch.
- [ ] The human is told at hold time, at resume time, and when a hold is refused.
- [ ] `Closes #120`; adapters regenerated from `adapters/core/`; shellcheck clean;
      full bats suite green.

## Escalated — the revision cap was spent

Revisions 1 and 2 were adversarially gated and each fixed four blocking findings. The
third pass returned four more, all verified against the code, at which point the
two-revision cap was spent. They are folded into revision 3 **without a further
critic pass**:

1. The hold record omitted `DISPATCH_SPEC`, so a restored hold would have dispatched
   with a title and no task body — now persisted to `<crew_dir>/holds/<id>.md`.
2. The floor verdict was emitted per ≥95% row with no per-engine fold, which
   reintroduced the wrong-window bug in the renderer — now folded, with the binding
   window the only one that can read `holdable`.
3. A ≥95% window with a null or past `resets_at` on a _sized_ window escaped the
   unholdable rule, which keyed only on `wsecs` — now a second, separate predicate.
4. The duplicate guard keyed on a "resolved branch" nothing exposed — now recorded as
   `task.branch`, with the slug rule written into the protocol.

**What this means for the reader:** the design in revision 3 has not itself been
adversarially reviewed. The plan-critic gate is the next independent pass over the
same material, and the code review gate follows it.
