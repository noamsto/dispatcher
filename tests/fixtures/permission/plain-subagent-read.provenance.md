# plain-subagent-read.txt provenance

- **Source:** a live, plain (non-classifier) Bash permission dialog raised by a
  `general-purpose` subagent running `cat /etc/os-release`.
- **Version:** `claude --version` = `2.1.310 (Claude Code)`.
- **Captured:** 2026-11-01.
- **Pane:** 200x50, on a private tmux server: `tmux -L cap509b`, with a private
  `TMUX_TMPDIR` and `TMUX` unset so the capture could never attach to the
  owner's live/default server.
- **Command:** `claude --permission-mode manual`, launched from
  `/tmp/cap509b/scratch`. The workspace-trust prompt was answered, then the main
  agent was asked to have a `general-purpose` subagent run
  `cat /etc/os-release`.
- **Option 2 forms:** its option 2 is the READ form
  `2. Yes, allow reading from /nix/store from this project`, unlike the WRITE
  form `2. Yes, and always allow access to … from this project` in
  `plain-subagent.txt`. Both are real 2.1.310 forms.
- **Capture:** `tmux -L cap509b capture-pane -p -t cap` (this fixture) and
  `tmux -L cap509b capture-pane -p -e -t cap` (escapes variant).
- **Dismissal:** the dialog was dismissed with `Esc`; no approving key (`1` or
  anything else) was ever sent. The owner confirms the "pressing 1 answers it"
  part separately.
- **Cleanup:** `tmux -L cap509b kill-server`.

The dialog lines are verbatim; no home path or username appears.
