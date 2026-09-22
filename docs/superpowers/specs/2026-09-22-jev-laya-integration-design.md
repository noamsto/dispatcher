# JEV / Laya "System One" models — integration design

**Issue:** #261 · **Date:** 2026-09-22 · **Status:** design — verdict recorded

## Question

The human shared two links — TypeSafe's launch post for Jev
(typesafe.ai/blog/introducing-system-one-models-and-jev) and a third-party
CoreML port of Laya (github.com/mizorewww/laya-coreml) — and asked whether the
dispatcher can and should integrate "JEV / Laya" System One models. This note
settles what these things are, maps every plausible integration point onto a
real seam in this repo, weighs the trade-offs, and ends in one verdict:
integrate, experiment, or don't.

## What they are

### JEV (TypeSafe AI)

TypeSafe's first "System One model" — a non-generative decision model that
answers typed questions (`choice`, `score`, `noul` = yes/no probability) over
a `state` blob, in one API call, with calibrated probabilities and a
`confidence` field
(typesafe.ai/blog/introducing-system-one-models-and-jev, dated 2026-09-22,
i.e. launched today; "available in early access").

Access is proprietary hosted API only — `POST https://api.typesafe.ai/v1/systemone`,
`Authorization: Bearer <key>`. No self-host, no open weights
(docs.typesafe.ai/models.md, docs.typesafe.ai/api.md). Per the quickstart, the
key comes from console.typesafe.ai, the Python SDK installs via
`pip install typesafe-sdk`, and the tested version is `jev-1.13.0`
(docs.typesafe.ai/introduction/quickstart.md); a bash `curl` + `jq` call is
also sufficient for the REST shape. The `jev-latest` alias moves on release —
docs advise pinning if confidence thresholds are tuned. Limits: 64k
tokens/request, 32k for state + longest question, text only, choice ≤255
options, score 2–10 levels.

Cost is $0.042/MTok input, output free (blog, docs.typesafe.ai/models.md).
Rate limits: 1,200 rpm / 250k tok/s, "adjusting dynamically."

Latency, vendor claim: 70–500 ms end-to-end (blog). **Competitor-reported**
(Laya's README, which benchmarks against Jev to sell Laya): 236–276 ms p50
(github.com/NandhaKishorM/laya). Ours: none.

Quality, vendor claim: "similar levels of intelligence on System One tasks"
to frontier LLMs; workflow evals "193.6× faster, 444.6× cheaper" — the
authors themselves flag these evals as vendor-built and biased (blog).
**Competitor-reported** (same Laya README): Jev scores 0.727 argmax accuracy
on Laya's own 400-case typed-decisions set and 0.870 on Banking77 (77
labels). Ours: none. There is no third-party evaluation of Jev anywhere in
the sources.

Known limits, vendor-documented
(docs.typesafe.ai/model-jaggedness/jev-1.13.md): literal reading; unreliable
counting; dates read as text; degrades with large irrelevant state; **does
not treat adversarial/injected content as hostile**; cannot generate text
("will not work well and will be very slow").

Agent positioning (docs.typesafe.ai/introduction/coding-agents.md): Jev is
meant to be used _inside software the agent writes_, not as the agent's own
model — "There is no `model: 'jev-latest'` setting"; it "does not generate
text, write code, or hold a conversation." Consistent with that, TypeSafe
ships a Claude Code plugin (`claude plugin install typesafe@typesafe-ai`)
that teaches an agent to _write code that calls Jev_, not to run on Jev
(docs.typesafe.ai/agent-skill.md).

Data handling (docs.typesafe.ai/legal.md): the privacy policy commits not to
train on user data; a DPA covers retention; ZDR is available for enterprise.
See Trade-offs below for how this compares to today's egress baseline.

### Laya (Convai Innovations) — upstream

Open-weights, Apache-2.0, non-autoregressive encoder decision model in the
same category — positions itself explicitly as the open competitor to Jev
(README benchmarks against "TypeSafe Jev": "7.8× faster", 0.766 vs 0.727
typed-decisions accuracy). github.com/NandhaKishorM/laya,
huggingface.co/convaiinnovations/laya.

Checkpoints: `laya` (ModernBERT-large, 421M params, 512 ctx),
`laya-multilingual` (mmBERT-base, 322M, 1024 ctx), `laya-typed-decisions`
(421M, 1024 ctx). Same three primitives (`choice`, `score`, `noul`). Trained
with RLCD against proper scoring rules.

Runtime: Python 3.10+, `pip install laya`, PyTorch 2.14+ / transformers 5.x;
CPU or CUDA. Measured 32.8–39.5 ms/question on a Tesla T4; **CPU ~193–464
ms** (model card). No ROCm or Apple-silicon path is documented upstream (an
MLX sibling exists; laya-coreml is covered separately below).

Accuracy per checkpoint (README, BENCHMARKS.md — **self-reported by Laya's
own authors**): on Laya's own 400-case typed-decisions set, the
`laya-typed-decisions` checkpoint scores 0.766 argmax (0.471 soft accuracy
vs Jev's 0.580 against teacher distributions); the base checkpoints `laya` /
`laya-multilingual` are near chance zero-shot on that same set (0.362 /
0.352 vs 0.318 random; both below the 0.461 majority baseline). The
typed-decisions checkpoint is therefore the only local candidate for
rubric-style decisions — whether _our_ rubric (tier/engine/effort over a
task doc) falls inside its distribution is unmeasured; its training set is
Laya's own 30k-question typed-decisions corpus, not agent-dispatch routing.
Fine-tuning: a Kaggle notebook, 2×T4, 4–5 h over 30k questions.

Known limits (README, BENCHMARKS.md): weak on high-cardinality labels
(Banking77 0.425 vs Jev's 0.870); ordinal scores weakest; "over-confident as
shipped" (ECE 0.466 → 0.081 only after per-question-type temperature
refit); language collapse on low-resource languages at high stated
confidence.

Activity: 13.3k stars, 63 commits, 30 open issues.

### laya-coreml (mizorewww)

An _independent third-party port_ of Laya to Apple CoreML ("Independent port
of Laya, by Convai Innovations and contributors"). Its README makes **no
reference to TypeSafe or JEV**. github.com/mizorewww/laya-coreml.

Platform: **macOS only** — "Apple Silicon · macOS 15+ · Python 3.11–3.13."
No Linux path. The ANE variant has a **96-token total limit** (question +
options + state); the CPU+GPU variants keep the upstream 512/1024 context.

Perf on M3 Max: P50 4.88–4.98 ms ANE vs 6.94 ms MLX; 0.13–0.15 J/decision.

License: Apache-2.0, NOTICE attributes Convai Innovations. Fidelity is
scoped narrowly: "conversion-fidelity fixtures, not proof of general task
accuracy" (189/189 argmax agreement with upstream on validation questions).

Activity: 1.1k stars, 5 commits, 1 open issue.

### How they relate

Jev and Laya are two competing products in one new category — typed,
non-generative "System One" decision calls — one proprietary and hosted, one
open-weights and self-hostable, each benchmarking itself against the other.
laya-coreml is a third-party port of the open one to Apple hardware; it has
no relationship to TypeSafe or Jev at all.

## Evidence buckets

Every number below sorts into vendor (the maker's own claim), competitor
(one product's claim about the other), or ours (an independent measurement
on our machines, our rubric). None of the three buckets is an independent
third-party measurement — vendor numbers are self-reported, competitor
numbers come from a party selling against what it's measuring, and the ours
column is empty because we have not run anything.

| Metric      | Vendor                                                                                                                                                       | Competitor-reported                                                      | Ours |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------ | ---- |
| Latency     | Jev: 70–500 ms end-to-end (blog). Laya: 32.8–39.5 ms/question on T4 GPU, ~193–464 ms CPU (model card). laya-coreml: 4.88–6.94 ms on M3 Max (README)          | Jev: 236–276 ms p50, per Laya's README                                   | none |
| Accuracy    | Laya `laya-typed-decisions`: 0.766 argmax on Laya's own 400-case set; base checkpoints near chance at 0.362/0.352 (README, BENCHMARKS.md)                    | Jev: 0.727 argmax on the same set, 0.870 on Banking77, per Laya's README | none |
| Calibration | Jev: "calibrated probabilities" + `confidence` field, claimed (blog). Laya: ECE 0.466 → 0.081 only after per-question-type temperature refit (BENCHMARKS.md) | —                                                                        | none |
| Cost        | Jev: $0.042/MTok input, output free, 1,200 rpm / 250k tok/s dynamic (blog, models.md). Laya: free, open weights, compute cost only                           | Laya claims "7.8× faster" than Jev (Laya README)                         | none |

## Candidate seams

### 1. Routing judge for `{tier, engine, model, effort}`

- **Seam:** `adapters/core/protocols/DISPATCHER_PROTOCOL.md` → "For each
  task, decide tier AND model"; the `--effort` requirement in
  `adapters/core/dispatch.sh`; prior analysis in
  `docs/superpowers/specs/2026-08-10-dispatcher-routing-judge-design.md`
  (recommended no stronger judge until `crew rate` shows misrouting; option
  (d) was a classifier only on uncertain tasks).
- **Decision it would replace/add:** second-guess or pre-filter the
  dispatcher LLM's `{tier, engine, model, effort}` call with a typed `choice`.
- **Failure mode:** a wrong routing call sends a task too cheap (needs a
  later escalation, extra round-trip) or too expensive (wasted spend); the
  dispatcher already makes this call with full task-doc context in the same
  turn it produces the dispatch text, so a System One call would be an added
  round-trip, not a replacement for one.
- **Disposition:** no — zero misrouting evidence in the prior analysis or in
  `crew rate` to date.

### 2. Watchdog frame classification

- **Seam:** `adapters/core/crew.sh` `stall-watch` (D0–D4 detectors, the
  per-engine signature table, `_is_prompt`, `_is_quota_prompt`,
  `_is_quota_session_limit`), pinned by the frame fixtures in
  `tests/crew.bats`; design rule "a guessed signature is a false-positive
  generator" in
  `docs/superpowers/specs/2026-08-10-worker-liveness-detection-design.md` §5.
- **Decision:** classify a pane-capture frame into a D0–D4 state / prompt vs.
  quota vs. stall.
- **Failure mode:** `prompt:`/`quota:` are sticky and escalation-exempt;
  `quiet:`/`turn-stall:` escalate to `failed` — a misclassification either
  parks a live worker or kills one. Decisive: Jev "does not treat
  adversarial/injected content as hostile," and a pane capture is arbitrary
  text the worker (or a file it printed) chose.
- **Disposition:** no — deterministic, bats-tested today; the injection
  surface alone disqualifies a model here.

### 3a. Bus triage as label choice

- **Seam:** the dispatcher's "Read the bus" loop
  (`adapters/core/protocols/DISPATCHER_PROTOCOL.md`), which classifies
  `blocked` details into watchdog-vs-worker, permission, quota, question.
- **Decision:** a choice model would re-derive what the writer already
  stamped, via deterministic prefixes — e.g. `prompt:`, `quota:`,
  `permission:`, `acceptance:`, `body.source:"watchdog"`.
- **Failure mode:** none new — a misclassification here would just be worse
  than the string match it replaces, for latency and API cost with no
  informational gain.
- **Disposition:** no — the class is already carried by the writer; a model
  call would be strictly redundant.

### 3b. Retro synthesis / roster diagram labels as text

- **Seam:** `crew retro` tag rollup (`adapters/core/crew.sh`), roster
  diagram labels (`adapters/core/protocols/DISPATCHER_PROTOCOL.md` → "Roster
  diagram").
- **Decision:** summaries and labels are generation; only the `other` → tag
  re-classification of retro notes is choice-shaped.
- **Failure mode:** applying either model to the generation part is a
  category error, not a quality trade-off — both state plainly they do not
  generate text.
- **Disposition:** no — most of the seam is disqualified by definition; the
  sliver that fits (one re-classification step) is too small to justify a
  new dependency.

### 3c. One-rung model escalation after a failed attempt

- **Seam:** `adapters/core/protocols/DISPATCHER_PROTOCOL.md` → "One-rung
  escalation" and the `escalated_from` logic in `adapters/core/dispatch.sh`
  (~lines 974–1250).
- **Decision:** the _whether_ is read off the bus (`failed` at the same
  tier); the _target_ is a table lookup. Choice-shaped only in the judgment
  of whether the failure was model-bound rather than task-bound — a
  judgment that needs the failure detail text plus repo context.
- **Failure mode:** a wrong call either escalates a task-bound failure
  (wasted upgrade, same failure repeats at higher cost) or fails to escalate
  a model-bound one (repeats without benefit).
- **Disposition:** no — same shape as seam 1: a judgment that needs repo
  context an LLM already holds in the same turn.

### 4. Fifth engine (`--agent jev`) or a pi/OpenRouter model entry

- **Seam:** the engine gate in `adapters/core/dispatch.sh` /
  `docs/superpowers/specs/2026-09-17-configurable-engine-roster-design.md`;
  pi's OpenRouter model ids in
  `adapters/core/protocols/dispatch-orchestration.md`.
- **Decision:** would add Jev as a worker engine slot.
- **Failure mode:** not applicable — disqualified outright. TypeSafe's own
  docs say Jev "does not write code, hold a conversation, or call tools."
  It cannot fill an engine slot; this isn't a trade-off, it's a category
  mismatch.
- **Disposition:** no.

### 5. Reviewer-roster `when:` / security trigger

- **Seam:** the worker's evaluation of `when:` lines and the
  `security-reviewer` trigger in
  `adapters/core/protocols/WORKER_PROTOCOL.md` → "Code review gate"
  (`adapters/core/reviewers/resolve-roster.sh` handles globs deterministically; `when:` is judged by
  the worker LLM today).
- **Decision:** would judge whether a `when:` condition or the security
  trigger fires, from the diff under review.
- **Failure mode:** the input is the diff itself, which can carry injected
  instructions — the same jaggedness as seam 2 — and a false "no" silently
  drops the security reviewer with no visible signal.
- **Disposition:** no — same injection-surface disqualifier as seam 2, here
  gating a security review rather than worker liveness.

### 6. Nothing at all

- **Disposition:** this is the verdict — see below.

## Trade-offs

**Cost.** Jev: $0.042/MTok input, output free, rate limits 1,200 rpm / 250k
tok/s "adjusting dynamically" (vendor). Laya: free, open weights — the cost
is compute, not API spend.

**Latency.** Jev vendor claim 70–500 ms end-to-end; competitor-reported
236–276 ms p50. Laya: 32.8–39.5 ms/question on a T4 GPU, ~193–464 ms on CPU;
laya-coreml 4.88–6.94 ms on an M3 Max. None of these numbers were measured
by us, and none reflect our rubric or our machines under load.

**Quality.** Self-reported and competitor-reported only — no independent
measurement exists in any source (see Evidence buckets). Laya's
`laya-typed-decisions` checkpoint is the only local candidate that isn't
near chance on rubric-style decisions, and even it is unmeasured against
_our_ routing rubric.

**Portability.** The fleet is g6 (Linux x86_64, AMD Ryzen AI MAX+ 395 /
Strix Halo, Radeon 8060S iGPU — no CUDA, 32 threads) and mbp-m4-pro (Apple
silicon). Laya on CPU runs on both. laya-coreml runs on the Mac only, and
its fast ANE path caps total input at 96 tokens (question + options +
state) — tight for a task doc. CUDA numbers from Laya's own benchmarks apply
to neither machine.

**Maintenance.** The harness is bash + jq + bats under Nix (`flake.nix`,
`tests/crew.bats`); it carries zero Python/ML dependencies today. Adding
Laya means Python 3.10+, PyTorch 2.14+, transformers 5.x — a new dependency
class, not a version bump. Jev is a day-zero early-access hosted API with a
`jev-latest` alias that moves on release and rate limits that adjust
dynamically — operational risk on top of the integration itself.

**Data egress, as a delta against today.** Task text and diffs already
leave this machine for four engine vendors today; adding Jev is a fifth
vendor for the same data class, not a new class of exposure. Pane captures
are different: today they go nowhere off the machine (`stall-watch` samples
them locally), and they routinely contain whatever a worker just printed.
Routing pane captures to Jev (seam 2) would be a genuinely new egress
class — and exactly the surface its own "does not treat adversarial/injected
content as hostile" caveat applies to.
`adapters/core/protocols/WORKER_PROTOCOL.md` rule 7 governs secrets only; it
is not a general egress rule and is not being cited as one here.

## Verdict

**Don't**

- Every seam that needs _judgment_ (routing, escalation, review ingestion,
  bus triage) is already performed by an LLM that holds the repo context and
  must also produce text in the same turn; a System One call would be an
  extra round-trip that cannot replace that turn, only precede it. The prior
  routing-judge analysis
  (`docs/superpowers/specs/2026-08-10-dispatcher-routing-judge-design.md`)
  found zero evidence of misrouting to fix, and `crew rate` still shows
  none.
- Every seam that is _deterministic today_ (watchdog frames, bus-prefix
  triage) is deliberately so — bats-testable, sticky-state-safe
  (`tests/crew.bats`). A probabilistic classifier there trades a testable
  false-positive story for an untestable one, on detectors whose false
  positives park or kill workers.
- The only viable local option, Laya's `laya-typed-decisions` checkpoint, is
  self-reported at 0.766 on Laya's own corpus, unmeasured on our rubric,
  ships over-confident until temperature-refit, and any refit or fine-tune
  needs labelled routing data we don't have (22 runs, none labelled as
  misrouted).
- Running Laya adds Python + PyTorch (CPU, ~200–460 ms/decision) to a
  dependency-free bash/Nix harness; the fast port (laya-coreml) runs on only
  one of the two fleet machines and caps state at 96 tokens.
- Jev is hosted, launched today, early access, with a moving `jev-latest`
  alias and "dynamically adjusting" rate limits. It would receive task text
  (a fifth vendor for a data class four vendors already see) or pane
  captures (a genuinely new egress class, exactly where its injection
  caveat applies).
- The offline replay that could turn any of this into a measurement is
  deferred, not run now, because there is no misroute label to score it
  against: over the 22 runs to date, `rounds` is 0.0 on every row and `rev`
  is 0, so every replay disagreement would be unscorable.

Because the verdict is don't, no GitHub issue is opened for this task.

### What would reopen it

A `crew rate --report` row with `n ≥ 5` where `trivial` or `standard` shows
mean `rounds ≥ 1.0` **or** `rev ≥ 1`, while `deep` rows of similar size stay
below both. That's the fingerprint that would give an offline replay a
ground truth to score against.

### First experiment when it does

Run the offline replay: `choice` for tier over the historical task docs in
the crew bus, using Jev via `curl` and `laya-typed-decisions` on CPU as the
local comparator, scored against the dispatcher's tier and, on
disagreement, against the outcome labels from `crew rate`'s post-hoc GitHub
reconcile
(`docs/superpowers/specs/2026-08-10-crew-rate-reconcile-report-design.md`).
Success criterion: on tasks where the model's tier disagrees with the
dispatcher's, the model's tier predicts the outcome label (`rounds`/`rev`)
better on ≥ 2/3 of disagreements, over ≥ 10 disagreements.

Secondary triggers only change _which_ model the replay uses: Jev exiting
early access with a pinned model id, or a Laya checkpoint with a published
out-of-distribution rubric result.

## Sources

- https://typesafe.ai/blog/introducing-system-one-models-and-jev
- https://docs.typesafe.ai/models.md
- https://docs.typesafe.ai/api.md
- https://docs.typesafe.ai/introduction/quickstart.md
- https://docs.typesafe.ai/introduction/coding-agents.md
- https://docs.typesafe.ai/model-jaggedness/jev-1.13.md
- https://docs.typesafe.ai/legal.md
- https://docs.typesafe.ai/agent-skill.md
- https://github.com/NandhaKishorM/laya
- https://huggingface.co/convaiinnovations/laya
- https://github.com/mizorewww/laya-coreml
