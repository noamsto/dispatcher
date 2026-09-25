---
name: deslop
description: Use when reviewing a branch before committing or creating a PR. Checks the changes you're about to push (unpushed commits, or the whole branch when nothing is pushed yet) and removes AI-generated slop (bloated comments, defensive blocks, casts, style inconsistencies, hallucinated APIs).
---

# Remove AI code slop

Check the changes you're about to push and remove AI-generated slop introduced in them. Scope to the unpushed commits, not the whole branch — so adding a small fix to an already-pushed PR reviews only that fix, not everyone's prior work on the branch.

## Process

1. Determine the review base — the changes you're about to push, not the whole branch. If you were handed a base (a dispatcher worker on a stacked layer passes its Base ref `base`), use it as `<BASE>` and skip to step 2. Otherwise:
   ```sh
   git rev-parse --verify --quiet '@{push}' \
       || git rev-parse --verify --quiet '@{upstream}' \
       || git merge-base "$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)" HEAD
   ```
   On an already-pushed branch (e.g. adding a fix to an open PR) this scopes to just your new commits. On a fresh branch with no upstream it falls back to the merge-base with the default branch (`origin/HEAD`, else `origin/main`), i.e. the whole branch. For a stacked branch on a non-default-branch parent, swap the default branch for the real parent (check `git log --oneline -20` if unsure).

   **Substitute the SHA it prints into every later command.** Resist assigning it to a shell variable: variables do not survive between tool calls, and `git diff ""..HEAD` exits 0 with an empty diff, so the passes below would silently find nothing.
2. Get the diff: `git diff <BASE>..HEAD`
3. **Run the comment pass** (below) over every added comment. Do this first and finish it before anything else — it is the slop teammates report most.
4. Review each changed file for the other patterns below.
5. Remove identified slop while preserving legitimate changes.
6. **Self-audit pass:** re-read your edits and ask "what still looks obviously AI-generated?" Fix anything that stands out.
7. Report a 1–3 sentence summary of what was changed.

## The comment pass

A comment survives only by passing **both** gates. Apply them per comment.

**Gate 1 — audience.** Who is it written to?

- **Reader-facing** — someone who opens this file cold in six months, knowing nothing about your change. Passes.
- **Reviewer-facing** — the person reading *this diff today*. It argues the change is right. **Move it to the commit body and delete it from the code.**

Reviewer-facing rationale hides from an ordinary review because it is genuinely true and hard-won — it just belongs in the PR, not the file. Deleting it relocates the rationale to where its audience actually is.

**Gate 2 — payload.** Does it tell the reader something the code doesn't already say? A comment can be perfectly reader-facing and still carry nothing: `// GetUser returns the user by ID` above `GetUser(id) (*User, error)`. Restatement fails here regardless of language — godoc, JSDoc, docstrings, and inline narration of the next line. Delete it outright; there is nowhere to relocate it to.

What passes gate 2, at any length:

- A derivation — arithmetic, a budget, a table of measured values behind a magic number.
- An external constraint — wire format, upstream bug, hardware quirk, spec clause.
- Why the obvious approach fails here.

A 24-line comment deriving a timeout from the retry budgets that feed it is a good comment: nobody can recompute it from the file. A 6-line comment explaining why a boolean is `false` is not.

### Finding them

Size is a trigger for review, never a verdict. List every added comment block of 4+ lines, substituting the base SHA from step 1:

```sh
git diff <BASE>..HEAD | awk '
  /^\+\+\+ b\// { file = substr($0, 7); next }
  /^@@/ { split($0, a, "+"); split(a[2], b, /[, ]/); ln = b[1] + 0; next }
  /^-/ { next }
  { if ($0 ~ /^\+[[:space:]]*(\/\/|#|\/\*|\*[[:space:]]|\*\/$)/) { if (n == 0) start = ln; n++ }
    else { if (n >= 4) printf "%s:%d  %d lines\n", file, start, n; n = 0 }
    ln++ }
  END { if (n >= 4) printf "%s:%d  %d lines\n", file, start, n }'
```

Open each hit and apply both gates. Then read the diff for short comments too — a one-line restatement fails gate 2 just as hard, and the detector never sees it.

### Register tells for reviewer-facing

- **Counterfactual defense** — "would treat those as…", "short-circuiting here would answer the wrong question", "is not sufficient on its own". It rebuts an alternative implementation nobody reading the file will propose. Only your reviewer would.
- **Branch walkthrough** — enumerating what each value of a flag or each arm of a conditional does, when the call sites already show it.
- **Not-X-but-Y about the change** — "turns on whether the caller's decision WAS a review — not on whether the value changed". Contrasting against what the code used to do, or what you almost wrote.
- **Ticket rationale** — narrating the bug this fixed or the constraint that motivated the PR.
- **Caller census** — listing today's callers ("both real callers are org-scoped: …"). It rots on the next call site and the reader can grep.

### Worked example

Cut — reviewer-facing, restates the call sites and defends the choice:

```go
// preserveCorrectness turns on whether the caller's decision WAS a correctness
// review — not on whether the value changed:
//   - false — the "Keep" path and contention resolve (winner pick or
//     reject-all). Both clear the needs-check mark by dropping the verdict.
//   - true — stamp approval confidence while keeping a pending review.
// Acknowledging on confidence alone would clear a review that was never
// finished, so short-circuiting there would answer the wrong question.
preserveCorrectness: false,
```

Keep — the one fact the code doesn't show:

```go
preserveCorrectness: false, // "Keep" drops the verdict, clearing needs-check
```

### Duplication

The same rationale pasted at both the declaration and the call site is one comment too many. Keep it at the declaration.

## Other focus areas

- Defensive checks or try/catch blocks abnormal for trusted code paths in this codebase.
- Type-system workarounds used to bypass real type issues rather than fix them.
- Deeply nested code that should be flattened with early returns.
- Hallucinated APIs — verify any unfamiliar import, method, or config option actually exists in the dependency.
- Convention-blind code — compare naming, error handling, and logging against the surrounding file before keeping new patterns.
- Over-engineering — extra abstraction, options, or indirection beyond what the task requires.
- Ephemeral scratch files — leftover `PLAN.md`, `SCRATCH.md`, `NOTES.md`, `TODO.md`, etc. added during the session but not part of the deliverable. Delete unless the task explicitly asked for them.

## Language-Specific Patterns

### Go
- Redundant `if err != nil { return err }` that loses context — wrap with `fmt.Errorf("...: %w", err)` only when adding information; otherwise return as-is.
- `fmt.Errorf` re-wrapping that adds nothing new to the message.
- Leftover `context.TODO()` where a real context is in scope.
- `_ = result` discards left over from scaffolding.
- Unnecessary interfaces created for a single implementation.

### TypeScript
- `as any`, `as unknown as X`, or `@ts-ignore` used to silence the type checker instead of fixing the type.
- Optional chaining (`?.`) chained after a non-null guard already proved the value exists.
- `useEffect` doing work that derived state or an event handler would handle correctly.
- Leftover `console.log` / `console.debug`.
- `try/catch` that re-throws unchanged or swallows the error silently.

### Python
- Imports declared inline inside functions — move to the top of the file with the rest.
- `except Exception` blocks that swallow or re-raise unchanged.
- Type hints written as comments when real annotations are available.

## Guardrails

- Keep behavior unchanged unless fixing a clear bug.
- Prefer minimal, focused edits over broad rewrites.
- Don't reformat unrelated code.
