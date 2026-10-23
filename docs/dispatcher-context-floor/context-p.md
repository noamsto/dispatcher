# `/context` of the `-p` dispatcher analog (Claude Code 2.1.307, opus)

Command: `claude -p --model opus --effort high --no-session-persistence --append-system-prompt-file adapters/core/protocols/DISPATCHER_PROTOCOL.md /context`, cwd the main checkout, 2026-10-23. These are Claude Code's own estimates, used only to name what sits in each measured row; the report's numbers are usage deltas. The per-tool MCP table is folded into per-server sums.

### Estimated usage by category

| Category                | Tokens | Percentage |
| ----------------------- | ------ | ---------- |
| System prompt           | 65.6k  | 6.6%       |
| System tools            | 933    | 0.1%       |
| MCP tools               | 673    | 0.1%       |
| MCP server instructions | 3k     | 0.3%       |
| MCP tools (deferred)    | 580.5k | 58.0%      |
| System tools (deferred) | 15k    | 1.5%       |
| Custom agents           | 2.6k   | 0.3%       |
| Memory files            | 3.4k   | 0.3%       |
| Skills                  | 9.7k   | 1.0%       |
| Messages                | 2.7k   | 0.3%       |
| Free space              | 878.5k | 87.9%      |
| Autocompact buffer      | 33k    | 3.3%       |

### MCP tools by server (deferred: names only reach the model)

| Server                                  | Tools | Estimated schema tokens |
| --------------------------------------- | ----- | ----------------------- |
| claude_ai_ElevenLabs                    | 151   | 54822                   |
| claude_ai_Gmail                         | 30    | 7461                    |
| plugin_hm_firefox-devtools              | 29    | 4966                    |
| plugin_hm_playwright                    | 25    | 7709                    |
| claude_ai_Cloudflare_Developer_Platform | 23    | 5045                    |
| claude_ai_Sentry                        | 15    | 2315                    |
| claude_ai_Google_Drive                  | 11    | 5137                    |
| claude_ai_Google_Calendar               | 9     | 2573                    |
| claude_ai_Claude_Docs                   | 8     | 2107                    |
| plugin_hm_context7                      | 2     | 875                     |
| claude_ai_PostHog                       | 1     | 17                      |

### Custom Agents

| Agent Type                      | Source | Tokens |
| ------------------------------- | ------ | ------ |
| agent-docs-reviewer             | User   | 164    |
| agent-smith:editor              | Plugin | 85     |
| agent-smith:oracle              | Plugin | 91     |
| agent-smith:skeptic             | Plugin | 113    |
| charm-tui-reviewer              | User   | 132    |
| commit-writer                   | User   | 87     |
| database-reviewer               | User   | 109    |
| Explore                         | User   | 125    |
| expo-mobile-reviewer            | User   | 98     |
| general-reviewer                | User   | 59     |
| go-reviewer                     | User   | 69     |
| nix-reviewer                    | User   | 77     |
| nixos-expert                    | User   | 133    |
| pg-atlas-reviewer               | User   | 142    |
| postgres-reviewer               | User   | 172    |
| python-reviewer                 | User   | 46     |
| refactoring-agent:plan-refactor | Plugin | 84     |
| refactoring-agent:refactor      | Plugin | 82     |
| scout                           | User   | 105    |
| security-reviewer               | User   | 104    |
| shell-reviewer                  | User   | 58     |
| sqlite-reviewer                 | User   | 213    |
| terraform-reviewer              | User   | 69     |
| typescript-reviewer             | User   | 51     |
| yaml-reviewer                   | User   | 107    |

### Memory Files

| Type    | Path                                                                     | Tokens |
| ------- | ------------------------------------------------------------------------ | ------ |
| User    | /home/noams/.claude/CLAUDE.md                                            | 760    |
| User    | /home/noams/.claude/AGENTS.shared.md                                     | 1.3k   |
| AutoMem | /home/noams/.claude/projects/-home-noams-git-dispatcher/memory/MEMORY.md | 1.3k   |

### Skills

| Skill                                             | Source                            | Tokens |
| ------------------------------------------------- | --------------------------------- | ------ |
| bootstrap-nix-repo                                | User                              | ~60    |
| charm-tui                                         | User                              | ~130   |
| codebase-design                                   | User                              | ~100   |
| deslop                                            | User                              | ~100   |
| diagnosing-bugs                                   | User                              | ~60    |
| find-bugs                                         | User                              | ~60    |
| go-workspace-management                           | User                              | ~60    |
| grilling                                          | User                              | ~60    |
| mockery-at-scale                                  | User                              | ~50    |
| prototype                                         | User                              | ~60    |
| slack-voice                                       | User                              | ~130   |
| sqlc-workflow                                     | User                              | ~50    |
| staged-pre-commit-hooks                           | User                              | ~50    |
| temporal-split-workers                            | User                              | ~60    |
| work-tracking                                     | User                              | ~90    |
| worktrunk                                         | User                              | ~50    |
| writing-for-agents                                | User                              | ~40    |
| xcodebuildmcp-cli                                 | User                              | ~50    |
| commit                                            | User                              | ~20    |
| pr-review                                         | User                              | ~60    |
| push                                              | User                              | ~20    |
| commit-commands:clean_gone                        | Plugin (commit-commands)          | ~60    |
| commit-commands:commit-push-pr                    | Plugin (commit-commands)          | ~20    |
| commit-commands:commit                            | Plugin (commit-commands)          | < 20   |
| agent-smith:apply                                 | Plugin (agent-smith)              | ~60    |
| agent-smith:mine                                  | Plugin (agent-smith)              | ~90    |
| agent-smith:propose                               | Plugin (agent-smith)              | ~50    |
| agent-smith:run                                   | Plugin (agent-smith)              | ~70    |
| agent-smith:status                                | Plugin (agent-smith)              | ~40    |
| resolved:stale                                    | Plugin (resolved)                 | ~50    |
| tmux-og:issue-tracking                            | Plugin (tmux-og)                  | ~110   |
| tmux-og:tmux-interactive                          | Plugin (tmux-og)                  | ~70    |
| aeye:diagrams                                     | Plugin (aeye)                     | ~90    |
| aeye:image-gallery                                | Plugin (aeye)                     | ~80    |
| dispatcher:agent-browser                          | Plugin (dispatcher)               | ~100   |
| dispatcher:deslop                                 | Plugin (dispatcher)               | ~100   |
| dispatcher:spec-plan-critic                       | Plugin (dispatcher)               | ~80    |
| superpowers:brainstorming                         | Plugin (superpowers)              | ~80    |
| superpowers:dispatching-parallel-agents           | Plugin (superpowers)              | ~50    |
| superpowers:executing-plans                       | Plugin (superpowers)              | ~50    |
| superpowers:finishing-a-development-branch        | Plugin (superpowers)              | ~50    |
| superpowers:receiving-code-review                 | Plugin (superpowers)              | ~90    |
| superpowers:requesting-code-review                | Plugin (superpowers)              | ~50    |
| superpowers:subagent-driven-development           | Plugin (superpowers)              | ~40    |
| superpowers:systematic-debugging                  | Plugin (superpowers)              | ~40    |
| superpowers:test-driven-development               | Plugin (superpowers)              | ~40    |
| superpowers:using-git-worktrees                   | Plugin (superpowers)              | ~80    |
| superpowers:using-superpowers                     | Plugin (superpowers)              | ~60    |
| superpowers:verification-before-completion        | Plugin (superpowers)              | ~90    |
| superpowers:writing-plans                         | Plugin (superpowers)              | ~40    |
| superpowers:writing-skills                        | Plugin (superpowers)              | ~40    |
| context-efficient-tools:ast-grep                  | Plugin (context-efficient-tools)  | ~80    |
| context-efficient-tools:code-structure            | Plugin (context-efficient-tools)  | ~150   |
| context-efficient-tools:jq                        | Plugin (context-efficient-tools)  | ~40    |
| context-efficient-tools:ripgrep                   | Plugin (context-efficient-tools)  | ~80    |
| context-efficient-tools:yq                        | Plugin (context-efficient-tools)  | ~40    |
| refactoring-agent:how-to-plan-refactor            | Plugin (refactoring-agent)        | ~90    |
| refactoring-agent:how-to-refactor                 | Plugin (refactoring-agent)        | ~90    |
| frontend-design:frontend-design                   | Plugin (frontend-design)          | ~80    |
| resolved:check-stale-refs                         | Plugin (resolved)                 | ~140   |
| cowork-plugin-management:cowork-plugin-customizer | Plugin (cowork-plugin-management) | ~110   |
| cowork-plugin-management:create-cowork-plugin     | Plugin (cowork-plugin-management) | ~130   |
| dataviz                                           | Built-in                          | ~480   |
| update-config                                     | Built-in                          | ~240   |
| keybindings-help                                  | Built-in                          | ~80    |
| code-review                                       | Built-in                          | ~330   |
| simplify                                          | Built-in                          | ~60    |
| fewer-permission-prompts                          | Built-in                          | ~60    |
| loop                                              | Built-in                          | ~120   |
| schedule                                          | Built-in                          | ~130   |
| claude-api                                        | Built-in                          | ~360   |
| workflow-authoring                                | Built-in                          | ~80    |
| run                                               | Built-in                          | ~120   |
| plugin-authoring                                  | Built-in                          | ~90    |
| init                                              | Built-in                          | ~20    |
| security-review                                   | Built-in                          | ~30    |
| built-in-browser                                  | claude.ai sync                    | ~290   |
| chrome-browser                                    | claude.ai sync                    | ~260   |
| computer-use                                      | claude.ai sync                    | ~320   |
| anthropic-skills:deep-research                    | claude.ai sync                    | ~200   |
| docs                                              | claude.ai sync                    | ~340   |
| docx                                              | claude.ai sync                    | ~320   |
| google-workspace                                  | claude.ai sync                    | ~330   |
| import-memory                                     | claude.ai sync                    | ~60    |
| morning                                           | claude.ai sync                    | ~120   |
| pdf                                               | claude.ai sync                    | ~150   |
| pptx                                              | claude.ai sync                    | ~330   |
| skill-creator                                     | claude.ai sync                    | ~120   |
| xlsx                                              | claude.ai sync                    | ~320   |
