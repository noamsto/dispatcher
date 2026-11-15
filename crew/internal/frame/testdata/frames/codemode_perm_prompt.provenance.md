# codemode_perm_prompt.txt provenance

- **Source:** a permission prompt from an inner tool call made by code: title `Bash command`, `Run shell command`, the command in the U+254C box, `Ask rule … overrides auto mode for this command.`, `Do you want to proceed?`. No unattended countdown line appeared within about 60s.
- **Version:** Claude Code `2.1.325`, Code Mode on.
- **Captured:** 2026-11-15.
- **Pane:** a live capture in a private scratch tmux server, 120x45, started
  lead-shaped: `claude --settings <dispatch --code-mode overlay> --add-dir …
  --disallowedTools 'Edit(//…/**)' … --permission-mode auto`. The overlay also carried `permissions.ask` `Bash(echo cmp-ask-probe*)` to force the prompt. The file is
  the capture verbatim, trailing blank rows included.
