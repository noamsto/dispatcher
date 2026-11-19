# permission_bash_guard_block.txt provenance

- **Source:** synthetic. Issue #1669 showed an auto-blocked dangerous-`rm`
  guard dialog: the command, a `╌` rule and the guard line, behind a `│`
  gutter with the rule between command and guard line, then the
  `block this request in …` countdown, question, `1. Yes` / `2. No` options,
  footer and the named bottom rule `───── moss ─`. That snippet was
  hand-abbreviated (`<command>`, `<worktree>`), so this fixture deliberately
  keeps the #1424 layout of `permission_bash_guard.txt` (`╌` rules around the
  command and guard block, no gutter) for the top rule, header, command,
  guard line and rules. **Not a live capture**.
- Replace with a real capture when one is available.
