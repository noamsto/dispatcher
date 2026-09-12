# Review evidence: optional tools

Online sweep: 2026-09-12. These are candidates, not installed integrations or
measured wins. The policy lives in
[EVIDENCE_REVIEW.md](../adapters/core/protocols/EVIDENCE_REVIEW.md); tools supply
evidence inside that workflow.

| Tool                                                                                                    | Useful experiment                                                                                                                   | Tradeoff / adoption decision                                                                                                                                                                       |
| ------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [Go fuzzing](https://go.dev/doc/security/fuzz/)                                                         | Exercise parsing and normalization invariants; saved failing inputs run as regression seeds under ordinary `go test`.               | Start with bounded, package-scoped runs. Requires a meaningful oracle and deterministic target; input volume does not prove the contract. No new Go dependency.                                    |
| [Gremlins](https://github.com/go-gremlins/gremlins)                                                     | Check whether tests detect changed production predicates in a small Go module.                                                      | Pilot only: mutation runs add cost and surviving mutants need interpretation. A single isolated old-behavior check is the cheaper default for one bug fix.                                         |
| [StrykerJS incremental mode](https://stryker-mutator.io/docs/stryker-js/incremental/)                   | Mutation-test selected TypeScript/JavaScript files or lines and reuse previous results.                                             | Candidate for a testable shared package, not a required monorepo gate. Incremental reuse has blind spots for external changes; use `--force` when dependencies/environment affect validity.        |
| [Semgrep rule tests](https://docs.semgrep.dev/writing-rules/testing-rules)                              | Encode a recurring, mechanically recognizable mistake with positive `ruleid` and negative `ok` fixtures, then run `semgrep --test`. | Add only after a pattern recurs and a rule can distinguish valid usage. This is not a substitute for runtime contract tests or consumer tracing.                                                   |
| [GitHub GraphQL review state](https://docs.github.com/en/graphql/reference/pulls) through existing `gh` | Read thread IDs, `isResolved`, `isOutdated`, and head SHA instead of inferring resolution from comment age.                         | Use the existing GitHub connection. Paginate threads and nested comments separately; fetch issue comments and submitted reviews too. API errors are incomplete evidence, not an empty finding set. |

## Ownership

[Superpowers](https://github.com/obra/superpowers) already supplies planning,
test-driven development, debugging, and review techniques. Dispatcher owns the
policy about when they run, which context/model runs them, proof of completion,
and bounded recovery. This change reuses those techniques without layering a
second lifecycle or depending on a particular Superpowers installation.

## Evaluate before expanding

Compare a small batch of similarly risky PRs before and after the protocol:
time to first reviewable PR, review/fix rounds, confirmed invariant recurrences,
missed consumers, and stronger-review usage. Separate mechanical changes from
cross-component work and deduplicate bot findings. Raw comment counts and CI
green alone are not quality measures.

Adopt a tool only when it catches a missed invariant at acceptable local runtime
and false-positive cost. No new review SaaS, whole-repo mutation gate, or blanket
premium-model routing is required by this change.
