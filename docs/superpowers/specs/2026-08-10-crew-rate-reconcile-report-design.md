# `crew rate` — GitHub reconcile, cost proxy, and the routing rollup

**Issue:** #36 · **Consumer:** #86 (routing judge) · **Date:** 2026-08-10
· **Supersedes the unbuilt half of** `2026-07-22-dispatch-worker-model-ratings-design.md`

## What already exists (and why this spec is smaller than the issue)

The issue reads as if nothing is built. It is out of date on two counts, both
verified before writing a line:

1. **The "stale unmerged branch `feat/102-dispatch-ratings` in `noamsto/nix-config`"
   is not stale and not unmerged.** It shipped as nix-config PR #106
   ("Phase 1 — data capture"), merged 2026-07-22, and was carried into this repo
   by `4eed165` when the harness was extracted. There is nothing to salvage
   because it is already here. Its design doc is the spec cited above.
2. **Phase 1 is live in `crew.sh`.** `crew rate` already sweeps the repo bus into
   `${XDG_DATA_HOME:-~/.local/share}/crew/ratings.jsonl` with dispatch-boundary
   windowing, the roster `pr_url` carry-forward idiom, and the `_lock_acquire`
   gate. `WORKER_PROTOCOL.md` already mandates the `metrics:` emit **on all
   tiers**, so the issue's "extend deep-only to all tiers" is already satisfied.

So the deliverable here is the unbuilt half: **t2 GitHub reconciliation, the cost
proxy, and `crew rate --report`** — plus closing the four open questions the task
asks to settle in a spec rather than in review.

## The four open questions, answered

### 1. `run_id` — single-writer derivation, and what survives of "two observers"

`run_id = "<repo>:<branch>:<t0_ms>"`, where `t0_ms` is the `kind:"dispatch"`
event's timestamp and `repo` is `remote.origin.url` reduced to `owner/name`.

The two-observer split is real and stays: the worker uniquely writes
`rework_count`, `review_high`, `review_mode`, `plan_critic_first_pass`,
`replanned` and `consulted` into its `metrics:` body, and GitHub uniquely holds
the t2 fields. What is _not_ two-sided is **`run_id` derivation**: there is exactly
one writer of `ratings.jsonl` — the sweep — so no two parties ever have to compute
a matching id. The worker emits bus events addressed from
`worker:<branch>#<session>` and the sweep attributes each to a run by
**dispatch-boundary windowing**: for a branch's ordered dispatch timestamps
`d₁<…<dₙ`, an event with `dᵢ ≤ ts < dᵢ₊₁` belongs to run `i`. That join is what
makes branch reuse safe on the bus side; §"Branch reuse and t2" covers the side
windowing does _not_ protect.

Last-wins by `run_id` then works because re-sweeping the same bus recomputes an
identical `run_id` — `t0` is an immutable historical event, not a clock read.

**One caveat, stated rather than solved:** `repo` falls back to the toplevel
basename when `remote.origin.url` is unset (`crew.sh:732-734`), so sweeping the
same run from a checkout without an `origin` mints a different `run_id` prefix and
yields a second row that never folds with the first. Every dispatched repo has an
`origin` (dispatch opens PRs), so this is documented, not defended against.

### 2. `shape` — defined, captured, and deliberately **not** a normalization axis in v1

`shape` is already a closed vocabulary in `dispatch-orchestration.md`:
`mechanical | ui | ambiguous | security | wide`. It reaches the bus from the
`DISPATCH_SHAPE` env var (`dispatch.sh:633`).

**Nothing sets it.** No `--shape` flag exists, `DISPATCHER_PROTOCOL.md` never
tells the dispatcher to export it, and this task is scoped out of editing that
protocol. So today `shape` is the empty string on every dispatch event, and
"normalized by (tier, shape)" would stratify by a constant — splitting
already-tiny cells for zero information.

The task said "define it or cut it". There is a third option and this spec takes
it deliberately: **capture, don't stratify.** The sweep records `shape` on the row
(normalizing `""` → `null`, since `dispatch.sh` writes an empty string, not a
null) but `--report` groups by `(engine, model, tier)` only. A captured-but-unused
field earns its place here because the store is append-only historical evidence:
if capture waits for the consumer, the day `DISPATCH_SHAPE` gets populated we start
from n=0 instead of already holding the runs. Tier _is_ the normalization that
matters and is real — it is a grouping key, so a deep run's cost and rework are
never averaged against a trivial run's.

### 3. A PR still open when the sweep runs — convergence, not a premature terminal row

The sweep writes the row it can honestly write, and re-sweeps upgrade it via
last-wins. Two fields carry this, and **keeping them separate is load-bearing**:

- **`pr_state` ∈ `OPEN | CLOSED | MERGED | null`** — a _fact about the PR_. It is
  written only from a **successful** query. A failed query never touches it; the
  stored value is carried forward.
- **`last_query_ok` ∈ `true | false | null`** — a _fact about the sweep_. `false`
  means this sweep tried and could not reach GitHub; `null` means it had no reason
  to try.

Collapsing these into one field is how a transient failure would bias the rollup:
`merge%` is keyed on `pr_state`, and a stored `MERGED` row is never re-queried
while `OPEN`/`CLOSED` rows are — so a shared health-and-fact field would let an
expired token remove _failures_ from the merge denominator while leaving
_successes_ intact, inflating merge rate with infrastructure flakiness. With the
split, a failed query changes no denominator at all.

| stored `pr_state` | next sweep                                                                                                                                                                                                                                                                                                     |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `null`            | queries, if the run owns a GitHub PR URL                                                                                                                                                                                                                                                                       |
| `OPEN`            | re-queries                                                                                                                                                                                                                                                                                                     |
| `CLOSED`          | re-queries while `now − closed_at_ms < 30d` — a closed PR can be reopened and merged, so it is not immediately terminal; after 30 days it is treated as settled, which bounds the cost of a repo full of abandoned branches                                                                                    |
| —                 | `OPEN` has **no** age bound and so is the one unbounded re-query set: a long-lived abandoned open PR costs up to 3 calls per sweep forever. Accepted — it is bounded by the count of open PRs, which a human closes eventually, and capping it by age would write a terminal row for a PR that is not terminal |
| `MERGED`          | skips `gh pr view` only; see the per-call finality rule below                                                                                                                                                                                                                                                  |

`merged:false` on an `OPEN` row therefore never reads as "this run failed to
merge": `merge%` is taken over `pr_state ∈ {MERGED, CLOSED}` only, and `OPEN` rows
are counted in a separate `pend` column.

### 4. `protocol_ok` — it already exists as `reported_ok`, and it is genuinely falsifiable

The shipped derivation (`crew.sh:794`) is:

```
reported_ok = (a metrics body was present and parsed) AND (any status event was observed)
```

It is **not** "reached a terminal status" — `$ls` is the last status of any kind,
so `working` or `blocked` satisfies it. This spec does not change that derivation:
redefining a field in place is exactly the comparability hazard the existing
`watchdog_blocked_count` comment in `crew.sh` was written to avoid, and the same
reason the field is not renamed to the issue's `protocol_ok` spelling. The issue's
name is the concept; `reported_ok` is its shipped identifier, and this section is
the mapping.

It is false whenever a worker dies before its stopping path: `tmux kill-window`, a
crashed turn, an unanswered permission prompt, or a metrics body so truncated that
`try fromjson` yields null. Those are the runs the routing judge must not score as
clean — so the field is not always-true. It is sweep-derived rather than
worker-stamped because **a worker that failed to report cannot report that it
failed to report**; self-attestation is structurally impossible here.

## t2 — the GitHub reconcile

### What gets queried, and against what

Every call is targeted from **the run's own `pr_url`**, never from the sweeping
worktree's ambient state. `pr_url` is parsed as
`https://github.com/<owner>/<name>/pull/<N>`; a URL that does not match that shape
yields no calls at all. `<owner>/<name>` from _that URL_ — not the origin-derived
`repo` — addresses every call, so a store holding runs from several repos never
cross-queries:

- `gh pr view "$pr_url" --json state,closedAt,mergedAt,mergeCommit,commits,reviews`
- `gh api "repos/<owner>/<name>/actions/runs?branch=<branch>&per_page=100"`
- `gh api graphql` → `repository(owner,name).pullRequest(number:N).reviewThreads(first:100){isResolved}`

An argument-less `gh pr view` would resolve the PR of the sweeping checkout's
current branch and silently write it into every row — the reason the target is
spelled out here rather than left to the implementer.

**Hard requirement: a run with no `pr_url`, or a non-GitHub `pr_url`, issues zero
network calls.** The four pre-existing `crew rate` tests in `tests/crew.bats` run
sandboxed with no network, and that file is off-limits to this task; their fixture
URL is `https://example.com/pr/1`, which fails the shape match.

### Per-call finality — the skip is per call, not per run

A run-level "skip if MERGED" would strand any field whose own call failed on the
sweep that first saw the merge: it would be `null` forever with no path back. Each
call is therefore skipped only when **its own** output is final:

| call           | skipped when                                                                                |
| -------------- | ------------------------------------------------------------------------------------------- |
| `gh pr view`   | `pr_state == "MERGED"`, or `CLOSED` older than 30d                                          |
| Actions runs   | `first_ci_green != null` — it describes the _first_ CI, which never changes once known      |
| review threads | `pr_state == "MERGED"` **and** `unresolved_notes != null` — threads stop moving after merge |

So a stored `MERGED` row with a null `first_ci_green` still issues the Actions
call on the next sweep. Any call failing sets `last_query_ok: false` and leaves
that field's stored value untouched (see §Merge-forward); nothing is ever fatal to
the sweep.

### Fields

| Field                                          | Derivation                                                                                                                               |
| ---------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `pr_state`                                     | `.state`, written only on a successful query                                                                                             |
| `merged`                                       | `pr_state == "MERGED"`                                                                                                                   |
| `closed_at_ms`, `merged_at_ms`, `merge_commit` | `.closedAt`, `.mergedAt`, `.mergeCommit.oid` — stored so later sweeps bound the CLOSED re-query and re-check `reverted` with no API call |
| `time_to_merge_ms`                             | `merged_at_ms − t0`                                                                                                                      |
| `review_rounds`                                | review→fix _cycles_, below                                                                                                               |
| `first_ci_green`                               | earliest CI **within this run's window**, below                                                                                          |
| `unresolved_notes`                             | count of `isResolved == false` review threads                                                                                            |
| `reverted`                                     | local `git log` probe, below                                                                                                             |
| `owns_pr`, `last_query_ok`                     | §Branch reuse, §3                                                                                                                        |

**`review_rounds` — a review→fix _cycle_, not a review count.** Take reviews with
state `CHANGES_REQUESTED` or `COMMENTED` (an `APPROVED` review is not a rework
signal), sorted by `submittedAt`. A round is a maximal group of such reviews
followed by ≥1 commit whose `committedDate` is later than the group's last review.
**Three reviewers commenting before one fix commit is one round, not three** —
this is the semantics that drifts if it is not pinned, so a test pins it. Reviews
with no subsequent commit contribute 0.

**`first_ci_green` — the earliest CI _this run_ triggered.** `statusCheckRollup`
reports the head commit's checks, which answers a different question ("is it green
now"), and `commits[0].oid` is usually wrong too: a worker pushing three commits at
once gets check-runs only on the tip, so the first commit has no checks and the
field would be null on nearly every run. Instead: from the Actions query, **keep
only runs whose `created_at` falls in this run's dispatch window
`[t0, t_next_dispatch)`**, group by `head_sha`, take the sha with the smallest
earliest-`created_at`, and report `true` iff every **completed** run on that sha
concluded `success | skipped | neutral`. No runs in the window (a repo without
Actions, or a branch whose CI belongs to an earlier run) → `null`, counted as
unmeasured and never as a red `false`. The window filter is what stops run _i+1_ on
a reused branch from inheriting run _i_'s CI.

**`unresolved_notes` — real unresolved review threads, not the PR body.** The
obvious cheap parse — bullets under the worker-authored `## Review notes` heading
— is a **t1 field wearing a t2 label**: the worker writes it at PR-open time, it
is immutable across every re-sweep, and it is cheaper to read off the bus. The
issue puts this field in the "data the worker cannot know" bucket, so it is
sourced from review threads, whose count genuinely changes as reviewers resolve
them, so re-sweeps converge. `null` if the call fails.

**`reverted` — bounded, local, and three-valued.** Only for merged runs with a
known `merge_commit`. First an existence probe — `git cat-file -e <oid>^{commit}`
— because without it "not reverted" and "merge commit not in local history" are
indistinguishable and the field would quietly report `false` for every unfetched
repo. If the object is absent → `null` (unknown). If present:
`git log --all --since=<mergedAt> --grep="This reverts commit <oid>" --max-count=1`,
non-empty ⇒ `true`. `--since` bounds the traversal, since a revert necessarily
post-dates its merge, and the probe costs no API call. This is the weakest of the
six t2 fields: cheap enough to be worth having, honest enough not to mislead.

**Pagination caps are accepted, not solved.** `reviewThreads(first:100)`,
`actions/runs?per_page=100` and `gh pr view --json commits` all truncate on very
large PRs. A dispatched worker's PR is far below those limits; a PR that exceeds
them undercounts rather than errors.

### `outcome` — full precedence

`n` is defined as "rows whose outcome is neither `running` nor `incomplete`", so
an unspecified outcome map is an unspecified denominator for every column. The
derivation replaces the `outcome:` expression at `crew.sh:776-780`, first match
wins:

| #   | condition                                                                                            | `outcome`    |
| --- | ---------------------------------------------------------------------------------------------------- | ------------ |
| 1   | `owns_pr && pr_state == "MERGED"`                                                                    | `merged`     |
| 2   | last status ∈ {`working`, `blocked`}                                                                 | `running`    |
| 3   | `pr_url != null`                                                                                     | `pr_open`    |
| 4   | last status ∈ {`done`, `failed`}                                                                     | `failed`     |
| 5   | **otherwise** — including no status at all, last status `exited`, and a `pr_open` posted with no URL | `incomplete` |

Rule 5 is the default arm, not a fifth condition. `crew status` accepts
`pr_open` without a URL (`crew.sh:269-277` omits the key when the 4th arg is
empty), so a table of five conditions would leave that run with no `outcome` at
all — in neither `n` nor `inc`/`run`, silently absent from the rollup. The
expression it replaces (`crew.sh:776-780`) ends in `else "incomplete"`, and this
one must too.

Rule 4 preserves shipped behavior: `done` with no PR is a failure to deliver, and
`crew.sh:779` already maps it that way. Rule 3 keeps its shipped name even though
it now also covers a `CLOSED` PR — `pr_open` means "reached a PR, not known
merged", and renaming a value inside an append-only store is the comparability
hazard §4 exists to avoid. `running` is new: without it every in-flight worker
lands in `incomplete` and inflates the tally that exists to keep zombies honest.
Both `running` and `incomplete` are excluded from every rate denominator.

### Branch reuse and t2

Dispatch-boundary windowing partitions _bus events_. It does not partition
GitHub: two runs on one reused branch both carry the same `pr_url` through the
per-window carry-forward, so naively both would score `merged: true` off one
merge, and the earlier run's `time_to_merge_ms` would span the later run's entire
life — double-counting one merge across two rungs.

**Attribution rule, entirely bus-derived so it cannot be circular:** the
candidates for a `pr_url` are the runs on that branch whose window contains a
`status` event carrying it; the **owner is the earliest such candidate** — the run
that opened the PR. Ownership is therefore decided _before_ any query, which
matters because the query gate depends on it: a rule derived from the query's own
commit list could not be evaluated without first making the call it gates.

Non-owning candidates keep `pr_url` as evidence and get `owns_pr: false`,
`pr_state: null` and every other t2 field null; they are excluded from the merge
and CI denominators, and their `outcome` is `pr_open` by rule 3 — so they still
count as having reached a PR, which they did.

**t1 needs the same treatment, for the same reason.** The owner rule requires a
non-owning candidate to have posted a status carrying the `pr_url` inside its own
window — and the shipped t1 derivation turns exactly that event into
`reached_pr: true` and `time_to_pr_ms = $propen.ts − t0` (`crew.sh:773-775`). A
fix-up run would therefore contribute a near-zero `ttpr` that measures _time to
re-announce an existing PR_, poisoning a headline column with the branch-reuse
credit this section exists to remove. So: **`time_to_pr_ms` is null when
`owns_pr == false`**. `reached_pr` stays `true` — the run did reach a PR, that is
simply a fact — and the mild generosity in `pr%` is preferred to claiming a
fix-up run produced nothing.

**The bias this chooses, stated:** crediting the _opener_ means a `deep`/opus run
that produced the PR owns the merge even if a later cheap fix-up run pushed the
last commit. Crediting the newest committer instead would transfer merge credit
from expensive rungs to cheap fix-up rungs and shrink their `ttmerge` — the same
inflation with the sign flipped, and the more damaging direction for a system whose
purpose is comparing rungs. Opener-attribution is the deliberate choice; a
`pr_role` split (opener vs finisher) is a follow-up if re-dispatch onto a live PR
becomes common. The cost of this choice, stated so it is not rediscovered as a
bug: the opener's `time_to_merge_ms` spans the later run's entire life, because
`merged_at − t0` is measured from the _opener's_ dispatch.

### Merge-forward

For any field whose fresh query did not succeed, the sweep **carries the stored
value forward unchanged** rather than writing null over it. Without this, one
expired token or one offline sweep permanently degrades every already-reconciled
row, because last-wins would fold the report to the degraded version. t1 fields
always come from the fresh local bus fold — they recompute identically every time.

Having read the store anyway, the sweep **appends only rows that differ from the
stored one ignoring `swept_at`**. That field is `now`, so without the exclusion
every sweep would append every row and an idle repo's store would grow without
bound.

**Locking — two short windows, never held across the network.** The sweep (1)
takes the lock, folds the store to the last row per `run_id`, releases; (2) makes
its GitHub calls unlocked; (3) re-takes the lock, merges forward against a fresh
read, appends the changed rows, releases. Holding the lock across step 2 would
make `crew rate` a multi-minute lock-holding command on a repo with many historical
runs. `_lock_acquire` is non-blocking (`crew.sh:118`), so a loser prints
`crew: ratings store busy` and exits 1 — shipped behavior, retained.

Two consequences of splitting the window, both accepted rather than engineered
away:

- **No _permanently_ lost update** — the stronger claim would be wrong. A slow
  sweep holding a successful `OPEN` response can re-take the lock after a fast
  sweep already wrote `MERGED`, and append the staler `OPEN` with a later
  `swept_at` (merge-forward only protects _failed_ queries). It self-heals,
  because `OPEN` is not final and the next sweep re-queries it. Every non-final
  state is re-queried, so the store converges; it is not instantaneously
  consistent.
- **Losing the _second_ acquisition discards that sweep's network work** — it
  exits 1 having written nothing, after paying for the calls. There is no retry:
  the next sweep redoes them. This is a genuinely new failure mode versus holding
  one lock throughout, and it is the price of not blocking every other repo's
  sweep for the duration of a multi-minute reconcile.

**Against the task's non-goals.** The task says "do not reach for a mutable store
or a lock". The store stays strictly append-only and last-wins — that contract is
untouched. The lock is **not introduced here**: `_lock_acquire` around
`ratings.jsonl` is shipped phase-1 code (`crew.sh:801`), and this spec neither adds
a second lock nor makes the store mutable. It does _read_ prior rows, which is what
buys convergence under partial GitHub failure; append-only alone gives idempotency
but not durability of already-won t2 data.

## Cost — `price_class(model) × wall_clock`

`wall_clock_ms = last_completion_status.ts − t0`, where **completion means
`done` or `failed` only** — null otherwise, because a zombie's wall clock is
unbounded, not zero. This is deliberately _narrower_ than `crew.sh:259-260`'s
terminal set (`pr_open|done|failed|exited`): a worker that posts `pr_open` and
dies, or that the SessionEnd hook marks `exited`, never reached the end of its
run, so timing it to its last breath would understate the burn and hand a fake
`cost` to a rung that did not finish.

The **class membership** is transcribed from `dispatch-orchestration.md` → "Burn
classes". The **weights are a spec-local decision** — that document enumerates
three classes and publishes no numbers:

| class      | weight | members                                                   | provenance                    |
| ---------- | ------ | --------------------------------------------------------- | ----------------------------- |
| `free`     | 0      | `composer-2.5*`                                           | the doc annotates it "(free)" |
| `cheap`    | 1      | `haiku*`, `gpt-5.6-luna`, `cursor-grok-4.5-low-fast`      | membership from the doc       |
| `standard` | 2      | `sonnet*`, `gpt-5.6-terra`, `cursor-grok-4.5-medium-fast` | membership from the doc       |
| `premium`  | 4      | `opus*`, `gpt-5.6-sol`, `cursor-grok-4.5-high`            | membership from the doc       |
| `fable`    | 8      | `claude-fable-5`                                          | the doc's "fable ≈2× opus"    |

`fable` is a spec-local extension of the doc's own parenthetical. `free` is more
than that — it is a **regrouping**: the doc places `composer-2.5*` _inside_
`cheap` and annotates it "(free)", and this table promotes that annotation to its
own zero-weight class. Named as a regrouping so a later reader does not mistake it
for transcription. `1/2/4` is a spec-local geometric spacing, chosen because a
burn _ordering_ is all the doc supports and any spacing is a guess — this one at
least makes the ratio legible, and is stated as a choice rather than smuggled in
as transcription.

`cost_proxy = weight × wall_clock_ms` is stored in **weight·milliseconds**; the
report divides by 3_600_000 and prints **weight·hours**, which is why a two-hour
opus run reads `8.0` and not `2.88e7`. It is a **relative** signal for comparing
rungs, not dollars.

A **model the burn table does not name maps to `cost_class: null` and
`cost_proxy: null`**, and the report carries its own denominator so the gap is
visible rather than silently averaged over. Two known consequences, both
deliberate:

- `kimi-k3-high` — cursor's `deep` worker rung — **is absent from the burn-class
  table**. That is a gap in `dispatch-orchestration.md`, which this task is scoped
  out of editing, so cursor-deep rows carry a null cost until the dispatcher fills
  it in. Flagged in the PR body rather than guessed, per "do not invent a parallel
  taxonomy".
- **Effort does not multiply cost in v1.** The burn doc says effort multiplies burn
  within a rung but publishes no multipliers. `effort` is on every row, so a
  consumer can stratify by it.

## `crew rate --report`

Reads the global store, folds **last-wins by `run_id`** (the `crew roster` idiom:
`group_by | max_by`, here keyed on `swept_at`), then groups by
`(engine, model, tier)`. It performs no sweep and issues no network call. It still
runs inside a git repo, because `crew.sh:228-232` rejects every subcommand outside
one before dispatching on the subcommand — lifting that would mean editing the
shared prologue every live worker's `crew status` runs through, which is out of
scope.

**Flags:** `crew rate` sweeps; `crew rate --report` reports; `crew rate --report
--json` reports as JSON. `--json` without `--report` is an error, and the `rate)`
region rejects unknown flags rather than ignoring them as it does today — an
additive change confined to that region.

**Flag parsing must precede `crew.sh:731`'s `[ -f "$log" ] || exit 0`.** That
guard is the first statement in the `rate)` region and is correct for the sweep —
no local bus, nothing to sweep — but `--report` reads the _global_ store, so
leaving the guard in front of it makes the acceptance-criteria command print
nothing, silently, in any repo that has never dispatched. Scope the guard to the
sweep path. `--report` against a missing or empty store prints the header row and
exits 0, so "no data yet" is visibly distinct from "command did nothing".

### Column contract

Every aggregate declares its statistic, unit and null handling. `—` means
**unmeasured** and is never rendered as `0`.

| col                    | statistic                                                          | unit / denominator                       |
| ---------------------- | ------------------------------------------------------------------ | ---------------------------------------- |
| `n`                    | count of rows whose outcome is neither `running` nor `incomplete`  | the default denominator                  |
| `inc` / `run` / `pend` | counts of `incomplete` / `running` / `pr_state == "OPEN"`          | raw counts — these columns are not rates |
| `pr%`                  | share of `n` with `reached_pr`                                     | integer percent                          |
| `merge%`               | share of `pr_state ∈ {MERGED, CLOSED}` that are `MERGED`           | integer percent                          |
| `ttpr` / `ttmerge`     | **median** `time_to_pr_ms` / `time_to_merge_ms` over non-null      | humanised (`38m`, `2.1h`)                |
| `rework`               | **mean** `rework_count` over non-null                              | 1 decimal                                |
| `high`                 | **mean** `review_high` over rows with `review_mode ∉ {none, null}` | 1 decimal                                |
| `rounds`               | **mean** `review_rounds` over non-null                             | 1 decimal                                |
| `blocked`              | **mean** `blocked_count + watchdog_blocked_count` over `n`         | 1 decimal — how often this rung wedges   |
| `ci1`                  | share of non-null `first_ci_green` that are true                   | integer percent                          |
| `notes`                | **mean** `unresolved_notes` over non-null                          | 1 decimal                                |
| `rev`                  | count of `reverted == true`                                        | raw count                                |
| `cost`                 | **mean** `cost_proxy` over non-null                                | weight·hours, 1 decimal                  |

**Which rows leave `n`, and which only leave a column.** `inc` and `run` count
rows that are _outside_ `n` and therefore outside every denominator. `pend` and
`rev` count rows that are **inside** `n` — an `OPEN`-PR run has outcome `pr_open`,
so it contributes to `pr%`, `ttpr`, `rework`, `blocked` and `cost` like any other,
and is excluded only from `merge%` and `ttmerge`. Reading "excluded from every
rate" onto `pend` would shift every column in the table.

### Making small samples impossible to miss

The task's stated failure mode is "a mean over 2 runs next to a mean over 40
without saying so". A single per-row `n` marker does not prevent that: a cell with
`n 12` whose `high` mean is over 2 measured rows reproduces the failure _inside a
row that looks well-populated_. So the marker attaches to the **quantity**:

1. An aggregate renders as `value` when its own denominator equals `n`, and as
   `value(k)` when it does not — a shrunken denominator is never invisible.
2. Any aggregate whose own denominator is `< 5` takes a `!` suffix. `n` itself
   takes `!` when `n < 5`; raw counts (`inc`, `run`, `pend`, `rev`) never do.
3. A legend states the threshold in words; a footer counts the affected rows.

```
engine  model   tier       n  inc  run  pend   pr%  merge%  ttpr   ttmerge   rework     high  rounds  blocked    ci1  notes  rev  cost
claude  opus    deep      12    1    1     2   100  90(10)   38m   2.1h(9)  0.6(11)  0.5(2)!     1.1      0.3  89(9)    0.2    0   4.2
claude  sonnet  standard  3!    0    1     0  100!     67!  21m!  1.5h(2)!     1.3!     0.7!    0.7!     0.0!    67!   0.7!    0  1.1!
codex   sol     deep      1!    0    0     1  100!       —  55m!         —     2.0!        —    1.0!     1.0!   100!   3.0!    0  6.0!

! own sample < 5 — anecdote, not evidence.   value(k) = measured over k of n runs.   — = unmeasured.
3 of 3 rows carry at least one small-sample quantity.
```

`ttmerge`'s row-2 value renders `1.5h(2)!`, not the `1.4h(2)!` an earlier hand-typed
draft of this block showed. Design decision 5's threshold (`< 90m → m, else h`) makes
`X.4h` for any `X` unreachable by construction: the smallest duration that clears the
90-minute gate is exactly 90m = 1.5h, and `fmt1` rounds to the nearest 0.1, so the
lowest displayable hour reading is `1.5h`. `1.4h` was never producible from a real
`time_to_merge_ms` pair; `1.5h` is.

Reading row 1, since this example is the golden fixture the report test encodes:
14 runs — one died (`inc`), one is in flight (`run`) — so `n` is 12. All 12 were
queried successfully, so `rounds`, `notes` and `blocked` have denominator 12 and
carry no `(k)`. Two PRs are still open, leaving 10 settled, of which 9 merged →
`90(10)`; `ttmerge` is over those 9. One run died before emitting metrics, so
`rework` is over 11. Only 2 ran a Claude review gate (the rest were `downgraded`
or `none`), so `high` is a mean over 2 — the exact case rule 1 exists for, and the
one an `n 12` row would otherwise hide. (Every value here is reachable from
integer inputs: `review_high` is an int, so a mean over `k=2` can only end in `.0`
or `.5`. Values in this block are pinned to what a real fixture can produce.) Three runs had no Actions runs inside their
window, so `ci1` is over 9. Rows 2 and 3 have `n < 5`, so every aggregate in them
takes `!`, `(k)` and all.

`--json` emits every column plus the raw fields. Each aggregate is
`{value, k, n}` — **not** `{value, n}`: `k` is the aggregate's own denominator and
is the quantity rule 2's `!` threshold reads, while `n` is the cell's row count,
and one field cannot carry both. Raw counts are plain numbers. This is the
artifact #86 consumes; the table is for a human.

**Legacy rows.** The store may already hold phase-1 rows with no t2 fields and the
pre-`running` outcome vocabulary. Migration is a task non-goal, so those rows sit
in `n` with every t2 denominator shrunken — which the `value(k)` marker surfaces
rather than hides. Re-sweeping their repo upgrades them.

## Scope boundary

Touched: `adapters/core/crew.sh` (the `rate)` region only — `msg`, `reap`, `id`,
`_crew_id`, `register` and the shared prologue are untouched; workers are live in
all of them), a new `tests/rate.bats`, this spec. `scripts/gen-adapters.sh` reruns
only if a protocol file changes — none is expected to.

**`WORKER_PROTOCOL.md` needs no change.** Its `metrics:` emit is already all-tier,
and the issue's remaining worker-stamped fields (`reached_pr`, `time_to_pr_ms`,
`blocked_count`, `protocol_ok`) are all sweep-derived by design — see Q1 and Q4.
Asking the worker to stamp them would duplicate bus data the sweep already reads,
and would make a dead worker's row worse, not better.

**Nothing in scope causes `crew rate` to run.** v1 ships with **no automatic
caller**: `DISPATCHER_PROTOCOL.md` is out of scope and carries no `crew rate`
reference, so the sweep is a human/dispatcher-typed command until a follow-up
wires it in.

## Testing

`tests/rate.bats`, synthetic `events.jsonl` + a PATH-shim `gh`:

- **The four pre-existing `crew rate` tests in `tests/crew.bats` pass unmodified** —
  that file is off-limits. This is why the network gate is a URL-shape match:
  their fixture `pr_url` is `https://example.com/pr/1`, not a GitHub PR URL, so
  they issue no calls.
- **Idempotency** — sweep twice; the store is byte-identical after the second
  sweep and the folded projection is unchanged.
- **Last-wins** — a stale row followed by a fresh one for the same `run_id` folds
  to the fresh one.
- **t1 → t2 upgrade** — sweep with an `OPEN` PR, re-sweep with `MERGED`; the first
  row is non-terminal, the second carries `merged` and `time_to_merge_ms`.
- **Per-call finality** — a stored `MERGED` row issues no `gh pr view` but _does_
  issue the Actions call while `first_ci_green` is null; a `CLOSED` row inside 30d
  re-queries and outside 30d does not (assert via the stub's call log).
- **Merge-forward** — reconcile once, then sweep with a `gh` that exits non-zero;
  stored t2 survives, `last_query_ok` is `false`, and `pr_state` is unchanged.
- **Branch reuse** — two dispatches on one branch sharing a `pr_url`; the earlier
  run has `owns_pr: true` and the t2 fields, the later has `owns_pr: false`, t2
  null and `time_to_pr_ms: null` so it cannot enter the `ttpr` median; the windowed
  Actions filter keeps run 1's CI out of run 2.
- **`--report` with no local bus and an empty store** — in a repo that never
  dispatched, `--report` still prints its header and exits 0 rather than being
  short-circuited by the sweep's `[ -f "$log" ]` guard.
- **The sample table is the golden fixture** — assert the rendered rows
  byte-for-byte against the example above, so the marker rules cannot drift from
  the document that defines them.
- **Call targeting** — two runs whose `pr_url`s point at different repos; assert
  each call carries that run's own `owner/name`, and that `gh pr view` is never
  invoked without a URL argument.
- **`review_rounds` batching** — 3 reviews then 1 commit ⇒ `1`; `APPROVED` ⇒ not a
  round; reviews after the last commit ⇒ `0`.
- **`first_ci_green`** — earliest-sha grouping; mixed conclusions on the first sha
  ⇒ `false`; no runs in the window ⇒ `null`.
- **`unresolved_notes`** — threads with mixed `isResolved`; failure ⇒ `null`.
- **`reverted`** — merge commit absent from local history ⇒ `null`, not `false`.
- **`outcome` precedence** — one fixture per row of the table, including
  `done`-with-no-PR ⇒ `failed` and a non-owning run ⇒ `pr_open`.
- **Cost** — known model ⇒ weight × wall clock; `kimi-k3-high` ⇒ null class and
  null proxy.
- **Report** — the `!` fires on a quantity whose own denominator is small even when
  `n` is large; `(k)` appears exactly when the denominator differs from `n`;
  unmeasured `review_high` is excluded from the mean rather than counted as 0;
  `running` and `incomplete` are out of the denominators; `--json` without
  `--report` errors.
- **The metrics emit does not wake `crew watch`** — park a watch, emit a
  `metrics:<crew>` msg, assert empty stdout at timeout. `watch`'s filter is
  `.to == "dispatcher:<crew>"` or `"*"`, so this is the property that makes the
  whole scheme free; it is asserted directly rather than inferred from the existing
  `inbox` test.

## Follow-ups (filed, not done here)

- **Real token accounting.** It does _not_ fall out free even for Claude: the
  worker transcript is per-session JSONL under the Claude projects dir keyed by
  cwd, and joining it to a run means matching worktree path plus session window —
  a real spike, and codex/cursor have no comparable local artifact.
- `--shape` capture (a `dispatch` flag + a `DISPATCHER_PROTOCOL.md` instruction) →
  shape-aware stratification. The data is already being captured.
- `kimi-k3-high` (and any future non-grok cursor rung) into the burn-class table.
- Effort burn multipliers, once the burn doc publishes them.
- `pr_role` (opener vs finisher) if re-dispatch onto a live PR becomes common.
- An automatic caller for the sweep (dispatcher watch-completion handler), and with
  it the cross-repo finalization gap: `crew rate` sweeps one repo, so a run in a
  repo never revisited is never finalized. No daemon is an explicit non-goal.
- #86 consumes `--report --json`.
