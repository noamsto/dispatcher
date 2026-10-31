---
name: pr-watch
description: a pr-watch event
---

# pr-watch

A `pr-watch:<N>` msg is the other watchdog: `crew pr-watch <N>` parks detached until the PR's head SHA, reviews, review threads, checks,
open/merged state, `base_sha` or `mergeable` moves (a move to
or from `UNKNOWN` does not count), then posts the change event (`changed[]`
names what moved). Stacked layers: arm, re-arm and rebase per
`dispatcher:stacked`. Treat it as a question `msg`; a PR needing another pass gets a
`trivial` review worker (`--pr N`), not your own work.
