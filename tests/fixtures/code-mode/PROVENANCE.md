# Code Mode launch frames

Claude Code 2.1.325. Live captures taken 2026-11-15 in a private scratch tmux
server with `claude --settings <file> --permission-mode auto`, where the
settings file sets `CLAUDE_CODE_EXPERIMENTAL_CODE_MODE=1` in its environment
block (plus `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=0` except for the first
capture).

| File                               | Capture                                                                                                                                                                                                 |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent-teams-refusal.txt`          | live; agent teams left on, so Code Mode refuses                                                                                                                                                         |
| `optin-dialog.txt`                 | live; first-use opt-in dialog, owner has not accepted                                                                                                                                                   |
| `tools-refusal.txt`                | live; extra `--disallowedTools REPL`                                                                                                                                                                    |
| `live-launch-scrollback.txt`       | live; post-launch capture of the real flow (typed `bash '<launch script>'` that execs claude), Code Mode on, 100x40 pane. Claude Code clears screen and history at startup, so the typed line is absent |
| `live-startup-on.txt`              | live; Code Mode on startup frame, 120 columns                                                                                                                                                           |
| `live-relaunch-refused-100col.txt` | live; Claude Code 2.1.325, 100x40 pane, a relaunch into a reused shell pane without the AGENT_TEAMS override, so the refusal banner wraps mid-phrase (2026-11-15)                                       |

Other banner strings, verified in the binary: `Code Mode (experimental) isn't
available: <reason>. Starting with the ordinary tools.` and `Code Mode
(experimental) is off for this session, as you chose.`

The repo pre-commit hook strips trailing whitespace, so pane-padding spaces at line ends are gone from these captures; no other byte differs.
