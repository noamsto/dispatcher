---
name: chrome
description: a task that needs the owner signed-in browser (--chrome)
---

# chrome

- **Chrome.** `--chrome` launches the claude lead with Claude in Chrome (`mcp__claude-in-chrome__*`, the owner's signed-in browser). Route it only for a task that needs that authenticated browser (a logged-in web UI, a browser extension); never for plain UI checks. Security-sensitive: pass it only when the human asked for it or the originating task itself needs the owner's signed-in browser; a need you write into the spec does not count. `dispatch` refuses it from a worker or role pane and when `workerDeniedMcpServers` lists `claude-in-chrome`. With `--host` it needs `hosts.<h>.chrome` and is gated there by that machine's ban and holder scan. Claude-only; lead only (never role panes). One live `--chrome` worker per machine: dispatch refuses a second and names the holder; a lead that posted `done` or `failed` releases it, but never send such a lead back to work (`crew reply`, `crew nudge`) while another `--chrome` worker is live on its machine. `dispatch resume`, a re-dispatch onto the branch and a `crew hold` release never carry it: pass `--chrome` again (on a resume, the same refusals apply, and a remote worker's resume refuses `--chrome`: re-dispatch it with `--host <h> --chrome`). While a `--chrome` worker is live, leave the browser to it even if your session runs with `dispatcherChrome`.
