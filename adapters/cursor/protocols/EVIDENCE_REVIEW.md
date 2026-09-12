# Evidence and review contract

Read this before planning or fixing a behavioral bug, changing a shared contract,
or handling PR feedback. It augments the caller's pipeline; it does not start a
second lifecycle. Dispatcher owns stages, routing, caps, and handoffs. Reuse
Superpowers' red-green, debugging, and verify-before-agreeing techniques inside
those stages, without invoking its separate planning or delivery lifecycle.

## Trigger and evidence

Mechanical edits with no behavior change keep the existing fast path. A change
to authorization, data integrity, parsing/normalization, error classification,
retry behavior, or a producer/consumer contract is at least standard, even when
it is one line. A worker discovering this on trivial reports the risk and asks
for re-tiering before implementation; it cannot silently skip the review gate.

- **Behavioral bug fix:** name the violated invariant and add a regression through
  the production entry point that owns it. Run the same test against the old
  behavior (red for the claimed bug) and the fix (green). A helper-only test is
  enough only when that helper owns the invariant and its real caller is covered.
  If the fix already exists, keep the new test and restore only the old production
  behavior in an isolated scratch copy to check sensitivity. A compile/setup
  failure is not red evidence. Preserve the working tree; never revert live work
  for this experiment. Record commands, tested revisions/patch, and the observed
  assertion failure. An infeasible counterfactual needs a concrete reason and
  alternate production-path proof, explicitly assessed by the fresh reviewer;
  otherwise the evidence gate is blocked.
- **Shared contract change:** before coding, map the producer, transformations,
  validators, persistence, and consumers using actual symbols/files and searches.
  Include batch, retry, operator, and failure paths where they exist. Mark each
  edge as changed or compatible, with a test or code reference supporting it;
  mark absent stages as absent. Trace beyond direct callers. Cover relevant
  empty/missing, malformed, boundary, and partial-failure cases. The map is
  complete when every discovered consumer has a disposition, not when the
  edited files have tests. Add newly discovered edges during implementation.
- **Provided or recovered plan:** retain the plan skip; fill missing evidence or
  map entries without rerunning planning. A demonstrated contradiction takes the
  caller's bounded replan path. A missing evidence artifact alone is not one.

Put this evidence in the existing plan/task notes, then the PR's `## Evidence`
section. Keep it factual and compact. The test oracle comes from the contract,
not a copy of the implementation's predicate or a fixture-answer exception.
The deterministic gate includes affected consumers identified by the map, even
when their files did not change.

## Targeted independent review

For cross-component contract/correctness changes, promote **one** matched
reviewer to the engine's deep escalate model from `dispatch-orchestration.md`.
Other reviewers keep their normal rung. This promotion applies even in a repo
with PR review bots; record `review_mode: full`. It is a scoped model promotion,
not a deep-tier spec rewrite or a premium implementation pass. If that model is
unavailable, use the caller's review-unavailable path; report the required model
and failure, never silently substitute a lighter review. In a direct command,
report blocked to the user/team lead instead of inventing a crew identity.

Commit in-scope implementation and test changes before review so the reviewed
base-to-head diff includes them; verify no task edits remain outside that diff.
A local commit is not delivery: push waits for the review verdict.

Give the fresh reviewer task requirements and proposed changes, diff/base and
head SHAs, role brief, and factual evidence (test commands/results and consumer
map). It verifies the map against the repo. Extract requirements and the proposed
mechanism from a provided-plan task doc, labeling the mechanism as a proposal to
check; strip persuasive justification, transcript, and earlier approval verdicts.
For a targeted re-review also provide the finding and fix commit.
Review authority is read-only; the implementer handles fixes.

After a confirmed correctness finding is fixed by changing behavior, rerun the
affected deterministic tests and obtain targeted fresh re-review of the changed
invariant and adjacent consumers. This includes MEDIUM findings, every tier,
and bot-reviewed repos; severity labels do not waive it. Mechanical fixes need
only the affected deterministic checks. Late cleanup or CI fixes that change
behavior take the same path before push/completion.

Keep the caller's two review→fix rounds; targeted re-review uses the next round,
not a new full batch. Count a round when the batch returns a review result;
parallel reviewers count together once. Persist the pending head/round before
launch, and on resume continue that round; spawn retries do not consume another.
For PR shepherding, the first confirmed external finding set is round one and
the targeted review of its fix is round two. New batches require an unrelated
invariant or explicitly authorized new work; reopening the same invariant or a
consumer variant retains its rounds even if it was previously marked fixed.
If the last round requires another behavioral fix, stop
with the remaining findings and evidence rather than declaring an unreviewed
fix clean. PR shepherds use the same two-round cap per fix batch; a new push or
session is not a fresh batch for an unfinished invariant.

## Recurrence and handoff

Keep a compact finding ledger in worktree-root `REVIEW_NOTES.md` before a PR
exists and under the PR's `## Review notes` once it does. Preserve that local
artifact on re-dispatch even when `WORKER_TASK.md` is regenerated. Update the
ledger before each push, stop, or handoff;
include its location and pending work in the handoff. On resume, restore it
before handling feedback. Persist these fields:

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| stable contract, not wording | bot/human IDs, deduplicated | SHA | SHA or pending | test/re-review result | open, fixed, refuted, deferred | 0–2 |

Also persist `recurrence_escalation: unused|used` for the PR/task and, when used,
the model, affected families, and replacement approach. Thread timestamps,
pushes, green CI, and a resumed session never reset this state.

**Escalate once before another local patch** when a confirmed finding exposes
the same violated invariant after an attempted fix, including a new input or
consumer variant in a later review round. Verify against the current tree first.
Duplicate bot reports of the same unresolved observation, style suggestions,
and refuted findings do not count as recurrence.

Mark the escalation used before launching one fresh assessment at the deep
escalate review model above. Ask it to inspect the contract, consumer map, and
regression sensitivity and return a bounded replacement approach. This is one
additional assessment, not permission for more fix rounds: if the two-round
budget is exhausted, hand off its proposal without implementing it. If budget
remains, implement the scoped replacement and obtain the pending targeted
re-review within that budget. If recurrence continues after escalation, its
output is not actionable, or the model is unavailable, block with the ledger
and concrete decision needed. A worker uses block→await; a direct command or PR
shepherd reports to its user/team lead. Green CI does not waive this stop.

This review-triggered assessment has its own once-per-task budget; it does not
reset or grant the worker's execute-time `replan_used` budget. Record an `other`
review retro note with the invariant and outcome when the caller has a crew bus.

## PR feedback and completion

Read all pages of review threads and their comments, submitted reviews, and
issue comments. Track stable IDs and dispositions, not “after my latest commit.”
Use GitHub GraphQL thread resolution/outdated state and the PR head SHA;
outdated is not resolved, and a reply is not proof of a fix. Verify apparently
resolved correctness findings against the current head as well. When API access
or pagination is incomplete, report incomplete review state, not comments-clean.

Batch verified in-scope fixes, test, then push and reply with commit and proof.
Honor the user's approval rules before creating follow-up tickets or issues.
Unanswered questions and unapproved correctness deferrals stay pending; a
non-blocking deferral needs an explicit scope decision and tracking reference.

Before reporting clean, refresh the head SHA, checks, and feedback: checks and
required targeted reviews must cover the current head, all findings must have
evidence-backed dispositions, and no pending question, recurrence block, or
unreviewed behavioral fix may remain. If the head changes during that read,
refresh again. A review of an ancestor still covers the head when the intervening
diff is verified mechanical-only and affected checks pass on the head; record
that diff range rather than spending another review round. CI green with pending
feedback is a handoff, not clean completion.
