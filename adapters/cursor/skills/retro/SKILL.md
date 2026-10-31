---
name: retro
description: Use when a batch leaves the roster drained, to write the retro synthesis
---

# retro

**Retro synthesis — claude: any batch that leaves the roster DRAINED, heartbeat as backstop; cursor/codex/pi: at a DRAINED roster, before the re-arm.** (**ACTIVE** = `working`+`blocked`; **TERMINAL / budget-freeing** = `pr_open`+`done`+`failed`; **DRAINED** = nothing active.) Read the crew's notes and roster, then write:

1. For each terminal worker, compare its outcome against your `{tier, engine, model, plan}` call; emit `misrouted` **only** on a contradiction, naming it (e.g. "trivial, but the diff touched an auth path — no review gate ran").
2. Emit `fanout_binder` when a concrete binder capped fan-out (429, drag, pile-up); `spec_too_thin` when a worker blocked on what the inlined spec should have answered.
3. Emit **one** `session_summary`: per worker its codename, `{tier, engine, model}`, outcome and note tags, plus any tag on **≥2 workers**. `crew retro --draft` prints it: check it, then post it.

- **Every note must quote a specific observable** (a tag, status detail, outcome or dispatch field), never an impression.
- **A clean drained roster writes nothing.**

```
crew msg "dispatcher:$CREW_ID" "retro:$CREW_ID" '{"seam":"dispatch","tag":"<tag>","detail":"<what>"}'
```

Like `metrics:`, `retro:` is a synthetic sink: `watch`/`inbox` filter on `to == dispatcher:<crew>`/`*`, so it wakes no dispatcher; it adds no event kind or write subcommand. `crew retro` / `crew retro --report` folds these notes back out.
