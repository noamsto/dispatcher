# Review Task

`WORKER_TASK.md` stamps `kind: review`. You review a GitHub PR that already exists, post one review on it, and stop. This contract replaces every `WORKER_PROTOCOL.md` step that presupposes a code change — spec, plan, execute, the fast deterministic gate, code `/deslop`, push, PR. Everything else in that protocol still governs you: the startup bus drain, checkpoint-peek at each seam below, the block→await discipline, and `crew status`.

Your tier still means something: it sizes the **reviewer fan-out**, not the pipeline depth.

Read `pr:` from `WORKER_TASK.md` — that number is the PR under review.

## Never change the tree

No edits, no commits, no push, no PR, no merge, no branch or tag. Your only writes are review comments through `gh`. A findings scratch file at the worktree root is fine; it must never reach the index.

## The worktree is the PR head

`dispatch --pr N` attached this worktree to the PR's head branch, so the code in front of you **is** the code under review. Never reconstruct it: no `gh pr diff | patch`, no `git fetch origin pull/N/head`, no checkout of the default branch, and never assume you are on `main`. Scope the diff against the PR's own base:

```bash
base=$(gh pr view "$pr" --json baseRefName --jq .baseRefName)
git fetch -q origin "$base"
git diff --name-only "origin/$base...HEAD"
```

If that diff comes back empty, the tree is not what dispatch promised. Post `blocked` and ask the dispatcher — do not repair it by fetching the PR yourself.

## Dispatch reviewers directly

Map the changed files to the reviewers that match them (the language reviewers, config/SQL/UI reviewers, plus `security-reviewer` when the diff touches an auth, crypto, input-parsing, SQL, or network path — conservative trigger). Then dispatch **those reviewers**, one agent each, in a single parallel batch.

**Never route the review through a meta-agent** — a coordinator agent asked to "review this PR" falls back to weak inline review and misses real findings. You are the coordinator.

| tier | fan-out |
| --- | --- |
| `trivial` | no fan-out — review inline yourself, applying the matched reviewer's rubric |
| `standard` | every matched reviewer, one agent each, one batch |
| `deep` | the same batch plus a diverse-engine pass (work profile, claude only — the read-only codex MCP; a should, not a blocker) |

Changed files no reviewer covers are **reviewer gaps**. Carry them into the tally; never silently drop a file.

## Verify adversarially

Every surviving finding gets **one refuter agent**, all dispatched in one parallel batch, each scoped to the finding's file and prompted to prove the finding **wrong** — the bug cannot occur, the symbol it names does not exist or behaves differently, the misread is not real, or the fix is already in place. **Default to `refuted` when the claim cannot be confirmed from the code in front of it.** Drop every refuted finding.

Nothing gates this review before it lands, so the refuter pass is the only thing standing between a hallucinated finding and a human's PR.

## Deslop, then post exactly one review

Deslop the **comment prose you wrote** (not the code — the code is not yours): drop hedging, preamble, praise padding, restatement of what the code does, and repetition between what-is-wrong and the fix. A comment that says nothing concrete after deslopping is dropped along with its finding.

Then post **one** review event — not N comment spams:

```bash
gh api "repos/{owner}/{repo}/pulls/$pr/reviews" \
  -f event=COMMENT -f body='<one-line tally + any non-line-local finding>' \
  -f 'comments[][path]=<path>' -F 'comments[][line]=<line>' -f 'comments[][body]=<what → fix>'
```

`event=COMMENT` always. **Never `REQUEST_CHANGES`** — an unattended worker's review is advisory and must not block a human's merge.

## Approve only on zero survivors

Approve (`event=APPROVE`) only when **zero** findings survive verification and deslopping. One survivor of any severity means comment-only.

**Never approve a draft.** Read `gh pr view "$pr" --json isDraft --jq .isDraft` before approving, not after.

## Report the tally, then stop

Post the tally as one message to the dispatcher, then terminate at `done` — a review worker never reaches `pr_open`, because it opens nothing:

```bash
crew msg "worker:$(git branch --show-current)" "dispatcher:$CREW_ID" \
  '{"pr":<N>,"lane":"<inline|fan-out>","reviewers":["…"],"findings":{"blocker":0,"should-fix":0,"clarity":0},"approved":<true|false>,"review_url":"<url>","gaps":["…"]}'
crew status "worker:$(git branch --show-current)" done "reviewed PR <N> — <review_url>" "<pr_url>"
```

The url slot carries the **PR** url, not the review url: `crew reap` feeds it to `gh pr view` to decide whether this worktree can be reclaimed, and a `#pullrequestreview-…` fragment is not a PR reference. The review url rides in the detail and in the tally.

A review worker emits the tally **instead of** the outcome-metrics record in `WORKER_PROTOCOL.md` ("When done") — that record rates an implement pipeline this worker never ran.
