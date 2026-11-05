# permission_bash_guard.txt provenance

- **Source:** the frame shape in issue #1424 (Claude Code 2.1.310, dangerous-`rm`
  guard dialog). **Not a live capture**: a capture would have meant answering a
  live dialog. The guard line, countdown line, `Do you want to proceed?`,
  `1. Yes` / `2. No` options and footer are the issue's verbatim text; the
  surrounding rules and title are built from the plain
  `permission_bash.txt` layout minus its `Tip:` row.
- `permission_bash_guard.color.txt` is the same frame with SGR colour on the
  rules, guard and countdown lines, as `capture-pane -e` would emit.
- Replace both with a real capture when one is available.
