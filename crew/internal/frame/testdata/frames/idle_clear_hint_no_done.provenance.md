# idle_clear_hint_no_done.txt provenance

- **Source:** a real idle Claude Code worker pane (hookyard worker "khaki"),
  finished and posted `done` about 35 hours before capture. The frame ends
  with the final assistant text, `new task? /clear to save 485.5k tokens`, an
  empty input box and status rows (`… · PR #267 · ← 1 agent`). No
  `· done HH:MM` row is on screen.
- **Version:** Claude Code `2.1.310`.
- **Captured:** 2026-11-02.
- **Pane:** the owner's live default tmux server, pane `%384`, read-only:
  `tmux capture-pane -p -t %384` (plain) and `tmux capture-pane -p -e -t %384`
  (`.color.txt` twin). No keys were sent.
- **Trim:** blank rows dropped, then the last 30 rows kept (the sampler's
  window); the lines themselves are verbatim.
