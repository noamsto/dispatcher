---
name: pane-injection
description: the human asks you to type into a pane
---

# pane-injection

- **Pane injection beyond the two constant lines below is a human last resort, never an automatic path.** To deliver a
  message or type into a stopped or re-dispatched session, nothing types into an
  engine pane but two constant lines: `crew inbox "$CREW_WORKER_ID"` (`crew nudge`
  above, or the watchdog, into a live claude/pi lead) and
  `continue from where you stopped; if a command's result is missing, re-run it`
  (the watchdog only, into an idle pi lead after a `truncated:`/`provider-error:` frame).
  Verified trust prompts (your job, pane captured before and after) and tool-permission
  dialogs (`permission-check --answer` only, never by hand) follow the `prompt:` bullet
  above, not this rule. A stopped worker is reached only by re-dispatch with the context baked in; any other reach (a coded reply a human must hand-deliver) is a human running `tmux send-keys`: give them the
  `crew where <branch>` line; capture the pane before typing and after submitting,
  verify the prompt was accepted, and never type over unsent input, a live turn, or a
  `quota:` wait (watchdog steps above).
