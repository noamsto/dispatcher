# Worker liveness detection — interactive prompts and dead turns on the crew bus

**Status:** design — revision 2 (post-critic)
**Date:** 2026-08-10
**Repo:** dispatcher (personal / GitHub) · default branch `extract`
**Closes:** #31
**Artifacts touched:** `adapters/core/crew.sh` (`stall-watch`, `roster`, `rate`), `tests/crew.bats`, `adapters/core/protocols/WORKER_PROTOCOL.md` (liveness / blocked-state sections), `adapters/core/protocols/DISPATCHER_PROTOCOL.md` (the one `stalled:` liveness paragraph — see §9 for the ownership statement), `adapters/core/dispatch.sh` (**one token** on the stall-watch spawn line), regenerated trees via `scripts/gen-adapters.sh`.

## Problem

The crew bus reflects only what a worker **actively posts**. A worker that stops posting is indistinguishable from one that is working: `crew roster` keeps printing `working`, `crew watch` (which deliberately does not wake on `working`) parks quietly, and the dispatcher reads an idle roster as idle work. **Three** causes, one blind spot:

1. **Interactive prompt instead of `crew status blocked`** — the worker rendered an option-select frame in its own tmux pane and waited for input that could never arrive (#31, steady state).
2. **Dead turn** — a model-API stall ended the turn silently; the pane showed a spinner frozen at a constant token count while the wall clock climbed (#31, steady state).
3. **Interactive prompt during startup, mis-reported as `failed`** — a worker parked on Claude Code's workspace-trust prompt before it could read its task file. The existing watchdog saw a static pane, called it dead, and posted `failed`. **This is the same blind spot with the sign flipped: the bus said the worker was gone while it sat alive and answerable.** It is the highest-frequency occurrence measured to date — three of three workers in one dispatch (§Evidence, session 3).

`crew stall-watch` already tails the pane, but its rule (pane bytes unchanged for `--stall 300`) only runs inside the startup `--window 900`, and `_progressed()` exits the watchdog the moment the worker posts anything past its launch `working`. So the entire steady state — where symptoms 1 and 2 live — is uncovered; and inside the startup window, where symptom 3 lives, the rule fires but **cannot tell a parked prompt from a dead process** and resolves the ambiguity in the unrecoverable direction.

Symptoms 1 and 2 each cost roughly an hour of silent dead time. Symptom 3 costs three healthy workers, had the dispatcher acted on what the bus said.

## Evidence and provenance

**The capture sessions, stated once:** three, all 2026-08-10. Raw material for all three is pinned at repo root in `EVIDENCE-2026-08-10.txt`; every verbatim string below is quoted from it. **Session 1** (the original draft): `tmux capture-pane -p` against **seven** live claude worker panes on this machine, sampled at **20 s over ~5 minutes** (≈15 samples per pane). An earlier draft described this as "4 min"; it is the same session, and 5 min is the correct figure. Nothing longer than 19 m 12 s of elapsed-turn time was observed, and no pane was watched through a completed long tool call. **Session 2** (this revision): **nine** live claude panes, sampled at **15 s for ~75 samples per pane**. This session caught what session 1 missed: a **complete live prompt frame** (footer, option list, and geometry all at once), a **>7 min Agent-tool subagent batch** on a parent pane, and turns running out to **31 m 45 s**. Between the two sessions, A1, A3 and A4 below are now settled; A2 remains open. **Session 3** (this revision) is not a sampling run but a **field incident on this very dispatch**: the dispatcher hand-verified all three worker panes in this crew, and the bus log corroborates it. It is the only evidence to date of a detector firing on production workers, and it refutes two decisions this spec previously made.

Every claim below carries its provenance. `[live]` = from a sampling capture. `[bus]` = verbatim lines from `crew` `events.jsonl` (machine-written, not transcribed). `[field]` = the dispatcher's hand-verification at the pane during session 3 — a human reading, so exact bytes and geometry are _not_ guaranteed the way `[live]` and `[bus]` are. `[#31]` = from the issue's pasted text, which is a human transcription. `[UNVERIFIED]` = inferred, load-bearing, and gated in "Unverified assumptions and ship gates".

- `[live]` **Progress-meter line**, observed shape: `✳ Perusing… (5m 29s · ↓ 25.0k tokens · thinking more with high effort)` · `✻ Osmosing… (4m 8s · ↓ 12.1k tokens)` · `· Levitating… (19m 12s · ↓ 46.2k tokens · still thinking with high effort)`.
- `[live]` **The meter is present throughout a turn, including during a long Agent-tool subagent batch — confirmed, and it landed on the dangerous branch.** Session 2, parent pane %129, 15 s samples across a >7 min subagent batch: `✶ Hatching… (26m 57s · ↓ 73.2k tokens)` … `· Hatching… (29m 29s · ↓ 73.2k tokens)` — the token string held at `73.2k` across **11 consecutive samples (26m 57s–29m 29s, ~2.5 min)** while the clock advanced on every sample; the concurrent live subagent row's own token count (`71.5k`) held even longer, across 12 samples (3m 29s–6m 16s, ~2m 47s). Live subagent rows were present throughout, verbatim: `  ◯ general-purpose  Revise spec per critic` — and the completed-subagent history line `  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)` sat static in the same captures the whole time. This is now a **measured false-positive driver for D2, not a hypothetical**: a healthy deep worker running a subagent batch reproduces D2's exact signature (meter present, clock rising, token string static). One nuance: the parent's token string did **not** stay static for the whole batch — after the run above it advanced periodically (73.2k → 73.6k → 77.4k → 81.2k → 84.9k → 88.4k → 89.5k → 90.5k → 90.7k → 92.1k), so a fully-static 1800 s run during subagent work was not itself observed; the measured static runs are ~2.5–3 min, retained as insurance rather than a proven 1800 s trip, and the 3-concurrent review batch (not yet sampled) is the untested worse case. It was the single most load-bearing assumption in both D1 and D2; settled as assumption **A1**.
- `[live]` **Interactive-prompt footer, option-select family, three renderings observed.** Session 1: `Enter to select · ↑/↓ to navigate · Esc to cancel`, rendered as the **last line of a 51-row pane**, with **no meter line anywhere in that same frame**. Session 2: `Enter to select · Tab/Arrow keys to navigate · Esc to cancel`, caught this time as a **complete live prompt frame** (pane %118) — footer, full option list, and geometry all at once, again with **no meter line anywhere in the frame**. The stable substrings across these three (the two above plus #31's transcription) are `Enter to select` and `Esc to cancel`.
- `[field]` **A SECOND prompt family exists, and it does not contain `Enter to select`.** Claude Code's workspace-trust prompt, quoted verbatim from the evidence file: `"Quick safety check: Is this a project you created or one you trust?"` / `"> 1. Yes, I trust this folder"` / `"2. No, exit"` / `"Enter to confirm"`. **`Enter to confirm`, not `Enter to select`** — a D1 keyed solely on `Enter to select` misses this frame entirely, which is the frame that actually wedged three workers. The option lines match the existing numbered-option ERE unchanged (`> 1. …` with the ASCII `>` the ERE already accepts, and `2. …`). Settled as assumption **A5** (footer substring), with geometry inherited from A3 rather than separately captured.
- `[#31]` The same footer transcribes as `Enter to select - up/down to navigate - Esc to cancel`. **The transcription demonstrably ASCII-fies `·` → `-` and `↑/↓` → `up/down` in a frame we did capture live.** That fact is used in B2's decision below.
- `[live]` **Option-list rendering, from session 2's complete prompt frame (pane %118)**: `❯ 1. Version-agnostic wiring` (selected, `❯` = U+276F) / `  2. Gate everything on 3.8` (unselected, two leading spaces) / … through `  5. Chat about this`. The existing ERE `^[[:space:]]*[>❯*]?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]` matches both forms, unchanged. The list is long and multi-line-wrapped, with a `─────` rule line between options; the nearest numbered option to the footer, `  5. Chat about this`, sits **2 lines above it** — the "within 6 lines above the footer" window (D1 conjunct 2) is confirmed but tighter than it looks, and the rule only needs one numbered line in the window, which it has. Settled as assumption **A4**.
- `[live]` **Decoy lines containing `tokens`**: `⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)` (a completed-subagent history line, static in scrollback) and `◯ general-purpose  Review …  1m 4s · ↓ 57.3k tokens` (a **live subagent row**). A naive "last line containing `tokens`" match reads these and lies.
- `[live]` **The token display rounds to 0.1k** — `25.0k` held unchanged across two consecutive 20 s samples on a healthy thinking turn.
- `[live]` **The meter vanishes for ~20–40 s between turns**, and is gone entirely once a turn ends (one pane sat meter-less for 5+ samples after finishing).
- `[live]` **A finished/idle claude pane's last non-empty line is the input box, and a prompt-parked pane's last non-empty line is the prompt footer — confirmed.** Meter-less panes, last non-empty line: `  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)`, `  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`, `  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`; the parked pane's last non-empty line: `Enter to select · Tab/Arrow keys to navigate · Esc to cancel`. D1's geometry anchor (§Design 1, D1) is confirmed by this. Settled as assumption **A3**. **The load-bearing consequence, stated explicitly because D1's conjunct set now rests on it alone: a pane that is not parked on a prompt ends its last non-empty line on the input box — never on transcript text.** Every meter-less pane captured ended on `-- INSERT -- ⏵⏵ auto mode on …`; none ended on scrollback content. So a prompt frame that merely _scrolls through_ a pane — this change's own `tests/crew.bats` fixtures being read, a pasted frame in a transcript — is by construction not the last non-empty line, and the geometry anchor rejects it without any help from a transition rule.
- `[#31]` **≥1 h meter rendering**: `Considering... 1h 20m - 28.0k tokens`. Never observed live. `[live]` The longest turns sampled to date (session 2, same subagent-batch capture as A1) reached **31m 45s**, still rendering as `<m>m <s>s` with no hours component — the seconds component is confirmed present at least out to 31 minutes, but no hours-component meter has been observed. Assumption **A2** remains open.
- `[live]` **`pane_current_command` is `.claude-wrapped`, not `claude`** on this machine — the engine is **not** inferable from tmux.

Codex and cursor are work-profile only; `dispatch` refuses them on this (personal) host, so **no codex or cursor pane could be captured**. Every per-engine decision below reflects that.

### Session 3 — the D0 misfire, measured on production workers

`[bus]` D0 posted `failed` on **all three workers of this crew**, this dispatch, within 13 s of each other, verbatim:

```
{"ts":1786338559586,"from":"worker:feat/31-surface-workers-blocked-on-interactive-p#s1786338213-54181","state":"failed","detail":"stalled: no output for 300s (suspected startup/indexing hang)"}
{"ts":1786338566153,"from":"worker:feat/19-verify-the-worktree-is-at-the-pr-head-be#s1786338220-55306","state":"failed","detail":"stalled: no output for 300s (suspected startup/indexing hang)"}
{"ts":1786338572335,"from":"worker:feat/30-phrase-spec-plan-critic-engine-neutrally#s1786338226-56558","state":"failed","detail":"stalled: no output for 300s (suspected startup/indexing hang)"}
```

`[bus]` All three were alive. Each then posted `working`; two have since reached `pr_open` and `done`:

```
{"ts":1786338636640,"from":"worker:feat/31-surface-workers-blocked-on-interactive-p","state":"working"}
{"ts":1786338977871,"from":"worker:feat/30-phrase-spec-plan-critic-engine-neutrally","state":"pr_open"}
{"ts":1786340535331,"from":"worker:feat/19-verify-the-worktree-is-at-the-pr-head-be","state":"pr_open"}
```

`[field]` Cause, hand-verified at all three panes: they "sat on Claude Code's workspace-trust prompt in their fresh worktrees, ~1.7s CPU over 6 minutes" — idle on an answerable question, **not** indexing and **not** hung. A dispatcher acting on that `failed` would have killed three healthy workers.

Four facts fall out of this, each load-bearing below:

1. **D0's `failed` is this design's best-evidenced false positive** — 3/3 workers, one dispatch, zero true positives observed in the same window. It is the _same_ bug class the spec exists to fix, so "unchanged behaviour, out of scope" no longer holds (→ R1, §Design 1 D0).
2. **The detail string asserted a cause it could not know.** `(suspected startup/indexing hang)` was wrong on all three; the pane carried no evidence of indexing. A watchdog may report what it observed, never why (→ D0's new detail string).
3. **The wedging prompt is present from the worker's first sample**, so there is no non-match → match transition to observe. D1's appearance conjunct, as written, could never have fired on it (→ R2, §Design 1 D1 conjunct 4).
4. `[bus]` **The watchdog's identity does not match the worker's.** The watchdog posts `from: "worker:<branch>#s<session>"`; the worker's own posts use bare `worker:<branch>`. Verified against the code on this machine: the installed `dispatch` (line 434) builds `worker_id="worker:$branch#$session"` and spawns `crew stall-watch "$worker_id"`, and the installed `crew` takes `me="${1:-}"` verbatim; the **checked-in** `dispatch.sh` still passes a bare `$branch` and the checked-in `crew.sh` builds `me="worker:$branch"`. Both shapes are live on this machine at once (→ R3, §Design 7 INV-W0).

`[field]` Also present in every worktree: ``direnv: error .envrc is blocked. Run `direnv allow` `` — every pane came up with no devshell (→ R4, §Design 11).

### Unverified assumptions and ship gates

A gate is a **blocking** precondition: if it is unmet at ship time, the detector it gates is disabled in the signature table (its matrix cell flips to `no`) and the PR says so. No gate is satisfiable by argument — each needs one capture.

| #      | Assumption                                                                                                                                                                                                                                                                         | Status                                                                                                                                            | Gates                                  | Gate: what must be captured                                                                                                                            | If it fails                                                                                                                                                                                                                                                                                                                                                                       |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **A1** | The parent pane's meter is present, with a **static token string**, while an Agent-tool subagent batch runs (`WORKER_PROTOCOL` mandates 3-concurrent execute subagents and a parallel review batch, so a deep worker's parent pane sits inside one such call for tens of minutes). | **Settled (live capture 2026-08-10).** Landed on the dangerous branch: meter present, clock advancing, token string static for multi-minute runs. | **D2**, and D1's conjunct 3            | One parent-pane capture across a real subagent batch ≥5 min, sampled ≤20 s — **captured** (pane %129, >7 min batch, 15 s samples).                     | N/A — met. The live-subagent-row ERE is finalised from this capture (§Design 1, D2).                                                                                                                                                                                                                                                                                              |
| **A2** | At ≥1 h the meter drops the seconds component and keeps the `… ( · ↓ · tokens)` structure.                                                                                                                                                                                         | **Open.** No hours-component meter observed; turns sampled to 31m 45s still render `<m>m <s>s`.                                                   | **D2**                                 | One live capture of a turn ≥1 h.                                                                                                                       | D2's claude cell stays `no` until captured. This is a **ship gate, not a nice-to-have**: D2's entire regime is turns over 30 min, and no sample exceeds 31m 45s.                                                                                                                                                                                                                  |
| **A3** | A claude pane that has finished a turn ends with the input box as its **last non-empty line**; a prompt-parked pane ends with the prompt footer as its last non-empty line. **A non-parked pane never ends on transcript text.**                                                   | **Settled (live capture 2026-08-10).**                                                                                                            | **D1**, and now **D0**'s prompt branch | One capture of a pane immediately after a turn ends, and one full prompt frame — **captured** (session 2, meter-less panes plus the parked pane %118). | N/A — met. The geometry anchor is confirmed, and it is now the **sole** mechanism giving D1 immunity to its own fixtures (B3) — the appearance conjunct that previously shared that job is dropped in revision 2 (§Design 1, D1).                                                                                                                                                 |
| **A4** | The numbered-option list renders as `> 1. …` / `  2. …` above the footer.                                                                                                                                                                                                          | **Settled (live capture 2026-08-10).** A third footer variant surfaced in the same capture; see the provenance bullets above.                     | **D1**                                 | The full prompt frame from the A3 capture — **captured** (pane %118, complete frame with footer, option list, and geometry).                           | N/A — met. The ERE matches the captured frame unchanged. **The conjunct is never dropped** — the footer alone is a phrase that can appear in ordinary output.                                                                                                                                                                                                                     |
| **A5** | The workspace-trust prompt renders footer `Enter to confirm` above a `> 1. …` / `2. …` option list, and — like every other parked pane — puts that footer on its **last non-empty line**.                                                                                          | **Footer text and option list: settled `[field]`** (verbatim, §Evidence session 3). **Geometry: inherited from A3, not separately captured.**     | **D0**'s prompt branch, **D1**         | Nothing blocking. One `tmux capture-pane -p` dump of a live trust prompt would upgrade the geometry half from inherited to captured.                   | **Not a ship gate, because the failure direction is silence.** If the trust footer is _not_ the last non-empty line (e.g. the direnv error paints below it), D0/D1 miss the frame and fall through to the `stalled:` branch — a false negative on the prompt classifier, never a false positive. Adding the substring cannot make anything fire that would not have fired before. |

A1, A3 and A4 were captured in the 2026-08-10 session 2 deliberate run: a parent pane through a subagent batch, a meter-less pane after a turn ended, and a full live prompt frame. A5 came from session 3's field incident. A2 still needs patience or a long real run — no session to date has produced a ≥1 h turn.

## Decisions

1. **The watchdog becomes lifetime-scoped, not startup-scoped.** `crew stall-watch` runs from launch to the worker's terminal state. `--window` stops bounding the process and bounds only the existing startup rule.
2. **Four detectors, three of them new**, each with its own evidence shape and its own posted state. **D0, the existing startup rule, is no longer exempt.** Revision 1 kept it untouched on scope-creep grounds; session 3 refuted that — D0 is now the design's best-evidenced false positive, of exactly the class this spec exists to eliminate. It is re-decided in §Design 1, D0.
3. **Every watchdog detector — D0 included — posts `blocked` first**, never `failed` on first evidence. A watchdog `blocked` that is **still corroborated by fresh evidence** after a further `--dead` escalates to `failed` — see Decision 9.
4. **Watchdog events are machine-labelled**: `body.source = "watchdog"` plus a reserved `detail` prefix. The dispatcher never has to guess whether a `blocked` came from the worker or from the watchdog, and does not need to have re-read its protocol to tell.
5. **The watchdog writes `status` events only — never `msg`.** A fabricated question would wake the dispatcher into `crew reply`, and nothing is listening (the worker is not in `crew await`).
6. **Engine-specific detectors are gated behind a signature table that ships claude-only.** codex and cursor get the engine-agnostic detectors and nothing else, by decision, until someone pastes a real capture.
7. **Bus activity damps the steady-state detectors** — but is never _required_. A worker inside a long tool call cannot heartbeat by construction, so damping alone is not a safety argument; every detector must be silent on a healthy worker with **zero** bus activity. That constraint drives the conjuncts in §1.
8. **Issue direction 3 (remove the question tool) is a partial yes**: protocol instruction now, launch-flag change as a separate issue.
9. **The watchdog never leaves the roster permanently ACTIVE.** `blocked`+`working` = ACTIVE in `DISPATCHER_PROTOCOL`'s park partition, so an unclearable watchdog `blocked` would re-arm a 270 s park forever and never free fan-out budget. Escalation to `failed` restores the budget-freeing semantics D0 has today (§Design 6).
10. **The watchdog is a subordinate writer.** It aborts any append if the worker has reached a terminal state, checked immediately before every write (§Design 7).
11. **Worker identity is branch-keyed and suffix-tolerant, on every read and every write.** The watchdog and the worker demonstrably run under two different `from` strings on this machine (§Evidence session 3, fact 4). Exact-string matching makes the entire safety layer — INV-W1, self-reported-`blocked` suppression, heartbeat damping — silently no-op, and splits the roster into phantom rows. This is stated as an invariant, not a bug fix, because every other invariant depends on it (§Design 7, INV-W0).
12. **A static pane is classified before it is judged.** Inside the startup window as well as outside it, the watchdog first asks _what the frame is_ (prompt? not a prompt?) and only then decides what to post. The classifier runs ahead of the silence timer in D0, not after it. Session 3 is the whole argument: the pane was static **and** answerable, and a rule that only measures silence cannot represent that.

## Non-goals

- **#32** (dispatch's pre-reap folds a just-launched worker) — same family, explicitly out.
- Changing `crew watch`'s default `--states`, its cursor, or its park semantics.
- Any supervisor that respawns a dead watchdog. The watchdog is best-effort; a lost one degrades to today's behaviour.
- Codex/cursor prompt and meter signatures (deferred with a documented enable procedure — §5).
- Notifying the human directly (`tmux display-message`) — the bus is the channel; the dispatcher decides what reaches a human.
- Removing `AskUserQuestion` via a launch flag (§7 — follow-up issue).
- Filesystem/git-activity liveness probes (§10, rejected alternative).
- **A detector for the blocked-`.envrc` / missing-devshell condition** (§11 — documented note plus a PR-body callout, no code).
- Answering a detected prompt automatically. The watchdog reports; the dispatcher decides. `send-keys` into a worker pane on the strength of a substring match is the one action that could destroy work faster than a false `failed`.

---

## Design

### 1. Detector set

`grace` and `interval` keep their current meaning. All four detectors read the same per-tick pane capture, so the sampling cost is unchanged.

| #       | Name                                            | Evidence                                                                                                           | Engines   | Posts                    | `detail` prefix |
| ------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ | --------- | ------------------------ | --------------- |
| **D0p** | startup prompt (**new branch of D0**)           | pane bytes unchanged ≥ `--stall` (300 s), inside `--window` (900 s), **and the frame is a prompt**                 | claude    | `blocked`                | `prompt:`       |
| **D0s** | startup silence (**existing rule, re-decided**) | the same, **and the frame is NOT a prompt**                                                                        | all       | `blocked` (was `failed`) | `stalled:`      |
| **D1**  | interactive prompt                              | prompt frame present at the verified geometry, meter absent, 2 consecutive samples                                 | claude    | `blocked`                | `prompt:`       |
| **D2**  | dead turn                                       | meter present, clock string **changed**, token string **unchanged**, **no live subagent row**, ≥ `--idle` (1800 s) | claude    | `blocked`                | `turn-stall:`   |
| **D3**  | quiet pane                                      | pane bytes **byte-identical** ≥ `--idle` (1800 s), in steady state                                                 | all       | `blocked`                | `quiet:`        |
| **E**   | escalation (§6)                                 | a D0s/D1/D2/D3 episode still corroborated after a further `--dead` (1800 s)                                        | as origin | `failed`                 | `dead:`         |

#### D0 — startup silence, re-decided

**Revision 1 said: "D0 keeps posting `failed` because it is unchanged behaviour with a documented dispatcher recovery path; re-litigating it is scope creep." That is retracted.** Session 3 fired D0 on three healthy workers in one dispatch — a 100 % false-positive rate on the only production sample that exists — with a diagnosis in the detail string (`suspected startup/indexing hang`) that was wrong on all three. "Unchanged behaviour" is not a defence when the behaviour is the bug the spec was opened to fix, and the startup window turns out to be where the highest-frequency real occurrence lives: a fresh worktree is exactly where Claude Code asks for workspace trust, so _every_ worker's first 300 s is prompt-exposed by construction.

**The decision procedure.** On a static pane inside `--window`, once `--stall` has elapsed, classify **before** judging (Decision 12):

1. **Prompt branch (D0p)** — the last non-empty line contains any footer substring from the D1 signature set (§D1 conjunct 1) **and** a numbered-option line matches within the 6 lines above it. Post **`blocked`**, prefix `prompt:`, identical detail and clearance machinery to D1. **This test runs first and wins.** A static pane that is a prompt is never reported as silence, inside the startup window or out of it.
2. **Silence branch (D0s)** — anything else. Post **`blocked`**, prefix `stalled:`, detail **`stalled: no output for <n>s`** and nothing more.

The meter-absent conjunct (D1 conjunct 3) is redundant here and is **not** required on the D0p branch: the pane is already byte-static for 300 s, which no streaming meter can be. Requiring it would cost nothing but buy nothing; omitting it keeps the branch a strict specialisation of "static pane that looks like a prompt".

**The detail string loses its diagnosis.** `(suspected startup/indexing hang)` is deleted. The watchdog observed no output for N seconds; it did not observe indexing, and on 3/3 measured cases there was none. A watchdog reports what it saw. A cause it invents propagates into the dispatcher's decision and is read as corroboration — which is precisely how a `blocked`-shaped situation became a `failed`.

**Should the residual `stalled:` branch also soften from `failed` to `blocked`? Decided: yes.** Three reasons, and one counter-argument answered:

- **The classifier can miss, and its misses land here.** A5's geometry is inherited, not captured; a trust prompt with the direnv error painted below it, or any prompt family not yet observed, falls through to D0s. The branch that catches the classifier's errors must be the recoverable one, or the false positive is merely relocated rather than fixed.
- **`failed` is unrecoverable-by-reply and destroys work; `blocked` costs one pane glance.** That is this design's stated ordering everywhere else (Decision 3, §10). D0 was the sole exception, and it is the only detector with measured false positives.
- **Nothing is lost that §6 does not restore.** The one thing D0's `failed` bought was budget-freeing (Decision 9). Escalation gives it back on evidence rather than on first sight: a D0s episode whose pane stays byte-static for a further `--dead` posts `failed` with `dead: stalled: unchanged for <n>s`. A genuinely hung startup satisfies that second check by construction; the three session-3 workers would have cleared long before it.
- **Counter-argument, answered.** "A startup hang is different: nothing has been drawn yet, so a static pane really is dead." Session 3 is the counter-example — the pane was static _because_ it had drawn a question and was waiting. Startup is where the pane has the **least** history to distinguish hung from waiting, which argues for more caution there, not less.

Exposure: ACTIVE for ≤ `--stall` + `--dead` (≈35 min at defaults) before budget frees, against today's ≈5 min — paid for by never again posting an unrecoverable state on a worker that is alive and answerable.

**Regression test (required, names the session-3 case directly).** A pane that is byte-static across the whole `--stall` window and whose capture carries the pinned workspace-trust frame produces exactly one event: `blocked`, `source: watchdog`, detail prefixed `prompt:`. **It must produce no `failed`, and no `stalled:` detail, at any point in the run.** A second case, same staticness with a non-prompt frame, produces `blocked` with detail exactly `stalled: no output for <n>s` — asserted as a full-string match so a reintroduced diagnosis fails CI.

#### D1 — interactive prompt

Fires only when **all four** hold:

1. **Geometry (anchored, not "the tail region") plus a two-member footer set.** The **last non-empty line** of the capture contains one of exactly these case-sensitive substrings:

   | Substring          | Family                                           | Provenance                    |
   | ------------------ | ------------------------------------------------ | ----------------------------- |
   | `Enter to select`  | option-select (`↑/↓` and `Tab/Arrow` renderings) | `[live]` sessions 1 and 2     |
   | `Enter to confirm` | workspace-trust                                  | `[field]` session 3, verbatim |

   That is the **complete** set. `Esc to cancel` is deliberately **not** in it: it appears in the option-select footer only, adds no frame the two `Enter to …` anchors miss, and matching it alone would fire on any pane whose bottom line happens to mention cancelling. Keying solely on `Enter to select` — revision 1's rule — misses the trust prompt entirely, which is the frame that actually wedged three workers.

   **Re-checked against the false-positive constraint.** Widening a substring set can only add firings, so the added anchor is re-tested against all three surviving conjuncts: `Enter to confirm` must still be the **last non-empty line** (A3 says a non-parked pane ends on the input box, so ordinary output carrying the phrase cannot satisfy this), must still have a numbered-option line within 6 lines above it, and must still coexist with **no meter anywhere**. A pane discussing prompts in prose satisfies none of the three. The one shape that would defeat this is a _finished_ turn whose final rendered output ends in a numbered list under a line containing `Enter to confirm` and with the input box scrolled off — which A3 says cannot happen, since a finished pane's last non-empty line is the input box.

   Not "in the last 10 lines": the verified frames put the footer on the pane's bottom line, and relaxing that is exactly how this change's own `tests/crew.bats` prompt fixtures become a false positive on the highest-frequency file in the diff.

2. **A numbered-option line within the 6 lines directly above that footer** — ERE `^[[:space:]]*[>❯*]?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]`. Six covers the observed 3-option frame plus a question line with headroom for a 5-option list; the gate capture's 5-option frame put the nearest option 2 lines above the footer, so six retains headroom. Provenance A4, settled — the ERE matches the captured frame unchanged.
3. **No meter line matches anywhere in the capture.** A1's live capture confirms the meter is present throughout subagent work, so this conjunct discriminates unconditionally: a turn cannot be both streaming (meter present) and parked on a prompt (meter absent) — confirmed directly, too, against the A4 gate capture, which showed no meter line anywhere in the parked frame. Conjuncts 1 and 2 stay anchored the way they are regardless: neither is satisfiable by a busy pane running subagents, and that redundancy is kept rather than trimmed. **This conjunct carries more weight in revision 2** — with the appearance rule gone, meter-absence is one of only two conjuncts (with geometry) that a busy pane cannot satisfy.
4. 1–3 hold on **two consecutive samples**. Presence, not transition: a frame matching on the watchdog's very first two samples fires.

**Conjunct 4 of revision 1 — "the frame must have APPEARED (non-match → match)" — is dropped. Stated plainly because it was argued for at length and is now refuted.**

- **Why it is refuted.** The workspace-trust prompt is on screen from the worker's **first** sample: it is rendered before the worker reads WORKER_TASK.md, in every fresh worktree. There is no transition to observe, so D1-as-written could never fire on it — and that is the frame with 3/3 measured occurrences. A conjunct that is silent on the only production case is not a safety margin, it is a blind spot with a good story.
- **What it was for, and what replaces it.** It was added for critic finding B3: immunity to this change's own prompt fixtures in `tests/crew.bats` scrolling through a pane. **A3 is now settled by live capture and covers B3 on its own.** A pane that is not parked on a prompt ends its last non-empty line on the input box (`-- INSERT -- ⏵⏵ auto mode on …`), never on transcript text — measured across every meter-less pane in session 2. A fixture scrolling past therefore lands _in_ the transcript, above the input box, and can never be the last non-empty line. The geometry anchor is not a weaker substitute for the appearance rule; for B3's specific hazard it is a **stronger** one, because it holds on the first sample as well as on later ones, and does not depend on the watchdog having observed the pane before the fixture appeared.
- **Alternative considered and rejected: restate as "appeared OR present since the first sample".** That is a no-op dressed as caution — "present since the first sample" is satisfied by every case the plain rule catches, so the disjunction reduces to presence. Keeping the words would imply a discrimination the code does not make. Dropped outright instead.
- **Residual fixture risk, stated.** Geometry immunity fails in exactly one shape: a fixture whose text is the **last non-empty line of the pane**, with a numbered-option line directly above it and no meter — i.e. a bats run that ends by printing a prompt frame and then prints nothing else, in a pane the watchdog is watching. Bats prints a summary line after each file and the input box repaints after the run, so this needs the pane to be captured inside a sub-second window and to stay byte-static there for two consecutive samples. It is not zero; it is bounded, self-clearing (the next repaint clears the episode), and costs one `blocked` plus one pane glance. That is accepted in exchange for detecting the frame that wedged three workers.
- **What is genuinely lost.** The appearance conjunct also gave free immunity to double-posting from two watchdogs on one branch (open question 4). That immunity is _not_ free any more and is replaced by an explicit bus check, **INV-W3** (§7), which is available only because identity matching is now branch-keyed (INV-W0).

Detection latency: ≤ `grace` + 2 × `interval` — **≤ 75 s at defaults**, and a prompt already on screen at launch is now reported at `grace` + 2 × `interval` rather than never. That is the bounded interval the acceptance criteria name.

**Pinned fixture — the workspace-trust frame (`[field]`, verbatim from `EVIDENCE-2026-08-10.txt`).** Pinned as a fixture in its own right, _not_ folded into the option-select fixture, because it is a different shape:

```
Quick safety check: Is this a project you created or one you trust?
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
```

Differences from the option-select frame, each one a reason it needs its own assertion: the footer says **`Enter to confirm`, not `Enter to select`**; there is no `Esc to cancel`; the selection marker is ASCII `>` rather than `❯`; and it renders with a leading question line rather than a `─────` rule. Only the numbered-option ERE is shared, and it matches both unchanged. The fixture asserts D1 **fires** on it, and — paired with a static-pane sequence inside `--window` — that D0 posts `prompt:` and never `stalled:`/`failed`.

The frame is quoted at the fidelity the evidence file provides (hand-verified lines, not a byte-exact `capture-pane` dump — see A5). The fixture is therefore an assertion about the **substrings and their relative order**, not about column padding or box-drawing characters, and the test is written to match on that basis rather than on an invented full-width rendering.

Posted event: `blocked`, `source: "watchdog"`, detail `prompt: interactive prompt in pane %<id> — worker is waiting on input nobody can give`. The **question text is not scraped into the detail**: the frame's wording is unstable across renderings, and a truncated half-question invites the dispatcher to answer the wrong thing. The detail names the pane; the dispatcher reads it.

**Clearance.** When the frame stops matching, the watchdog posts `working` with `source: "watchdog"` and detail `prompt: cleared`. `working` is not in `crew watch`'s wake set, so this corrects the roster **without** waking the dispatcher a second time. Clearance re-arms D1 (and cancels any pending escalation), so a second prompt in the same run is reported again. Clearance is subject to the write invariant in §7.

#### D2 — dead turn

The discriminator #31 proposes ("rising wall clock against a static token count") is right in shape but wrong on two counts if taken literally: implemented numerically it is fragile, and **as stated it fires on the dominant long call in this harness — an Agent-tool subagent batch.** A deep worker's parent pane sits inside a 3-concurrent execute batch or a parallel review batch for tens of minutes, streaming no tokens of its own and unable to post a heartbeat. Damping is inert there by construction (Decision 7). This is now a **measured false-positive driver, confirmed by A1's live capture, not a hypothetical**: a healthy deep worker running a subagent batch reproduces D2's exact signature — meter present, clock rising, token string static — for minutes at a stretch. Four decisions make D2 safe:

- **Compare strings, never numbers.** Extract the clock (`5m 29s`) and the token figure (`25.0k`) from the meter line and ask only _changed_ / _unchanged_. No arithmetic, no unit parsing, nothing to misparse.
- **Require the clock to have changed.** A rising clock proves we are reading a **live frame**. If the clock is also static, the capture is stale (frozen renderer, or a human in copy-mode scrollback) — evidence we cannot trust, so the rule stays silent.
- **Require the absence of a live subagent row — unconditionally.** This is the conjunct that excludes the subagent batch. A1's capture confirms the meter is present throughout, so this conjunct now works alongside D2's meter-based conjuncts rather than being the sole discriminator on some other branch. It is stated as **presence of the row shape**, not "the row's timer is advancing": presence-only is the more silent reading, and the capture confirms it loses nothing — the captured row's own elapsed timer advanced on every sample throughout the batch.
- **Ship behind A2.** A1 is settled; D2's claude cell stays `no` until A2's ≥1 h capture exists.

Meter recognition, anchored on the verified frame and widened for A2 — a gerund + `…`, a parenthesised duration with **at least one unit** and optional dropped minutes/seconds, then `· ↓`, then the token figure:

```
^[^[:alnum:]]*[A-Za-z]+…[[:space:]]\(([0-9]+h([[:space:]][0-9]+m)?([[:space:]][0-9]+s)?|[0-9]+m([[:space:]][0-9]+s)?|[0-9]+s)[[:space:]]·[[:space:]]↓[[:space:]][0-9.]+k?[[:space:]]tokens
```

Live-subagent-row recognition, from the one verified row (`◯ general-purpose  Review …  1m 4s · ↓ 57.3k tokens`) — a glyph, a lowercase kebab agent-type, a column gap, then an elapsed timer and `· ↓`:

```
^[[:space:]]*[^[:alnum:][:space:]][[:space:]]+[a-z][a-z-]+[[:space:]][[:space:]]+.*[[:space:]](([0-9]+h[[:space:]])?([0-9]+m[[:space:]])?[0-9]+s)[[:space:]]·[[:space:]]↓
```

This ERE is **finalised by A1's capture**, against a second verbatim row of the same shape: `  ◯ general-purpose  Revise spec per critic` … `3m 29s · ↓ 71.5k tokens` — glyph, two leading spaces, lowercase-kebab agent type, description, then an elapsed timer and `· ↓` before the token figure, same as session 1's row. It must reject the completed-subagent history line, verified again in this capture — `⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)` sat static across all 20 samples in the same column — which it does twice over (no `· ↓` and `Done` is not lowercase-kebab); if it matched, that line's permanent presence in scrollback would neuter D2 entirely.

**Decision on #31's `Considering... 1h 20m - 28.0k tokens`: treat it as a lossy human transcription, and do not widen the ERE to accept it.** The same paste demonstrably ASCII-fied `·` → `-` and `↑/↓` → `up/down` in the _one_ frame we captured live; `...` for `…` and `-` for `· ↓` are the same substitution class. The alternative — accepting a bare `-` as the separator — makes the meter regex match ordinary prose and log output, which manufactures false positives in the direction the whole design forbids. The cost of being wrong is bounded and visible, not silent: A2's live ≥1 h capture is a ship gate, and if it shows a real `...`/`-` rendering the ERE widens and the fixture assertion below flips before D2 ships.

**Fixtures (`tests/crew.bats`), each with an explicit match assertion:**

| Fixture                                                                      | Provenance              | Meter ERE                                                                                 |
| ---------------------------------------------------------------------------- | ----------------------- | ----------------------------------------------------------------------------------------- |
| `✳ Perusing… (5m 29s · ↓ 25.0k tokens · thinking more with high effort)`     | `[live]`                | **matches**                                                                               |
| `· Levitating… (19m 12s · ↓ 46.2k tokens · still thinking with high effort)` | `[live]`                | **matches**                                                                               |
| `✻ Osmosing… (4m 8s · ↓ 12.1k tokens)`                                       | `[live]`                | **matches**                                                                               |
| `Considering... 1h 20m - 28.0k tokens`                                       | `[#31]`                 | **does NOT match** — asserted deliberately, with the transcription reasoning in a comment |
| `Considering… (1h 20m · ↓ 28.0k tokens)`                                     | reconstructed wire form | **matches** — this is what A2 must confirm                                                |
| `⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)`                              | `[live]`                | does NOT match (meter or subagent-row)                                                    |
| `◯ general-purpose  Review …  1m 4s · ↓ 57.3k tokens`                        | `[live]`                | matches **subagent-row** ERE, not meter                                                   |
| A2's live ≥1 h capture                                                       | gate output             | **matches** (added when the gate is met)                                                  |

**Fires** when, continuously for `--idle` (1800 s): a meter matched on every sample; its clock string changed at least once; its token string never changed; **no live subagent row matched on any sample**; and no status event from this worker landed on the bus in the window. Posts `blocked`, detail `turn-stall: token count static at <tok> for <n>s while the pane clock advanced`. One post per episode; clearance (token string moves, a subagent row appears, or the meter disappears) re-arms it and posts the same `working` correction as D1.

**What the subagent conjunct costs.** A turn that genuinely dies while a subagent row is still painted on the parent pane is invisible to D2 — and if those rows animate, invisible to D3 too. That is a real, stated blind spot, accepted because the alternative flips healthy deep workers, which the task forbids outright. A1's capture establishes the healthy baseline — the row's own elapsed timer advanced on every sample across the >7 min batch — but no capture has yet watched a subagent die mid-batch, so whether its row's timer actually freezes when that happens is still unknown (if it does, a future revision can add "rows present but all timers static" as a separate detector; that is deliberately not designed here on zero evidence).

**Why 1800 s.** A legitimately busy claude pane looks like a stalled one for the duration of any long call: `bats tests/`, `nix flake check`, a held `crew await --timeout 300`, `gh pr checks --watch`, **and the subagent batches above** — the clock advances and no tokens stream. 1800 s sits above every tool call this harness documents and below the ~1 h dead time the issue reports. The subagent conjunct, not the threshold, is what covers the batch case; the threshold covers the rest.

#### D3 — quiet pane

Pane capture **byte-identical** across every sample for `--idle` (1800 s), with no bus activity from the worker in that window, in steady state (after `--window`), and no terminal state on the bus. Posts `blocked`, detail `quiet: pane unchanged for <n>s`.

D3 is D0's rule, extended past the startup window and softened from `failed` to `blocked`. It earns its place three ways:

- It is the **only** steady-state coverage codex and cursor get, and it needs zero knowledge of their frames: any TUI that streams changes bytes.
- It is the **failsafe for signature rot**. If a Claude Code update changes the meter line, D2 goes quiet (a false negative, not a false positive) — and a genuinely idle pane still trips D3.
- It covers the "session died outright" case #31 names: a worker whose turn ended without a terminal post sits at a static input box. (`SessionEnd` → `exited` covers a process that actually exits; D3 covers one that stays up and stops working.)

D3 is stated as **byte-identity**, not "no meter" — a healthy claude pane repaints its spinner and clock every second, so a working worker can never satisfy D3 even if every claude signature rots to garbage. Byte-identity also makes D3 immune to whether a subagent batch is running: a pane running one repaints their timers regardless (confirmed by A1's capture).

### 2. Coverage window and lifetime

|                  | today                                                        | after                                                                               |
| ---------------- | ------------------------------------------------------------ | ----------------------------------------------------------------------------------- |
| process lifetime | `--window` (900 s)                                           | until a terminal bus state, a pane confirmed gone, or `--max-life` (43200 s / 12 h) |
| `--window`       | bounds the whole process                                     | bounds **D0 only**                                                                  |
| `_progressed()`  | **exits the process** on any state past the launch `working` | replaced by `_bus_state()` — a read, never an exit                                  |

`_progressed()` as written is a liability in two independent ways. **First, and newly measured: its `jq` filter is `.from==$m`, an exact string comparison, and on this machine `$m` and the worker's own `from` are different strings** (§Evidence session 3, fact 4) — so on the installed build it matches nothing, always returns "no progress", and every state-dependent behaviour built on it is inert. `_bus_state()` fixes this by construction under INV-W0 (§7); it is called out here because §2's disarm/suppress/exit split is meaningless without it.

Second — and this is the reading that matters for §3, and was true even before the identity bug — it returns 1 (keep watching) for `""` and `working`, so `working` heartbeats do **not** kill the watchdog today; what kills it is the _first_ `blocked` or `pr_open`. Both are states a worker keeps working through: a self-reported `blocked` is answered and resumed, and `pr_open` is followed by CI-babysitting — which is exactly what #31's symptom-1 worker was doing when it rendered the prompt. So today's watchdog reliably disarms itself right before the two windows where the reported failures happen. It is retired and its three jobs are split:

- **disarm D0** — the startup rule stops once the worker has posted anything past the launch `working` (its original intent, preserved);
- **suppress** — while the latest state is a **self-reported `blocked`**, all new detectors stay silent: the dispatcher is already awake about this worker and is possibly mid-`crew reply`, and the worker's pane during a held `crew await` is a static pane by construction (a D3 false positive waiting to happen);
- **exit** — the process ends on `done`, `failed`, or `exited`. **Not** on `pr_open`: a worker in `pr_open` is still working, and #31's symptom-1 prompt was rendered _by a worker watching PR CI_.

**Pane-gone is now a quorum, not a single failure.** Today a non-zero `tmux capture-pane` means "pane gone → exit 0", which permanently disarms liveness. At a 900 s lifetime that is cheap; at 43200 s one tmux hiccup silently kills the watchdog for the rest of a 12-hour run. The pane is concluded gone only after **N = 3 consecutive** sample failures (45 s at the default interval); any success resets the counter. Three is chosen to survive a transient without materially delaying the real case, which the `exited` backstop owns anyway.

**Bus-read cadence.** A whole-file `jq` over a growing, cross-crew `events.jsonl` every 15 s for 12 h is ~2880 spawns per worker, per detector-relevant read. The suppression/damping read runs **every 4th tick** (~60 s), so ~720 spawns per worker. Cost: up to 60 s of latency in noticing a heartbeat or a `pr_open`, which only matters at the `--idle` boundary and is 60 s against 1800 s. **The pre-write terminal check in §7 is exempt from this cadence** — it runs immediately before every append, and appends are rare (at most a few per episode).

`--max-life` exists because the watchdog is `nohup`-detached: without a hard cap, a bug or an orphaned pane leaves a process polling forever.

### 3. Composition with `working` heartbeats and `crew watch`

- **Any status event from the worker resets D2's and D3's clocks.** A worker that heartbeats every seam can never trip a steady-state detector.
- **Absence of heartbeats is never evidence**, and **presence of heartbeats is not a safety argument** (Decision 7). A worker inside a tool call — the exact regime D2 targets — cannot heartbeat. Damping is a bonus; the conjuncts carry the safety.
- **`crew watch` wakes on `blocked`** (default `--states blocked,pr_open,done,failed`) — every new post reaches a parked dispatcher with no change to `watch`. The `working` clearance re-stamp is invisible to `watch` by the same table, which is precisely why auto-correction is safe.
- **The watchdog posts at most once per episode.** No re-stamping while an episode persists — this keeps `roster`'s `age_s` on a watchdog `blocked` meaning "time since the watchdog said so", which is what the dispatcher's stale-blocked heuristic reads.

### 4. Telling watchdog `blocked` apart from a worker's own

Three layers, in order of how a reader hits them:

1. **`body.source: "watchdog"`** — machine-readable, on every watchdog-emitted event (including, retroactively, D0's existing `failed`). The watchdog appends its own JSON line, so this needs no CLI change and no schema migration; readers that ignore the field are unaffected.
2. **Reserved `detail` prefixes** — `stalled:` (existing), `prompt:`, `turn-stall:`, `quiet:`, `dead:`. **These are self-describing on purpose:** a LIVE dispatcher does not re-read `DISPATCHER_PROTOCOL.md` mid-session, so the disambiguation must work from the event alone whether or not the doc edit in §9 has landed in that session's context.
3. **`crew roster` carries `detail` and `source`** on each row. `detail` is **truncated to 120 characters** in the roster (the full string stays in the log): the roster is an LLM-read dashboard and `detail` is unbounded free text, so an untruncated row can crowd out the rest of the table. 120 fits every prefix plus its payload.

**The operational difference, and the recovery sequence.** A self-reported `blocked` worker **is in `crew await`** and resumes on `crew reply`. A watchdog-posted `blocked` worker is **not listening** — there is no question in `crew inbox`, and a reply is a no-op that looks like an answer.

Recovery is **verify, then act**, mirroring the guard `DISPATCHER_PROTOCOL.md` already carries for `stalled:`:

1. `tmux capture-pane -p -t %<id>` on the pane named in the `detail`. Always. The `detail` exists to make this one command possible.
2. **The pane confirms a prompt** (`prompt:`) → answer it in place; the worker resumes and the watchdog clears the state itself. **Startup prompts are the common case, not the exotic one** — a fresh worktree draws the workspace-trust question before the worker reads its task (§Evidence session 3).
3. **The pane confirms a dead turn or a dead pane** (`turn-stall:` / `quiet:` / `dead:`) → kill and re-dispatch.
   3b. **`stalled:` means "static pane, and the watchdog could not recognise the frame"** — it is now a `blocked`, not a `failed`, and it is deliberately the _weakest_ claim the watchdog makes. Look at the pane before doing anything: an unrecognised prompt family, a shell waiting on `direnv allow`, and a dead process all arrive under this prefix.
4. **The pane shows work in flight** (a live meter, advancing subagent rows) → it is a false positive. Do **not** kill. Post nothing; the watchdog clears itself on the next sample, or escalation is cancelled by the clearance.

Step 4 is the point: "kill and re-dispatch" as an unconditional instruction turns any false positive into the destroyed-real-work outcome the task forbids, and contradicts this design's own claim that a false positive costs one pane glance. The glance **is** the guard.

### 5. Per-engine behaviour

The engine cannot be inferred from tmux (`pane_current_command` reads `.claude-wrapped`), so `dispatch` passes it: **one token added to the spawn line**, `--engine "$agent"`. That is the whole `dispatch.sh` diff; the `--pr` path is untouched. A missing `--engine` resolves to `unknown` and takes the conservative row — the flag's absence can never enable a detector.

| engine      | D0s startup silence | D0p startup prompt | D1 prompt                  | D2 dead turn                      | D3 quiet pane | why                                                                                                                                                                  |
| ----------- | ------------------- | ------------------ | -------------------------- | --------------------------------- | ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **claude**  | yes                 | **yes**            | **yes** (A3+A4+A5 settled) | **yes, gated on A2** (A1 settled) | yes           | D1 frames fully verified; D0p reuses D1's signature set, so it ships with it; D2 still needs A2's ≥1 h capture                                                       |
| **codex**   | yes                 | **no**             | **no**                     | **no**                            | yes           | no pane could be captured (work-profile only); its TUI streams, so byte-identity is sound, but its prompt frame and meter format are unknown and will not be guessed |
| **cursor**  | yes                 | **no**             | **no**                     | **no**                            | yes           | same, plus a stronger reason to gate D1: a footer that permanently contained an `Enter to select`-like string would pin every cursor worker at `blocked` forever     |
| **unknown** | yes                 | no                 | no                         | no                                | yes           | conservative default                                                                                                                                                 |

**D0p is gated on the same signature availability as D1** — it _is_ D1's classifier, run early. So an engine without a verified prompt frame gets no prompt branch and every static startup pane falls to D0s. Note the consequence, which is the safe one: for codex/cursor, D0's softening from `failed` to `blocked` is a **pure improvement with no new signature risk** — they gain a recoverable state and lose nothing, because the branch that could mis-classify is disabled for them.

This is the degraded case stated plainly: **codex and cursor get liveness coverage, not prompt coverage.** They are already launched with approval bypasses (`--dangerously-bypass-approvals-and-sandbox`, `--force --trust --approve-mcps`), so their most likely interactive frames are suppressed at launch.

**Enable procedure (deferred, not guessed):** on a work-profile host, `tmux capture-pane -p` a codex/cursor worker in both states, paste the two frames into `tests/crew.bats` as fixtures, add the engine's row to the signature table in `crew.sh`, and flip its matrix cells. The signature table is a single block in `crew.sh` for exactly this reason — enabling an engine is data, not logic.

### 6. Escalation: not leaving the roster ACTIVE forever

`DISPATCHER_PROTOCOL.md` partitions the roster `working`+`blocked` = **ACTIVE**, `pr_open`+`done`+`failed` = **TERMINAL / budget-freeing**, and drives park length off that partition. A watchdog `blocked` is therefore ACTIVE. For a **genuinely dead** worker the state never clears — clearance needs a byte change a dead pane cannot produce — so the crew stays permanently ACTIVE, the dispatcher re-arms a 270 s park indefinitely, and fan-out budget is never freed. Today's D0 `failed` frees it; a naive "post `blocked` instead" is a regression on that axis. **This section is what makes D0's softening (§Design 1, D0) affordable** — escalation is the sole remaining path to a budget-freeing `failed`, and after revision 2 every detector reaches it the same way.

**The release path: evidence-gated escalation.** A watchdog `blocked` episode escalates to `failed` when its **originating detector's evidence still holds continuously** for a further `--dead` (default 1800 s). Detail: `dead: <origin-prefix> unchanged for <n>s`, `source: "watchdog"`. Any clearance cancels the pending escalation. Escalation obeys the write invariant in §7 and ends the watchdog process.

**Why escalation rather than "an explicit dispatcher action with a deadline".** A deadline the dispatcher must honour is a rule enforced by nothing — the same class of failure as #31 itself, where a documented right path existed and was not taken. Escalation is enforced by the process that already holds the evidence.

**Why this is safe against the false-positive constraint.** Escalation is not a timer; it is a **second, independent, later evidence check** of the same conjunct set. For it to fire on a healthy worker, that worker must look dead for a full hour with zero bus activity and — for D2 — zero live subagent rows throughout. And the dispatcher was woken 30 minutes earlier with a `blocked` naming the exact pane; the §4 verify-then-act sequence resolves a false positive at step 4 long before the hour elapses. Worst case a false positive costs a `failed` the dispatcher can re-dispatch from, which is what D0 does today for a weaker reason.

Total exposure: ≤60 min ACTIVE before budget frees, against today's _forever_.

### 7. Identity, write ordering, and the terminal-state invariant

`stall-watch` appends raw JSON with `printf >> "$log"`, bypassing `crew status`'s state validation and its terminal-state dedupe. This design adds new _repeated_ writers (D1/D2/D3 clearance re-stamps of `working`), so the ordering rule must be stated rather than assumed. Without it, a clearance `working` landing after the worker posts `done`/`failed` makes `roster`'s `max_by(.ts)` report `working`, `report`'s `$last` report `working`, `rate`'s outcome fall through to `incomplete`, and `reap`'s done-only filter never see the worker again.

**INV-W0 — worker identity is branch-keyed and suffix-tolerant, on every read and every write.** This is stated first because every invariant below it is vacuous without it.

_The failure it prevents, measured._ `[bus]` The watchdog posts as `worker:feat/31-…#s1786338213-54181`; the worker posts as `worker:feat/31-…`. Every safety mechanism in this design reads the worker's own status events by `from`: INV-W1's terminal-state abort, the self-reported-`blocked` suppression (§2), the heartbeat damping of D2/D3 (§3), INV-W2's staleness check, and the escalation cancel (§6). Under exact-string equality **every one of them silently no-ops** — no error, no log line, no failing test unless a test is written for it. The design would ship looking correct and be running with its safety layer disconnected. This is the one defect in this spec that is invisible at every level except a bus dump.

_Why it must be handled defensively rather than by picking a shape._ Both shapes are live on this machine right now: the installed `dispatch` stamps `worker_id: worker:<branch>#s<session>` and passes it whole to `crew stall-watch`; the **checked-in** `dispatch.sh` passes a bare `<branch>` and the checked-in `crew.sh` prepends `worker:`. A watchdog cannot assume which `dispatch` launched it, and pinning one shape would break the other on the next rebuild.

_The rule._

- **Derive the branch once, at startup, from the positional argument**, tolerating either shape: strip an optional leading `worker:` and an optional trailing `#…`. Everything downstream keys off `branch`. This lives entirely in `crew.sh` — which this worker owns — so **no `dispatch.sh` change is needed for it**, and none is proposed (the `--engine` token in §5 remains the whole `dispatch.sh` diff).
- **Reads match branch-keyed and suffix-tolerant:** a status event counts as this worker's when `.from == "worker:" + $branch` **or** `.from | startswith("worker:" + $branch + "#")`. One shared predicate, used by `_bus_state()` and by every check listed above — not re-implemented per call site, so a future third id shape is one edit.
- **Writes normalise `from` to the bare `worker:<branch>`.** Provenance stays where it already belongs: `body.source = "watchdog"` (§4, layer 1). Collapsing `from` loses nothing a reader needs and buys the roster consequence below.

_The roster consequence, and why it made session 3 worse than it had to be._ `roster` groups with `group_by(.from)` and takes `max_by(.ts)` per group, so two `from` strings for one worker are **two rows**. `[bus]` The session-3 misfires did exactly this: one row showing `failed` (the watchdog's suffixed id) alongside a second row showing `working` (the worker's own) — for the same branch, both current. `roster` also derives the codename and the collision suffix from `.from | sub("^worker:";"")`, so the phantom row carries a _different_ codename than the worker it describes: the dispatcher is not looking at an obviously-duplicated row, it is looking at two differently-named workers disagreeing. Normalising `from` on write collapses this to one row whose `state` and `source` tell the true story.

**INV-W1 — the watchdog is a subordinate writer.** Immediately before **every** append (a `blocked`, a clearance `working`, an escalation `failed`), re-read the bus and **abort the write, exit 0** if any status event for this worker in this crew has state in {`done`, `failed`, `exited`}. Checked per write, **not** once per loop iteration — the whole hazard lives in the gap between sampling the pane and appending the line.

**INV-W2 — a clearance never overwrites a later worker statement.** A clearance `working` is appended only if the watchdog's own `blocked` is still the **latest** status for this worker. If the worker has spoken since, the correction is stale and is dropped.

**INV-W3 — one open watchdog episode per branch, checked on the bus, not in process memory.** Before appending a `blocked`, abort if the latest status for this branch (INV-W0 matching) is already a `blocked` with `body.source=="watchdog"` and the same `detail` prefix. Revision 1 got per-episode dedupe from process-local state plus D1's appearance conjunct; with that conjunct dropped, two watchdogs on one branch (open question 4) could each observe the same standing prompt on their own first two samples and both post. The check is a bus read, so it works **across** processes — which is the only place the hazard lives — and it is possible only because INV-W0 makes one watchdog able to see the other's writes at all.

Together these mean the watchdog can only ever write into a window where the worker's last word was `working`, its own `blocked`, or nothing — and never after the worker has finished. INV-W1 also makes the watchdog self-terminating after its own escalation `failed`.

INV-W1 closes the two-watchdogs-on-one-branch overlap (open question 4) for terminal states; **INV-W3 now closes it for `blocked` of every prefix** — a strictly wider fix than the appearance conjunct it replaces, which only ever covered `prompt:`.

### 8. Issue direction 3 — removing the interactive-question tool

**Partial yes: protocol instruction now, launch flag as a follow-up issue.**

- **Yes, protocol (this PR).** WORKER*PROTOCOL.md gains an explicit prohibition: never use an interactive question tool; the only ask-path is `crew status blocked` + `crew msg` + `crew await`. It is free, it lives in a file this worker owns, `gen-adapters.sh` projects it to all three engines, and it addresses the actual cause — the worker \_chose* the wrong channel while a documented right one existed.
- **No, launch flag (this PR).** Denying the tool on claude means editing the `claude` launch line in `dispatch.sh`, which another live worker owns and which the scope guard limits to the stall-watch spawn. It also needs a flag/tool-name pair verified against the installed CLI, and this design does not guess CLI surfaces.
- **It can never be uniform anyway.** The lever is per-engine: claude has a deny-list; codex and cursor express approval policy through the bypass flags already passed, and neither exposes a comparable knob verified here.
- **A hard tool removal is not obviously safer, either.** A model denied its question tool may narrate the question and end its turn — _less_ visible than a prompt frame. Under this design both outcomes are covered (prompt → D1, ended turn → D3), so the flag is a nice-to-have rather than the fix.
- **Follow-up issue** records: verify the deny mechanism against the installed claude CLI, decide launch line vs harness settings, confirm there is no codex/cursor equivalent.

### 9. Protocol and reader changes

**WORKER_PROTOCOL.md (liveness / blocked-state sections — owned by this worker):**

1. **Ban the interactive question tool** (§8), stated where the blocked-state contract already lives, with the reason: nobody is watching your pane.
2. **Heartbeat at the seams.** Re-stamp `crew status worker:<branch> working "<stage>"` at the pipeline seams the checkpoint-peek already defines. No new machinery, no new event kind. It does not wake the dispatcher, it damps D2/D3, and it makes `age_s` mean "time since last sign of life". Stated with its limit: **a seam heartbeat cannot fire from inside a tool call**, so it does not protect a subagent batch — the conjuncts do.
3. **Document the watchdog contract**: what may be posted on the worker's behalf, and that a worker which finds a watchdog `blocked` in its own history should re-stamp `working` and carry on.
4. **Note the startup wedge, both halves** (§11 and §Evidence session 3), as environment facts a worker may hit in a fresh worktree: the workspace-trust prompt must be answered before anything else happens, and a blocked `.envrc` leaves the pane with no devshell. Stated as a note, not a detector.

**`crew.sh` reader changes (owned by this worker):**

- `roster` gains `detail` (truncated to 120 chars) and `source` per row — additive.
- `rate` gains **`watchdog_blocked_count`**, and `blocked_count` is narrowed to statuses **without** `body.source=="watchdog"`.
- **No `roster`/`rate` change is needed for INV-W0.** Both group on `.from`, and the watchdog now writes the bare `worker:<branch>` those readers already expect, so the phantom-row split disappears at the writer. Deliberately **not** doing the alternative — teaching every reader to normalise `#…` suffixes — because that is a wide change across `roster`, `rate`, `report` and `reap` to compensate for one writer, and it would leave the historical suffixed rows in `events.jsonl` retroactively re-grouped, changing what old runs mean. Those rows stay as they are: visible, wrong, and explained by this spec.

**Why split rather than simply exclude.** `blocked_count` feeds a persistent, append-only, cross-crew ratings store at `${XDG_DATA_HOME}/crew/ratings.jsonl` whose rows are compared across runs — including rows written before the watchdog existed. Changing that field's _population_ in place would make old and new rows silently non-comparable in the same field; adding a field leaves historical rows simply absent (`null`), which every reader there already tolerates. And the liveness signal is worth keeping: `watchdog_blocked_count` is direct evidence of how often a model/tier wedges.

**Readers verified against the code, individually** (replacing the previous blanket claim):

| Reader                     | Effect of watchdog events                                                                                                        | Action                                          |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------- |
| `report` duration          | derived from the **first** `working` (`sort_by(.ts)[0]`) — clearance re-stamps do not move it                                    | none                                            |
| `report` outcome (`$last`) | a watchdog `blocked`/`failed` that is genuinely last renders correctly; a _stale_ clearance landing last would lie               | fixed by INV-W1/W2                              |
| `rate` `blocked_count`     | **contaminated** — counts every `blocked` regardless of source                                                                   | **narrowed**, plus new `watchdog_blocked_count` |
| `rate` `outcome`           | keys off `$pr` then `$ls`; an escalation `failed` correctly yields `failed`; a stale clearance would yield `incomplete`          | fixed by INV-W1/W2                              |
| `rate` run keying          | keyed off the dispatch event; extra events do not split runs                                                                     | none                                            |
| `roster` `state`           | `max_by(.ts)` — correct under INV-W1/W2                                                                                          | none                                            |
| `roster` `age_s`           | now refreshed by watchdog writes; kept meaningful by one-post-per-episode (§3)                                                   | none                                            |
| `roster` `prev_state`      | last non-`exited` state; a watchdog clearance `working` can become `prev_state` for a spurious `exited` — the desired resolution | none                                            |
| `reap`                     | candidate filter is `done`-only; the watchdog never posts `done`                                                                 | none                                            |
| `watch`                    | wakes on `blocked`/`failed`, ignores `working` — the property clearance relies on                                                | none                                            |
| `inbox`                    | `kind=="msg"` only; this design adds zero `msg` events                                                                           | none                                            |

**DISPATCHER_PROTOCOL.md ownership, resolved.** The edit is **confined to the one `stalled:` liveness paragraph** (~lines 154–165): it gains the five `detail` prefixes, `source: "watchdog"`, the "do not `crew reply` to a watchdog-blocked worker" rule, the §4 verify-then-act sequence, and **the correction that `stalled:` now arrives as `blocked` rather than `failed`** — the existing paragraph tells the dispatcher to treat a `stalled:` `failed` as a recovery trigger, which is precisely the instruction session 3 would have turned into three dead workers. That file is claimed by neither of the two other live workers (one owns `dispatch.sh`/`tests/dispatch.bats`, the other `gen-adapters.sh`/`tests/adapters.bats`/the skill body), and the edit is called out in the PR body. The header's "Artifacts touched" list and this statement are the same claim; there is no separate open question. Independently, the design does **not** depend on the edit landing: a live dispatcher does not re-read its protocol mid-session, so the reserved prefixes and `body.source` carry the disambiguation in the event itself (§4, layer 2).

### 10. Rejected alternatives

- **Keeping D0's `failed` as "unchanged behaviour, out of scope"** (revision 1's decision). Refuted by session 3: 3/3 false positives on live workers, with an invented diagnosis in the detail. "Pre-existing" describes the bug's age, not its severity, and this is the same bug class the spec exists to fix. (§Design 1, D0.)
- **Softening D0's residual `stalled:` branch to `blocked` _without_ wiring it into §6 escalation.** Would trade an unrecoverable false positive for a permanently-ACTIVE roster row and a never-freed fan-out budget — Decision 9's failure, re-introduced. The softening is only sound _because_ escalation exists.
- **Keeping D1's appearance conjunct.** It is unsatisfiable by the workspace-trust prompt (present from sample 1), which is the only frame with measured production occurrences. (§Design 1, D1.)
- **Restating the appearance conjunct as "appeared OR present since the first sample".** Logically equivalent to plain presence; keeps words that imply a discrimination the code does not make. (§Design 1, D1.)
- **Keying D1 on `Esc to cancel` as a third anchor.** Present only in the option-select family, so it catches nothing the two `Enter to …` anchors miss, while matching any pane whose bottom line mentions cancelling.
- **Teaching every reader to normalise `#s<session>` suffixes** instead of normalising at the one writer. Wide change for one writer's defect, and it would retroactively re-group historical rows. (§Design 9.)
- **A detector for the blocked-`.envrc` condition.** Environment defect, not a liveness signal; thin evidence; the generic D0 fix already yields a recoverable state on the panes it produces. (§11.)
- **Filesystem/git activity as liveness.** Strictly more evidence, but it does not discriminate the case that matters — a worker watching CI touches nothing either — and it doubles the watchdog's surface for a signal the pane already carries.
- **Numeric parsing of the token count / elapsed time.** Rounding to 0.1k and multi-unit durations make it fragile, and every parse failure is a new decision branch.
- **Widening the meter ERE to accept `...` / `-`** (#31's transcription). Manufactures false positives on prose; A2's gate is the safe way to be proven wrong. (§Design 1, D2.)
- **Relaxing D1's footer anchor to "the last 10 lines".** Makes this change's own prompt fixtures in `tests/crew.bats` a false-positive source.
- **Requiring only "subagent timers are advancing" rather than "no subagent row at all".** Less silent than presence-only, and unnecessary — A1's capture shows the row's own timer already advances throughout a healthy batch, so presence-only loses nothing and stays the simpler rule.
- **Scraping the question text into the `detail`.** Unstable wording; a truncated question invites a confidently wrong answer.
- **Posting a `msg` on the worker's behalf.** Fabricates a conversation with a process that is not listening.
- **Failing (rather than blocking) on **first** D2/D3 evidence.** `failed` is unrecoverable-by-reply and, on a false positive, destroys an hour of real work. Escalation (§6) reaches `failed` only via a second independent evidence check.
- **Never escalating.** Pins the roster ACTIVE forever and never frees fan-out budget (§6).
- **A cheaper D2 threshold (e.g. 300 s).** Would fire on every `bats`/`nix flake check`/`crew await` in the harness.

### 11. The direnv wedge — noted, not detected

`[field]` Every session-3 worktree also came up with ``direnv: error .envrc is blocked. Run `direnv allow` ``, so every pane had no devshell. **Decision: no detector. A documented note in `WORKER_PROTOCOL.md` plus a callout in the PR body.** Reasons, in order of weight:

- **It is not a liveness signal.** It is a deterministic environment defect with a known one-line fix, in a component this design does not own. A liveness watchdog that also reports environment misconfiguration is two tools; the second belongs in `dispatch`'s worktree setup, where the cause is (an unallowed `.envrc` in a fresh worktree), not in the pane sampler.
- **The evidence is thin for detection and thick for the cause.** One dispatch, three worktrees, one string. Ample to state the cause and act on it in setup; nowhere near enough to define a _frame signature_ that must be silent on every healthy pane — and this design's own rule is that a guessed signature is a false-positive generator (§5).
- **The honest tension, stated rather than hidden.** The panes it produces are exactly the static-pane shape D0 misfires on, so it is causally adjacent to R1. But D0's fix is not "recognise direnv" — it is "classify the frame, and post a recoverable state when unsure". Under the new D0 a direnv-wedged pane that is _also_ prompt-parked posts `prompt:` (correct), and one that is merely static posts `blocked`/`stalled:` (recoverable, and the dispatcher's mandatory pane glance shows the direnv line immediately). The generic fix already covers the specific case; a dedicated detector would add surface for no additional recovery.
- **What is written down instead.** The PR body names the condition, the exact string, and that it was present in 3/3 worktrees, so whoever owns worktree setup has the evidence without re-deriving it. If it recurs across dispatches it earns a `dispatch` issue — not a detector here.

---

## Failure modes and safety margins

| Detector   | False positive                                                                          | Mitigation                                                                                                                                                                                                                                               | Residual cost                                                                              |
| ---------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| D1         | pane shows the phrase incidentally (this spec's own fixtures)                           | footer must be the **last non-empty line** (A3: a non-parked pane ends on the input box, never on transcript text), option line within 6 above it, meter absent, 2 consecutive samples. The appearance conjunct is **gone**; geometry carries this alone | one recoverable `blocked`, auto-cleared. Residual shape enumerated in §Design 1, D1        |
| D1         | widened footer set (`Enter to confirm`) matches prose                                   | re-checked against all three surviving conjuncts (§Design 1, D1 conjunct 1): geometry + option line + meter-absent are unsatisfiable by prose                                                                                                            | none identified                                                                            |
| D1         | a human is about to answer the prompt                                                   | 2-sample confirmation; clearance re-stamp fires when they do                                                                                                                                                                                             | one wake, self-correcting                                                                  |
| **D1/D0p** | **two watchdogs on one branch both see the same standing prompt**                       | **INV-W3** — bus-checked one-open-episode-per-branch (§7). Replaces the appearance conjunct's cross-process immunity, and covers every prefix, not just `prompt:`                                                                                        | none                                                                                       |
| **D2**     | **healthy deep worker inside a 3-concurrent subagent batch or a parallel review batch** | **no-live-subagent-row conjunct (unconditional)** + 1800 s + clock-changed + `blocked` not `failed`                                                                                                                                                      | **zero events** — this is the headline negative test                                       |
| D2         | long tool call (CI watch, big build) with no streaming                                  | 1800 s threshold; any bus event resets it; §4 step 4 stops the kill                                                                                                                                                                                      | dispatcher glances at one pane                                                             |
| D2         | frozen capture (copy-mode scrollback)                                                   | clock string must have **changed** — a frozen capture is silent by rule                                                                                                                                                                                  | none (silence)                                                                             |
| D3         | human in copy-mode for 30 min on a healthy pane                                         | 1800 s threshold; `blocked`; auto-clears on the next byte change                                                                                                                                                                                         | one wake; a human is at the pane by construction                                           |
| E          | escalation on a false-positive episode                                                  | requires the origin detector's evidence to hold for a further 1800 s, re-checked; any clearance cancels; the dispatcher had a pane-naming wake 30 min earlier                                                                                            | one `failed`, re-dispatchable                                                              |
| **D0**     | **worker parked on a startup prompt, reported dead** — 3/3 workers, session 3           | **classify before judging**: prompt branch posts `blocked`/`prompt:` and wins over the silence branch; residual silence branch softened to `blocked` and released via §6 escalation; the invented `(suspected startup/indexing hang)` diagnosis deleted  | **one recoverable `blocked` naming the pane**, where today it is an unrecoverable `failed` |
| D0s        | genuine startup hang under-reported (`blocked`, not `failed`)                           | §6 escalation re-checks the same evidence after `--dead` and posts `failed`                                                                                                                                                                              | ≤35 min of ACTIVE roster before budget frees                                               |
| **all**    | **safety layer silently disconnected by an id mismatch**                                | **INV-W0** — branch-keyed, suffix-tolerant matching on every read, normalised `from` on every write (§7); asserted in both directions by bats                                                                                                            | none once asserted; **before the fix, total and silent**                                   |

| Detector | False negative                                                                         | Accepted because                                                                                                                                                                                                                                                                                       |
| -------- | -------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **D2**   | **a turn that dies with a subagent row still painted**                                 | the cost of the alternative is flipping healthy deep workers; A1's capture establishes the healthy baseline (row timer advances throughout) but no capture has watched one die mid-batch, so a future revision can add a rows-static detector once that evidence exists                                |
| ~~D1~~   | ~~a prompt already on screen at the watchdog's first sample~~                          | **Closed in revision 2.** This was the design's accepted blind spot and it turned out to be the _only_ case with measured production occurrences (3/3, session 3). The appearance conjunct is dropped; D0p covers it inside the startup window at `--stall`, D1 outside it at `grace` + 2 × `interval` |
| D1       | a prompt family whose footer contains neither `Enter to select` nor `Enter to confirm` | signature-based detection is closed-set by construction; falls through to D0s (`blocked`, recoverable) inside the window and D3 outside it — a false negative on the _classifier_, never a false positive                                                                                              |
| D1/D0p   | the trust footer is not the last non-empty line (A5's inherited geometry is wrong)     | falls through to D0s/D3, both recoverable; adding the substring cannot make anything fire that would not have fired before (§A5)                                                                                                                                                                       |
| D1       | Claude Code changes the frame wording                                                  | D3 still catches the parked pane; the bats fixtures make the rot fail CI, not production                                                                                                                                                                                                               |
| D1/D2    | codex, cursor                                                                          | deliberate (§5) — a guessed signature is a false-positive generator, and D3 still covers them                                                                                                                                                                                                          |
| D2       | the meter freezes wholesale (renderer dead)                                            | the pane is then byte-identical → D3                                                                                                                                                                                                                                                                   |
| all      | the watchdog process itself dies                                                       | degrades to today's behaviour; `--max-life` bounds the opposite failure; N=3 sample-failure quorum stops a tmux hiccup from disarming it                                                                                                                                                               |
| D3       | a stalled worker whose pane animates                                                   | D2's case on claude (subject to the subagent gap above); on codex/cursor an acknowledged gap                                                                                                                                                                                                           |

**The margin, stated once:** every rule requires a _conjunction_ of independent conditions, posts a _recoverable_ state first, is _deduped_ per episode (now bus-checked, INV-W3), _auto-clears_, and reaches `failed` only through a second later evidence check. **After revision 2 this holds for D0 as well: the watchdog has no path to `failed` that is not a second, later evidence check.** The worst outcome of any single false positive, handled per §4, is one dispatcher wake and one pane glance.

**The evidence cuts both ways, and the design is built on both edges.** Session 3 is simultaneously the strongest proof that a real detector is needed — three workers wedged on an answerable question for six minutes with the bus reporting nothing useful — and the strongest proof that the existing one already destroys trust, by reporting `failed` on all three. A design that reads only the first half ships a more eager detector; one that reads only the second half ships nothing. This spec's response to both halves is the same move: **detect more frames, post weaker states.**

## Sink discipline

Unchanged and verified against the code: `metrics:<crew>` / `retro:<crew>` are `kind:"msg"` events whose `to` is a synthetic sink, and both `watch` (`.to==$me or .to=="*"`) and `inbox` filter on that. This design **adds no `msg` events at all** (Decision 5) and does not touch `watch`, `inbox`, or their filters. The only new writes are `kind:"status"` events addressed to `dispatcher:<crew>`, identical in shape to what `stall-watch` already appends. The existing `inbox: does not deliver a metrics-addressed message to the dispatcher` test stays green, and a new test asserts the watchdog writes **zero** `kind:"msg"` events.

## Test seam and plan

`CREW_STALL_SAMPLE_CMD` remains the seam, contract unchanged (stdout = pane text, exit code = pane liveness). One refactor: `_sample()` returns the **raw text** and the caller hashes it, because the new detectors read the text while D0/D3 read the hash. A test stub emits a scripted frame sequence from a counter file and exits non-zero at the end. Every threshold is a flag, so tests run in seconds with `--grace 0 --interval 1 --idle 2 --dead 2 --window 0 --max-life 20`; `--window 0` disarms D0 to test the new detectors in isolation, and a D0 test uses `--window 60 --stall 1 --idle 999`. No tmux, no timing flake beyond a couple of seconds.

Fixtures pinned in `tests/crew.bats`: the meter/decoy table in §Design 1 (each with its explicit match/no-match assertion), the live prompt footer, #31's transcribed footer and meter, the **real captured prompt frame from pane %118** (A3/A4's gate capture — no longer a synthetic placeholder, pinned verbatim), the **real captured parent-pane-with-subagent-rows frame from pane %129** (A1's gate capture, pinned verbatim in place of the earlier placeholder), and — new in revision 2 — the **workspace-trust frame** (`[field]`, §Design 1 D1, pinned verbatim: `Quick safety check: …` / `> 1. Yes, I trust this folder` / `2. No, exit` / `Enter to confirm`) plus a **meter-less non-prompt static frame** for the D0s branch.

**Identity fixtures.** INV-W0 needs the bats cases to use _both_ id shapes, since asserting one proves nothing about the other: a run with `crew stall-watch worker:<branch>#s<id>` against a bus whose worker events are bare `worker:<branch>`, and a run with `crew stall-watch <branch>` against a bus whose events carry the `#s` suffix. The scripted-frame stub is unchanged; only the invocation and the seeded bus differ.

`tests/crew.bats` today has **no** stall-watch coverage at all; these are the first.

## Acceptance criteria

1. A claude worker rendering an interactive prompt reaches the bus as `blocked` within **≤ 75 s** (`grace` + 2 × `interval` at defaults), `crew watch` wakes, and `crew roster` shows `blocked` with a `prompt:` detail and `source: watchdog`. **This holds whether the frame appears mid-run or is already on screen at the watchdog's first sample.**
2. A claude worker whose turn dies (clock advancing, token string static, no subagent rows) reaches the bus as `blocked` within `--idle` + one interval.
3. **A healthy long-running worker is not flipped** — six negative tests, all asserting **zero events**:
   a. meter with rising clock _and_ rising tokens across > `--idle`;
   b. **meter present, clock rising, token string static, no heartbeat, live subagent rows present throughout** (the deep-worker subagent batch — the measured case, confirmed by A1's live capture);
   c. **the same shape as (b), asserted against D1's silence too** — meter present, clock rising, token string static, live subagent rows present, no heartbeat: D1 stays silent (conjunct 3 fails, since the meter is present) and D2 stays silent (the subagent-row conjunct fires). Replaces the former "meter absent" framing of this test, which A1's capture shows is not the shape that actually occurs;
   d. **meter absent, live subagent rows present, no heartbeat** — kept as a cheap extra, even though A1 shows this is not the shape observed during a real subagent batch;
   e. rising clock, static tokens, plus a `working` heartbeat mid-window;
   f. a worker in self-reported `blocked` with a static pane.
4. A capture whose **last non-empty line** is not the prompt footer produces no D1 event, even when the footer appears 3 lines up (the fixture-in-scrollback case). Asserted for **both** footer substrings. The former "matches on the first sample → no event" assertion is **deleted with the appearance conjunct**, and its inverse is asserted instead: a frame matching from sample 1 fires on sample 2.
   4a. **The session-3 regression, named as such.** A pane that is byte-static across the whole `--stall` window inside `--window`, carrying the pinned workspace-trust frame, produces exactly one event: `blocked`, `source: watchdog`, `detail` prefixed `prompt:`. The test asserts **no event with state `failed`** and **no `detail` containing `stalled:`** anywhere in the run. A second case, same staticness with a non-prompt frame, produces `blocked` with `detail` matching `stalled: no output for <n>s` as a **full string** — so a reintroduced `(suspected …)` diagnosis fails CI.
   4b. **The trust frame fires D1 outside the startup window too** (`--window 0`), proving the footer widening is in D1's own signature set and not only in D0's classifier.
5. A prompt that clears produces a `working` `prompt: cleared` re-stamp, re-arms the detector, and cancels a pending escalation; no duplicate `blocked` while a single episode persists.
6. **A terminal state posted between sample and write produces zero watchdog events** (INV-W1), and a clearance whose worker has spoken since is dropped (INV-W2).
   6a. **INV-W0, asserted in both directions** — the criterion that would have caught the session-3 defect:
   a. a worker posting as bare `worker:<branch>` **suppresses** a watchdog invoked with a `#s`-suffixed id (self-reported `blocked` silences the new detectors; a terminal `done` aborts every write and exits);
   b. a worker posting as `worker:<branch>#s<id>` does the same for a watchdog invoked with the bare branch;
   c. a `working` heartbeat under either shape damps D2/D3 under the other;
   d. **every watchdog-written event carries `from == "worker:<branch>"` exactly** — no `#` — so `crew roster` shows **one row** for the branch, not two. Asserted on the roster output, not just on the log line, because the phantom row is the symptom the dispatcher actually sees.
   6b. **INV-W3** — a second `stall-watch` started against a branch that already has an open watchdog `blocked` of the same prefix posts nothing.
7. A watchdog `blocked` whose evidence persists a further `--dead` escalates to `failed` with a `dead:` detail; a clearance before that produces no `failed`. **Asserted for a D0s-origin episode as well** — that path is what replaces D0's budget-freeing `failed`, so it is load-bearing, not incidental.
8. `--engine codex` fed a claude-shaped prompt frame produces **no** D1/D2/D0p event; D0s and D3 still work for it — the static pane posts `blocked`/`stalled:`, never `failed`.
9. The watchdog survives 2 consecutive sample failures and exits after the 3rd.
10. The watchdog exits promptly on `done`/`failed`/`exited` and on a confirmed-gone pane; it does **not** exit on `pr_open` or on a `working` heartbeat.
11. The watchdog writes zero `kind:"msg"` events; `crew inbox dispatcher:<crew>` is unaffected.
12. `crew roster` rows carry `detail` (≤120 chars) and `source`.
13. `crew rate` emits `watchdog_blocked_count`, and `blocked_count` excludes `source=="watchdog"` — asserted against a bus containing both kinds of `blocked`.
14. Protocol changes land in `adapters/core/protocols/**` and are regenerated with `scripts/gen-adapters.sh`; no CI drift.
15. `bats tests/` green, `nix flake check` green, `shellcheck` clean.

**Ship gates (blocking, tracked separately from the criteria above):** A1, A3, A4 and A5 are settled (A5's geometry half is inherited from A3, and its failure direction is silence — not a gate, see the assumptions table). **A2** is the one remaining gate before D2's claude cell is `yes`; D1's and D0p's claude cells are already `yes`. Open question 2 before D3 is relied on for claude (it is unconditionally sound for codex/cursor). **INV-W0 is not a gate but a blocker: no detector may ship before it, because every other guarantee reads the bus through it.**

## Open questions for the plan

1. **Does `tmux capture-pane -p` follow copy-mode scrollback?** If not, D3's copy-mode false positive disappears entirely. Either way the design holds; the failure-mode table gets tighter.
2. **Is a truly idle claude pane byte-stable over 30 minutes?** If the input box animates anything, D3 never fires on claude. Verify with one 30-minute capture before relying on D3 for claude; codex/cursor coverage does not depend on the answer. (Same capture session as A3.)
3. **`--idle` 1800 s and `--dead` 1800 s.** Confirm `--idle` against the slowest real tool call (`nix flake check` cold, `gh pr checks --watch`) and raise it if a routine call approaches it. `--dead` trades roster-ACTIVE exposure against escalation risk; 1800 s is a starting point, not evidence.
4. **Two watchdogs on one branch** (a re-dispatch onto the same branch while the old pane lives). **Mostly resolved in revision 2, and the question is now narrower.** The old one exits on a confirmed-gone pane; the overlap cannot double-post a terminal state (INV-W1) and cannot double-post a `blocked` of any prefix (INV-W3, which replaced the appearance conjunct's narrower coverage). What remains is a **race**, not a gap: two watchdogs whose bus reads interleave between check and append can still both write. Decide in the plan whether the bus check is sufficient (a duplicate `blocked` is idempotent for the dispatcher, so probably yes) or whether it needs the per-branch marker file / the `_lock_acquire` helper `rate` already uses.
5. **Does `crew status`'s validation belong in front of the watchdog's writes?** The watchdog appends raw JSON precisely to bypass the terminal-state dedupe, but session 3 showed a raw writer can also bypass _identity_ conventions and produce a phantom roster row that no reader rejects. INV-W0 fixes the specific case at the writer; whether the general lesson is "route watchdog writes through `crew status` with a `--source` flag" is a plan-level call with a cost (dedupe semantics) this design has not priced.

## Effect on #32 (out of scope, per the task)

Mildly **easier**, not harder. `body.source` gives every reader a way to tell machine-posted states from worker-posted ones, which is the distinction #32's pre-reap fold needs; and no change here touches `reap`'s candidate filter (still `done`-only). One incidental observation for whoever takes #32: `pane_current_command` reads `.claude-wrapped` on this machine, so the `"claude $wtpath"*` pattern matching in `roster`'s exited-resolution and in `reap`'s "an engine is still running there" guard **does not match a live claude worker** — a latent bug in the same family, deliberately not fixed here.

---

## Post-cap critic resolutions (BINDING — these override any earlier text they contradict)

The 2-revision cap was spent when the final `spec-critic` pass returned `revise` with four blocking
findings. Each was verified against the code and the evidence file and each was found correct, so
rather than loop a third round the resolutions are recorded here as binding decisions. **The
escalation is surfaced verbatim in the PR body under `## Escalated`** — it is not silently dropped.
Every resolution below _reduces_ the design; none adds surface.

### C-1 — `prompt:` episodes never escalate to `failed` (overrides §6, detector row E, criterion 7)

A prompt frame still on screen is evidence that **nobody answered yet**, not evidence of death. §6's
safety argument ("to fire on a healthy worker it must look dead for a full hour") is false for the
prompt branch: an unanswered answerable question looks exactly like that for as long as the dispatcher
is slow. Escalating it would reproduce session 3 with a 30-minute delay — the precise failure this
spec exists to eliminate.

**Decision:** episodes whose origin prefix is `prompt:` are **exempt from escalation**. They stay
`blocked` until cleared. Escalation to `failed` remains available only to `quiet:` and `turn-stall:`
origins. The budget consequence is accepted and stated plainly: an unanswered answerable question _is_
active work, and holding the roster ACTIVE is the correct representation of it. The dispatcher's
release valve is to answer the prompt (§4 step 2) or kill the window — both already documented.

### C-2 — D1 is armed for the watchdog's whole life; D0p is deleted (overrides §Design 1 D0, §2, the false-negative table, criteria 1/4a/4b)

The spec stated D1's arming window three different ways. Resolving toward "armed lifetime-wide" makes
D0p unreachable — D1 confirms in 2 samples (~75 s) where D0p needs 300 s of byte-staticness, and
INV-W3 suppresses the second post — so D0p was dead code that only appeared to add safety.

**Decision:** D1, D2 and D3 are armed from `grace` onward, with no dependence on `--window`. `--window`
bounds **only** D0's silence branch. **D0p is deleted as a separate detector**; its intent survives as
a _negative check_ inside D0's silence branch: post `stalled:` only when the static pane does **not**
match the prompt frame. So a static prompt pane is classified by D1 (`blocked`/`prompt:`) and D0 stays
silent on it, which is exactly the session-3 fix with one detector instead of two. Latency for the
already-on-screen case is `grace` + 2 × `interval` (**≤ 75 s** at defaults), and criterion 1's figure
stands; the false-negative table's "D1 outside the startup window" clause is struck.

### C-3 — every bus read is scoped to the current run (overrides §7 INV-W1, INV-W3, §2 suppression)

`events.jsonl` is append-only per repo and re-dispatch onto the same branch is a first-class flow
(`rate` partitions per-branch runs by `$t0`/`$t1` for exactly this reason). Reading "any status event
for this worker" therefore lets the _previous_ run's `failed`/`exited` mute a freshly started
watchdog for its entire life — silently, at exit 0. Since §4's documented recovery for every watchdog
event is "kill and re-dispatch", the design would have guaranteed that the run after any watchdog
event has no watchdog at all.

**Decision:** the watchdog records its own start timestamp at launch, and INV-W1, INV-W3 and the
suppression/damping predicate consider **only status events with `.ts >= <watchdog start>`**. Acceptance
criterion: _a bus carrying a terminal state from a previous run of the same branch does not mute a
newly started watchdog._

### C-4 — A2 is demoted from ship gate to a documented false-negative risk (overrides the gate table, §5, Ship gates)

An unmet gate flips its detector's matrix cell to `no`, so A2 (the ≥1 h meter rendering, still
uncaptured) would have shipped D2 disabled on **every** engine — leaving #31's symptom 2 with zero
coverage while acceptance criterion 2 asserts the opposite. It is also inconsistent with the spec's own
treatment of A5, which is explicitly _not_ a gate "because the failure direction is silence". A2's
failure direction is likewise silence: an unmatched ≥1 h meter makes D2 quiet, never eager.

**Decision:** A2 is a documented false-negative risk, not a gate. D2's claude cell is `yes` on ship.
The meter ERE keeps its hours alternative, the `Considering… (1h 20m · ↓ 28.0k tokens)` reconstructed
form stays pinned as a fixture, and the PR body records that the ≥1 h rendering is unconfirmed.

### C-5 — UTF-8 locale is a stated requirement of the signature EREs (from the critic's notes; load-bearing)

The live-subagent-row ERE and the option-marker class match multibyte glyphs (`◯` U+25EF, `❯` U+276F)
through single-character bracket expressions. Under `LC_ALL=C` a bracket expression consumes one
**byte**, so the subagent row would never match and D2 would lose its only measured false-positive
guard — silently.

**Decision:** make the classes multibyte-safe rather than depending on ambient locale — use `[^[:alnum:][:space:]]+`
(one or more) for the glyph position and an alternation rather than a bracket set for `❯`. Cover it
with a fixture assertion that runs under the test suite's own locale.

### C-6 — smaller corrections adopted from the critic's notes

- **Provenance narrowed.** The blanket claim "every verbatim string below is quoted from
  `EVIDENCE-2026-08-10.txt`" is false for session-1 strings, which are transcribed in this spec rather
  than pinned in that file. Session 2 and 3 material is pinned; session 1 is transcribed. Session 1's
  longest observed turn is `19m 48s`, not `19m 12s`. Not every meter-less pane ended on
  `-- INSERT -- …` — `%126` ended on a bare `⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`, so
  the anchor is "the input box", of which `-- INSERT --` is one rendering.
- **`DISPATCHER_PROTOCOL.md` edit extended by one clause** at the "a worker that's `blocked` … answer
  promptly with `crew reply`" bullet, so a dispatcher re-reading the doc is told that a
  watchdog-posted `blocked` has no question behind it and `crew reply` is a no-op there.
- **The `dispatch.sh` spawn-line edit is two tokens** (`--engine "$agent"`), not one; the PR body says so.
- **A clearance `working` may be a worker's _first_ `working`** (the startup-prompt case, where the
  worker has posted nothing yet). `report` derives duration from the first `working`, so the effect is
  a shortened duration — harmless in direction, recorded for honesty.
- **Locate `stall-watch` by symbol, not line.** `WORKER_TASK.md` cites `crew.sh:792`; it is at line 572
  in this worktree.
