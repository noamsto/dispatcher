# Crew discovery — rediscover a crew after a dispatcher restart

**Issue:** #29
**Artifacts touched:** `adapters/core/crew.sh` (`crew id`, `_crew_id`, `crew register`
refusal message, new `crews` / `new` / `adopt` subcommands, usage line),
`adapters/core/dispatch.sh` (the "no crew id" abort message — one `echo`, plus the
`SC2016` directive it retires), `adapters/core/dispatcher.sh` (one call site,
`crew id` → `crew new`, plus the discovery notice of §4.1),
`adapters/core/commands/dispatcher.md` (the mint step becomes a discover-then-attach
step), regenerated adapter trees via `scripts/gen-adapters.sh`, a new
`tests/crews.bats`, and a two-test deletion in `tests/crew.bats` (see §7).

## 1. The problem

`CREW_ID` lives only in a session's process env. When a dispatcher session dies and
restarts, the id is gone — but the crew is not: the bus is at
`$git_common_dir/crew/events.jsonl` and registered crews at
`$git_common_dir/crew/crews/<id>/pid`. Nothing surfaces either.

Three behaviours compose into a footgun:

1. `dispatch` aborts with `no crew id — pass --crew-id <id> …`. It asks for an id and
   does not say where to get one.
2. `crew id` is named like an accessor and implemented as a generator:
   `printf '%s\n' "${CREW_ID:-$(date +%s)-$$}"` (crew.sh:205). With `CREW_ID` unset it
   mints a fresh `<epoch>-<pid>` **per invocation** — two consecutive calls return two
   different ids.
3. `crew register` refuses with `CREW_ID unset and no WORKER_TASK.md crew_id`, which
   also names no recovery.

The composition matters more than any one part. The abort asks for an id; the
obviously-named command hands you a _new_ one; you are now a second crew. In-flight
workers keep posting to the old crew id (they resolve it from `WORKER_TASK.md`, which
persisted), so `roster`/`watch`/`await` report an empty crew and _nothing errors_.
That is a split-brain crew presenting as "my workers died".

**The property to eliminate: the safe-looking read is the destructive one.**

## 2. Goals / non-goals

**Goals**

- A discovery primitive that lists this repo's on-disk crews from **both** sources.
- `crew id` never creates state as a side effect of being read.
- The two dead-end error messages name the command that unblocks you.
- A restarted dispatcher recovers using only printed messages — no source reading.

**Non-goals**

- Persisting `CREW_ID` outside the process env (a "current crew" pointer file). N crews
  legitimately share a repo; a single pointer would be wrong for the second dispatcher
  and is a larger design than the bug requires.
- Garbage-collecting dead crews. `crews` reports liveness; reclaiming is #49's ground.
- Unifying the `CREW_ID unset` refusal across every subcommand (see §7, scope).

## 3. Design decision: `crew id` stops minting; `crew new` creates

The issue offers two shapes. Both are stated and one is chosen.

**Option A — bare `crew id` prefers the newest live crew for the repo.**
The read becomes non-destructive, and no call site changes. But it is still _silent_,
and now silently **wrong** in the case the codebase explicitly supports: N crews per
repo (crew.sh:400-407). A second dispatcher in the same repo, or a worker's stray call,
would attach to whichever crew sorted newest — re-introducing split-brain in the
opposite direction, and this time without even a fresh id to notice. It trades a
visible wrong answer for an invisible one.

**Option B (chosen) — creation moves behind `crew new`; bare `crew id` reports or
fails.** Every operation is named for what it does: `id` reads, `new` creates, `adopt`
re-attaches. Reads never write. The failure is loud and directive, which is the whole
point of the fix.

Option B's cost is a contract change on `crew id`, so §4 enumerates every call site.
That cost is bounded and one-time; Option A's cost is unbounded and recurring.

**New contract:**

| invocation                                            | behaviour                                                                   |
| ----------------------------------------------------- | --------------------------------------------------------------------------- |
| `crew id`, `CREW_ID` set                              | prints `$CREW_ID` — **unchanged**                                           |
| `crew id`, unset, in a worktree with `WORKER_TASK.md` | prints its `crew_id:` — **new** (it previously minted, ignoring the file)   |
| `crew id`, unset, nothing to report                   | exits 1 with a directive message — **changed from minting**                 |
| `crew new`                                            | mints `<epoch>-<pid>` and prints it — the old bare-`id` behaviour, verbatim |

`crew id` is re-pointed at the existing `_crew_id` helper, which is already the
env → `WORKER_TASK.md` resolver every other subcommand uses. So the accessor and the
resolver stop disagreeing, and a worker can now run `crew id` in its own worktree and
get its real crew. It still newline-terminates, via `id=$(_crew_id); printf '%s\n' "$id"`
— capture then print, so the shape every `$(crew id)` consumer sees today is preserved.
Note `_crew_id` is _inconsistent_ about the trailing newline (the env branch uses
`printf '%s'`, the `WORKER_TASK.md` branch ends in `grep … | cut`, which emits one), so
the capture is what normalises it; `_crew_id; printf '\n'` would emit a blank line on
the worker path.

**Known sharp edge, accepted:** `_crew_id` (crew.sh:188-198) has no ownership check, so
in a checkout whose root holds a stale or foreign `WORKER_TASK.md`, bare `crew id` now
prints that file's crew rather than failing. This is strictly better than today (it
prints a _real_ crew instead of minting a phantom one), it is the same resolution every
other subcommand already performs, and the operator sees the id and can check it against
`crew crews`. Adding an ownership check is a separate change to a helper three live
workers depend on.

`crew new` must keep working **outside a git repo**: `dispatcher.sh:102` mints before
its `git rev-parse` guard, deliberately. So `new` is handled in the pre-repo block
alongside `id` and `identity` (crew.sh:204-224, above the `--git-common-dir` guard at
228-232), and it is a pure mint — it does **not** register. `crews` and `adopt` do read
the crew dir, so they sit below that guard and inherit the existing `not in a git repo`
refusal. Registration stays a separate, repo-requiring step, exactly as today.

## 4. Call sites of `crew id` (the blast radius)

Every live call site in this repo, and what happens to it:

| site                                                                                                                                               | current                          | after                                                                                               |
| -------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------- | --------------------------------------------------------------------------------------------------- |
| `adapters/core/dispatcher.sh:102` — `: "${CREW_ID:=$(crew id)}"` (launcher mint)                                                                   | mints when unset                 | → `crew new`, **plus a discovery notice** (below)                                                   |
| `adapters/core/commands/dispatcher.md:9` — `export CREW_ID=$(crew id); crew register $PPID` (in-session `/dispatcher`)                             | mints                            | → discover-first block (§6); mints via `crew new` only when there is nothing to adopt               |
| `adapters/claude-code/plugin/commands/dispatcher.md`, `adapters/cursor/commands/dispatcher.md`, `adapters/codex/plugin/skills/dispatcher/SKILL.md` | generated copies of the above    | regenerated by `scripts/gen-adapters.sh`                                                            |
| `tests/crew.bats:34` — `id: honours CREW_ID when set`                                                                                              | passes                           | unchanged, still passes                                                                             |
| `tests/crew.bats:40` — `id: mints … when CREW_ID is unset`                                                                                         | passes                           | **deleted** — asserts the removed behaviour (§7)                                                    |
| `tests/crew.bats:46` — `id: works outside a git repo`                                                                                              | passes                           | **deleted** — it exercises the unset-mint path; its intent moves to `new: works outside a git repo` |
| `tests/dispatcher.bats:48` — `prints the crew id it minted`                                                                                        | passes `CREW_ID=…`, stubs `crew` | unchanged, still passes                                                                             |

Nothing outside this repo consumes `crew id`: `README.md`,
`adapters/core/protocols/*.md`, `adapters/cursor/rules/dispatcher.mdc` and
`nix/hm-module.nix` contain no reference to it (verified by grep).

The **runtime** path — a launched dispatcher, every `dispatch`, every worker — always
has `CREW_ID` set or a `WORKER_TASK.md`, and is therefore untouched by the change. Only
the two mint sites are real, and both are in this repo.

### 4.1 The launcher must not restart into a silent new crew

Swapping `crew id` → `crew new` in `dispatcher.sh` is behaviour-preserving, and
behaviour-preserving is not good enough here: relaunching via the `dispatcher` launcher
is at least as likely a restart path as `/dispatcher`, and on it the operator sees only
`crew id: <brand-new-id>` — no error to read, no hint, workers still posting to the old
crew. The acceptance criterion "recovers using only the messages the tools print" would
be met on one entrance and unmet on the other.

So when the launcher **minted** an id (i.e. `CREW_ID` was unset) and this repo already
holds other crews, it prints one line to stderr after registering:

```
dispatcher: minted a NEW crew; this repo has 2 other(s) — 'crew crews' lists them; to
re-attach instead, relaunch as: CREW_ID=<id> dispatcher …
```

**The remedy named must be the one that works on this entrance.** `crew adopt` is the
wrong advice here and saying it would be worse than saying nothing: `dispatcher.sh:102`
has already exported `CREW_ID=<new>` into the launched agent's process env before the
notice can be read, and launcher sessions resolve their crew from exactly that env
(`DISPATCHER_PROTOCOL.md:122`). `crew adopt` rewrites `crews/<id>/pid` and prints an
`export` line into a shell that is _not_ the agent's — so the agent would keep watching
the new, empty crew and the split-brain would survive the fix meant to close it. The
env-prefixed relaunch is the remedy that actually re-attaches, and it needs no new code:
`: "${CREW_ID:=$(crew new)}"` already honours an inherited id.

It goes inside the existing `if git rev-parse --git-common-dir` guard (`crews` needs a
repo), is suppressed when `CREW_ID` was inherited, and is `|| true`-tolerant: a
discovery hiccup must never take down a launcher under `set -euo pipefail`
(`writeShellApplication` prepends it — flake.nix:122-126).

**"Other" must exclude the crew just minted.** `dispatcher.sh:107` runs `crew register

$$
` *before* this notice, so the fresh crew already owns a `crews/<id>/` dir and is
itself a row in `crew crews`. A naive row count returns 1 on a virgin repo and every
ordinary first launch would print `this repo has 1 other(s)` — a false alarm on the
happy path, which is how a warning gets trained out of a reader. The count is
`crew crews` rows whose id `!= $CREW_ID`, and §8 pins the negative case: minted with no
other crews on disk prints nothing.

## 5. `crew crews` — the discovery primitive

`crew crews` lists this repo's crews as a TSV table with a header, matching `crew
report`'s precedent (crew.sh:713).

```
crew_id            last_event_s  first_event_s  workers  pid    alive
1785951264-84289   412           88301          3        84289  yes
1785954148-64470   —             —              0        64470  no
1785900001-11111   9422          9500           1        —      —
```

Columns are **tab**-separated (`@tsv`, as `crew report` does at crew.sh:713-724); the
sample above is shown space-aligned only for readability, so no test may assert against
that literal block.

**Union of two sources, because neither is complete:**

- `crews/*/` — a crew with a crew dir. Note this is **not** the same as "a crew that
  registered": `crew watch` also does `mkdir -p "$cdir"` (crew.sh:502-503) before any
  registration, so a dir may hold only `cursor` and `watch.lock.d` and no `pid`. Every
  read of `$cdir/pid` is therefore guarded (`cat … 2>/dev/null || true`) — an unguarded
  read dies under `set -euo pipefail` — and `adopt`'s liveness guard treats a pid-less
  dir as not-live. A crew created by `--crew-id` alone has no dir at all.
- distinct `.crew_id` in `events.jsonl` — a crew that posted. A crew that registered
  and died before its first event is absent here.

**The log source must filter crew-less events.** Not every bus line carries a
`crew_id`: `reap` (crew.sh:1447-1449) and `release` (crew.sh:1327-1330) both emit
`jq -nc` objects with no such key. A naive `map(.crew_id) | unique` therefore yields a
`null` row, so any repo that has ever run `crew reap` would list a phantom crew — in the
primary output of the primary deliverable. The extraction is
`select(.crew_id != null and .crew_id != "")`, and `tests/crews.bats` seeds a `reap`
line and asserts no extra row appears.

Rows are deduplicated by id and sorted newest-activity first; a crew with no events
sorts last (it has no activity to order by).

**Columns**

- `last_event_s` / `first_event_s` — seconds since that crew's newest / oldest bus
  event. `—` when the crew has no events. Derived from the log, not from file mtimes:
  `stat` flags differ between BSD and GNU, and the repo targets both.
- `workers` — distinct worker branches that posted status under that crew.
- `pid` — the registered dispatcher pid, or `—` when never registered.
- `alive` — `kill -0` on that pid: `yes` / `no`, `—` when there is no pid. **The pid is
  probed only when it is a positive integer**; anything else reads `no` without a
  syscall. `kill -0 0` signals the caller's *own* process group and `kill -0 -1`
  broadcasts, so both all but always succeed — a `0` in a pid file (a truncated or
  hand-edited write) would otherwise report every dead crew as `alive: yes`, and make
  `adopt` refuse it forever. The same rule governs `adopt`'s liveness guard.

  Even for a well-formed pid this is a hint, not proof, in two directions: pids are
  reused, so a `yes` can name an unrelated process; and `kill -0` returns EPERM for a
  live process owned by another user, which reads as `no`. Both are documented rather
  than papered over — a correct answer needs a process start-time comparison, and `ps`
  flags for that differ between BSD and GNU.

`crews` takes no crew id — it is the command you run *because* you have none. It needs
a repo (it reads `$git_common_dir/crew`) and exits 0 with just the header when there is
nothing to list.

**All three empty cases are guarded, not just the obvious one.** The crew dir may be
missing; it may exist with no `events.jsonl` (a registered crew that died before posting,
or a `watch`-created dir); and `events.jsonl` may exist with no `crews/` at all (the
`--crew-id`-only crew this feature is for). An unguarded `jq … "$log"` on a missing file
dies under `set -euo pipefail` — the same hazard already handled for `cat …/pid` above —
so each source is read only when its file exists and contributes an empty set otherwise.

**A malformed `events.jsonl` must degrade, not abort.** Existing-file is not the only
way a `jq` read fails: a hard kill mid-append leaves a torn trailing line, which is
precisely the crash this command exists to recover from. A failing command substitution
*does* trip errexit when it is the whole right-hand side of an assignment
(`bash -euo pipefail -c 'x="a$(false)"'` exits 1), so an unguarded read would print the
header, dump a jq parse error and exit non-zero — losing every well-formed crew above
the torn line. Both reads are therefore best-effort, matching the sibling read sites in
`crew.sh`: the id scan appends `2>/dev/null || true` (jq streams, so the ids it already
emitted survive), and the stats pass — `jq -s`, which is all-or-nothing — falls back to
`|| echo '{}'`, costing the age and worker columns but never the table.

Ordering **among** no-event crews is undefined (they have no activity to sort by), so
`tests/crews.bats` asserts row presence and field values, never full-table row order.

It deliberately does **not** mark "the current crew": in the situation `crews` exists to
serve, `CREW_ID` is unset by definition, so the column would be empty exactly when it is
read.

## 6. `crew adopt <id>` — re-attach in one step

It falls out of §5 cleanly, so it is included.

```
$ crew adopt 1785951264-84289 $PPID
1785951264-84289
```

**Signature: `crew adopt [--force] <id> [pid]`**, refusing with
`crew: adopt [--force] <id> [pid]` when `<id>` is missing — the missing-argument guard
every other subcommand in `crew.sh` has.

**Argument parsing, stated because the obvious copy is wrong.** After `sub="$1"; shift`,
the id is `$1` and the pid is **`${2:-$PPID}`** — *not* `${1:-$PPID}`. That latter form
is the correct one in `crew register` (crew.sh:416), which takes only a pid; lifted
into `adopt` it writes the **crew id** into `crews/<id>/pid`, and both the `alive`
column and `adopt`'s own liveness guard then read garbage. `--force` is stripped from
the argument list wherever it appears *before* the positionals are read, so
`crew adopt <id> --force` cannot land the literal string `--force` in the pid file;
`tests/crews.bats` covers that ordering. The optional `pid` is
not decoration: `adapters/core/commands/dispatcher.md:9` passes `$PPID` *because* the
shell inside a Claude Code Bash call is throwaway and `$PPID` is the long-lived agent
process. A standalone `crew adopt <id>` — which is what both new error messages tell the
operator to run — would otherwise record that throwaway shell, and the crew it just
adopted would report `alive: no` on the very next `crew crews`.

**Output contract: the bare id on stdout and nothing else** — the same contract as
`crew id` and `crew new`, so all three are consumed the same way:
`CREW_ID=$(crew adopt <id> $PPID) && export CREW_ID`.

It deliberately does **not** print `export CREW_ID=<id>` for the caller to `eval`. The id
is caller-supplied — `dispatch --crew-id` accepts any string and writes it unsanitised
into the shared `events.jsonl`, which is `adopt`'s own "is this id known" source. An id
of `x; touch /tmp/PWNED` present in that log therefore makes `adopt` print
`export CREW_ID=x; touch /tmp/PWNED`, and the documented `eval` executes it: a shell
injection through a file any worker can append to. The fix is not to quote the export
line but to delete the eval-shaped API — a value that is never interpreted as shell
cannot be an injection. Printing a bare id also removes the need for the whole
"use the capture form, never `eval \"$(…)\"`" argument: a plain assignment already
propagates the exit status, so a refusal stops the `&&` chain by construction.

**The id is also validated before it becomes a path.** `adopt` builds
`crews/<id>/` and `mkdir -p`s it, so an id containing `/` or `..` writes outside the bus
dir. Any id that is not `[A-Za-z0-9._-]+`, or that starts with `-` (read as a flag by
anything that later interpolates it), or that is exactly `.` or `..` is refused:

```
crew: invalid crew id — expected only letters, digits, '.', '_' and '-'
```

This is a character-class check, **not** an `<epoch>-<pid>` shape check: ids like `c1`
are legitimate — `--crew-id` has always accepted them and the tests use them — so
enforcing the mint shape would reject valid crews. The check is a `case` glob
(`*[!A-Za-z0-9._-]*|-*|.|..`), matching this file's style rather than importing a bash
regex.

**Two further guards, both load-bearing:**

1. **No on-disk trace** — no `crews/<id>/` dir *and* no event with that `crew_id`:

   ```
   crew: no crew '<id>' in this repo — run 'crew crews' to list them, or 'crew new' to start one
   ```

   Without it, `crew adopt <typo>` silently registers an empty crew: a second way to
   mint, which is precisely the footgun being removed.

2. **The crew's registered pid is still alive and is not one of ours** — refuse, unless
   `--force`:

   ```
   crew: crew '<id>' still has a live dispatcher — 'crew register <pid>' re-registers a
   crew that is already yours; 'crew new' starts your own; '--force' overrides if that
   process is a stale pid reuse
   ```

   `crew watch` takes a per-crew exclusive lock and shares a per-crew cursor
   (crew.sh:503-514: `cursor_file="$cdir/cursor"`, `_lock_acquire "$wlock"` →
   `another watch is already running for this crew`). Two dispatchers on one crew id
   therefore wedge each other's watch loop, and the loser inherits a cursor that has
   already consumed its events. Adopting is a **recovery** operation: the crew worth
   adopting is one whose dispatcher is *dead*.

   **A session must still be able to re-attach to its own crew.** A guard that refuses
   *every* live pid breaks the primary recovery story twice over: re-running the
   documented adopt is an error, and a dispatcher that re-runs `/dispatcher` in the same
   session is pushed into minting a *new* crew while its own workers keep posting to the
   old one — the exact split-brain this feature closes. So the refusal fires only when
   the live pid is **not an ancestor of the calling process**. Ancestry is walked up from
   `$$` via `ps -o ppid= -p <pid>` (the one parent-of spelling identical on BSD and GNU),
   bounded against a cyclic chain, whitespace-trimmed, and tolerant of failure under
   `set -euo pipefail`.

   Ancestry is the right proof because it is one a different session **cannot fake**. The
   rejected alternative — trusting a caller-supplied pid as "same pid is idempotent" —
   would let anyone copy a number out of the error message and bypass the guard, turning
   the error text into the bypass recipe. That is why **the refusal deliberately does not
   print the live pid** either. Both properties are the same "the safe-looking action is
   the destructive one" defect §1 exists to eliminate, kept out of the guard meant to
   prevent it.

   `--force` is the escape hatch for the one case the guard genuinely gets wrong: `alive`
   is a `kill -0` hint, and pid reuse can make a dead crew read live (§5). It is an
   explicit flag precisely so the destructive option *looks* destructive — which is the
   distinction between it and the rejected idempotency carve-out.

   This guard is why §3's rejection of Option A does not just reappear in `adopt`'s
   clothing: Option A picks a crew *silently*, from a set that includes live ones;
   `adopt` requires an explicit id and mechanically refuses the dangerous half of that
   set.

The `/dispatcher` command body changes from an unconditional mint to a discover-first
block that ends, on both branches, with the same `echo` it has today:

1. `crew crews` — list this repo's crews.
2. If a row is recognisably this session's work (newest `last_event_s`, matching
   branches), re-attach:
   `CREW_ID=$(crew adopt <id> $PPID) && export CREW_ID && echo "crew id: $CREW_ID"`
3. Otherwise — no crews, or none of them is yours:
   `export CREW_ID=$(crew new); crew register $PPID; echo "crew id: $CREW_ID"`

**Step 2 does not ask the agent to judge the `alive` column.** An instruction to "adopt
only rows that are `alive: no` or `—`" is both weaker and more dangerous than the tool's
own guard: the agent cannot tell *whose* live dispatcher a row names, so on its own
session's still-live crew it would skip to step 3 and mint — reintroducing the
split-brain. `adopt` is the arbiter instead: it succeeds for a dead crew and for one
whose pid is an ancestor of this session, and refuses another live dispatcher's, in
which case the agent falls through to step 3. Complexity pulled into the tool, not the
instructions.

**It does not re-run `crew register`:** `adopt` already wrote `crews/<id>/pid`, so a
second call is a duplicate with a second chance to disagree about the pid.

The terminal `echo` is not optional: lines 17-21 of that file tell the agent to note the
literal id printed there, and lines 35-38 and 63-64 tell it to substitute that literal
into every later `dispatch --crew-id` and `CREW_ID=<id> crew …` call, because each Bash
call is a fresh shell. Dropping the echo would leave an in-session dispatcher with no id
for the rest of the session. Those three surrounding paragraphs are otherwise unchanged.

## 7. Error messages, and what is deliberately left alone

**Every message spells the pid argument.** All three strings below say
`crew adopt <id> $PPID`, never bare `crew adopt <id>`. Adding an optional `[pid]` in §6
does not change the default, and the default is wrong in the environment these messages
are read in: inside an agent's Bash call `$PPID` is the long-lived agent process while
the shell itself is throwaway — which is exactly why `commands/dispatcher.md:9` already
writes `crew register $PPID` rather than bare `crew register`. A message that omits it
hands the operator a crew that reports `alive: no` on the very next `crew crews`. At an
interactive shell the form degrades correctly: fish has no `$PPID`, so it expands to
empty and the pid default falls back to the shell itself, which is the right answer
there.

**`$PPID` must reach the terminal unexpanded**, or the instruction becomes the bug it
exists to prevent. `crew.sh:410` and the new bare-`crew id` error are double-quoted
`echo`s, where `$PPID` would expand at print time to the pid of the shell that invoked
`crew` — the throwaway one — printing `crew adopt <id> 48213` and recording exactly the
pid B1 was raised about. Each of the three messages therefore uses single quotes, or
`\$PPID` where the surrounding string must stay double-quoted.

That also means **the `SC2016` directive on dispatch.sh:172 stays** — §7 earlier claimed
dropping the literal `$CREW_ID` retires it, and that is wrong: SC2016 fires on *any*
`$name` inside single quotes, and the replacement string still contains `$PPID`.
Confirmed by running `shellcheck` on the replacement text. Removing the directive would
fail this spec's own "shellcheck clean" gate.

**`dispatch`** (dispatch.sh:173):

```
dispatch: no crew id — run 'crew crews' to find this repo's crews and
'crew adopt <id> $PPID' to re-attach, or 'crew new' to start one; then pass
--crew-id <id> or export CREW_ID
```

**`crew register`** (crew.sh:410):

```
crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this
repo's crews, 'crew adopt <id> $PPID' to re-attach, or 'crew new' to start one
```

**Bare `crew id` with nothing to report** — the message the incident's operator actually
reaches, since `dispatch` aborts and `crew id` is the obvious next keystroke. It gets
the same text, and `tests/crews.bats` asserts on it:

```
crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; run 'crew crews' to find
this repo's crews, 'crew adopt <id> $PPID' to re-attach, or 'crew new' to start one
```

The `CREW_ID unset` clause is kept in both for continuity with the wording operators
already see. Note this is a *self-imposed* consistency, not a test constraint: the
existing `*"CREW_ID unset"*` assertion at `tests/crew.bats:112` belongs to
`@test "status: fails when no crew id can be resolved"` and guards the **`status`** arm,
which this change leaves alone. No test asserts `crew register`'s refusal text today;
`tests/crews.bats` adds one.

**Deliberately unchanged:** the identical refusal in the `status | msg`, `reply`, and
`watch` arms. The issue names exactly two messages, and `crew msg` is being edited by a
live worker (#46). Those three arms keep the old wording; unifying them is a follow-up,
noted in the PR body.

**The two `tests/crew.bats` deletions** are unavoidable, not scope creep: both tests
assert the exact behaviour the issue requires removing, and the gate requires bats
green. They sit at lines 40-50, well away from the `msg` / `reap` / `rate` regions the
live workers are editing. Every replacement lands in the new `tests/crews.bats`.

`WORKER_TASK.md` fences that file, so the deletion is **posted to the dispatcher on the
bus as a question, not an announcement** — whether to touch a file three live workers
hold is the dispatcher's call. It is not, however, a *blocking* question, and the worker
does not stop dead on it: the same task doc requires both "bare `crew id` no longer
mints" and a green `bats tests/`, and those two demands **jointly entail** deleting a
test that asserts minting — there is no third option, since even skipping the tests
edits the same file. The dispatcher has therefore already authorised the class of edit;
what it may still want is to warn #46/#48/#49 or to make the deletion itself. So
implementation proceeds, the reply is collected at the **post-execute checkpoint-peek**
(before the review gate, so a revert still costs nothing), and the deletion is kept to a
**single contiguous hunk at `tests/crew.bats:40-50`** so reverting it is one mechanical
operation rather than an aspiration. Called out in the PR body either way.

## 8. Acceptance criteria → verification

| criterion | verified by |
| --- | --- |
| `crews` lists both sources, including a `--crew-id`-only crew | `tests/crews.bats`: events-only crew, pid-dir-only crew, and a crew in both appearing exactly once |
| `crews` does not invent a phantom crew from crew-less events | `tests/crews.bats`: a seeded `reap` line adds no row |
| `crews` reports pid liveness | `tests/crews.bats`: a live pid (`$$`) reads `yes`, an unused one `no`, none `—` |
| bare `crew id` no longer mints; two consecutive calls do not differ | `tests/crews.bats`: both calls exit 1 with identical (empty) stdout |
| bare `crew id` says how to get unstuck | `tests/crews.bats` asserts `crew crews` / `crew adopt` / `crew new` all appear in its stderr |
| every existing `crew id` call site still works | §4 table; regression tests for `CREW_ID` set, `WORKER_TASK.md` resolution, `new` minting, `new` outside a repo, and the launcher minting via a stubbed `crew` |
| both messages name the unblocking command | `tests/crews.bats` asserts on `dispatch.sh`'s abort and `crew register`'s refusal |
| the launcher restart path is not silent, and names a remedy that works | `tests/crews.bats`: minting with other crews on disk prints a notice naming the `CREW_ID=<id>` relaunch; **minting with no other crews prints nothing**; inheriting `CREW_ID` prints nothing; `dispatcher.bats:48` already pins that an inherited id is honoured |
| every printed recovery instruction is runnable as printed | `tests/crews.bats`: all three messages contain `crew adopt <id> $PPID`, not bare `crew adopt <id>` |
| `adopt` cannot become a second way to mint | `tests/crews.bats`: unknown id refused; another session's live-pid crew refused; the refusal does not leak the pid; `--force` overrides; events-only crew adoptable |
| `adopt` prints a value that is never shell | `tests/crews.bats`: output is the bare id, with no `export` prefix; an injection-shaped id is refused outright |
| a crew id can never escape the bus dir | `tests/crews.bats`: a `../`-traversal id is refused and writes nothing outside `crews/` |
| a session can re-adopt its own live crew | `tests/crews.bats`: adopting a crew registered to an ancestor pid succeeds |
| a torn `events.jsonl` does not cost the table | `tests/crews.bats`: a truncated trailing line still lists the well-formed crews and exits 0 |
| a non-positive pid never reads as alive | `tests/crews.bats`: a pid file of `0` reads `alive: no` and stays adoptable |
| restart → re-attached from printed messages alone | literal transcript in the PR body |
| `shellcheck` clean; `bats tests/` green | the gate, minus the two known-red reap-release tests in `tests/crew.bats` (#54, #58, owned by #49) |

The launcher and `dispatch` rows need a **richer `crew` stub than `stub_bin`**, which
prints nothing and exits 0 (`tests/helpers.bash:24-35`) — `crew new` would return an
empty id and `crew crews` no rows. `tests/crews.bats` defines a local stub that answers
`new` and `crews`. Nothing in `tests/dispatcher.bats` breaks: every test there sets
`CREW_ID`, which suppresses the notice, and its two `grep -cx` assertions (lines 88, 90)
are exact-match, so an extra `crews` line in the stub log cannot perturb them.

## 9. Risks

- **Contract change on `crew id`.** Mitigated by §4's enumeration and by the fact that
  the only behaviour that changed is the previously-broken unset path.
- **Usage-line conflict.** Three new subcommands append to crew.sh's single-line usage
  string, which live workers also append to. A one-line textual conflict, trivially
  resolved.
- **PID reuse in `alive`.** Documented, not fixed — a correct answer needs a start-time
  check that is not portable across BSD/GNU `ps`.
$$
