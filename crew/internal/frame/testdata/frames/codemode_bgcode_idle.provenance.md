# codemode_bgcode_idle.txt provenance

- **Source:** the idle prompt while Code Mode background code (`sleep 150; echo bgdone`) still runs: `✻ Cogitated for 6s · done 2:24 PM · 1 background task still running`, footer `· 1 background task`. Code Mode says `background task` where the shell-based tools say `shell`.
- **Version:** Claude Code `2.1.325`, Code Mode on.
- **Captured:** 2026-11-15.
- **Pane:** a live capture in a private scratch tmux server, 120x45, started
  lead-shaped: `claude --settings <dispatch --code-mode overlay> --add-dir …
  --disallowedTools 'Edit(//…/**)' … --permission-mode auto`. The file is
  the capture verbatim, trailing blank rows included.
