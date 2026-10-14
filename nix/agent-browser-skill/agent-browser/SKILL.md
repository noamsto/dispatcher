---
name: agent-browser
description: "Headless, isolated browser checks with the agent-browser CLI: verify that a page or flow renders and behaves, take snapshots and screenshots, drive forms. In an interactive session the Playwright and Firefox DevTools MCPs stay the right tool; use this for scripted, throwaway checks."
allowed-tools: Bash(agent-browser:*)
---

# agent-browser

This file is a discovery stub, not the usage guide. Load the guide from the installed CLI, so it always matches the installed version:

```bash
agent-browser skills get core          # workflows, common patterns, troubleshooting
agent-browser skills get core --full   # adds the full command reference
agent-browser skills                   # list every other topic; read one with `skills get <name>`
```

Read `core` before running any other `agent-browser` command.

In a dispatched worker, `AGENT_BROWSER_SESSION` and `AGENT_BROWSER_CONFIG` are already exported. Never override them or pass `--session`.
