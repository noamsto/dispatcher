# Code Mode launch frames

Claude Code 2.1.325. Live captures taken 2026-11-15 in a private scratch tmux
server with `claude --settings <file> --permission-mode auto`, where the
settings file sets `CLAUDE_CODE_EXPERIMENTAL_CODE_MODE=1` in its environment
block (plus `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=0` except for the first
capture).

| File                      | Capture                                                                                                                                                             |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent-teams-refusal.txt` | live; agent teams left on, so Code Mode refuses                                                                                                                     |
| `optin-dialog.txt`        | live; first-use opt-in dialog, owner has not accepted                                                                                                               |
| `tools-refusal.txt`       | live; extra `--disallowedTools REPL`                                                                                                                                |
| `on-banner.txt`           | synthetic, not a capture: the binary string `Code Mode (experimental) is on · this session can be resumed only with Code Mode on`, until a live capture replaces it |

Other banner strings, verified in the binary: `Code Mode (experimental) isn't
available: <reason>. Starting with the ordinary tools.` and `Code Mode
(experimental) is off for this session, as you chose.`
