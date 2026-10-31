# Triage-reader spike: inline `--agents` MCP auth

Closes #1279. Informs rows 1 and 5 of the
[Sequenced plan](../dispatcher-context-floor.md#sequenced-plan).

## Question

Can a subagent defined inline through `claude --agents` with inline `mcpServers`
authenticate to the servers the claude.ai connectors use (Sentry, PostHog),
non-interactively? If not, row 5 (`triage-reader`) cannot replace the connectors
that row 1 removes.

## Method

Claude Code 2.1.310, run from the worktree, `claude -p --strict-mcp-config`.
The agent definition is passed with `--agents`; the prompt asks the dispatcher
session to delegate to it. No token, cookie or login was supplied or printed.

Docs read first (official only):

- [Subagents](https://code.claude.com/docs/en/sub-agents): in JSON, `mcpServers`
  is an **array** of `{name: config}` entries (an object is rejected, see run 0).
  `--strict-mcp-config` does not filter inline `--agents` servers. The page
  documents no auth behaviour for them.
- [MCP](https://code.claude.com/docs/en/mcp): `claude -p` has no `/mcp` panel and
  cannot run OAuth. Sign in first with `/mcp` or `claude mcp login <name>`;
  tokens are stored and refreshed.

## Evidence

Agent definition (jq-built; `$n`/`$u` = server name/URL):

```json
{
  "reader": {
    "description": "read-only spike agent",
    "prompt": "List up to 5 tool names from your MCP server, or quote the exact auth error text.",
    "disallowedTools": ["Bash", "Write", "Edit"],
    "mcpServers": [{ "<n>": { "type": "http", "url": "<u>" } }]
  }
}
```

Command (exit codes were not captured: the shell pipe to `head` hid them):

```
claude -p --strict-mcp-config --agents "$A" "Use the reader subagent to list one tool from its <n> MCP server. Report the subagent answer verbatim."
```

| Run | Server                                  | Variation                                       | Outcome                                                                                                                                                                        |
| --- | --------------------------------------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 0   | sentry                                  | `mcpServers` as an object                       | `Error: Invalid --agents configuration: reader.mcpServers: Invalid input: expected array, received object`                                                                     |
| 1   | sentry, posthog                         | `tools: ["Read"]`                               | Agent saw only `Read`; no MCP tools and no auth message. Most likely the `tools` allowlist hid the MCP tools (not isolated), so the run says nothing about auth.               |
| 2   | sentry (`https://mcp.sentry.dev/mcp`)   | no `tools` allowlist, `disallowedTools` instead | Server configured but no tools. Harness message: servers "require authentication before their tools can be used". ToolSearch for `sentry`: "No matching deferred tools found". |
| 2   | posthog (`https://mcp.posthog.com/mcp`) | same                                            | Same outcome.                                                                                                                                                                  |

Neither run reused the claude.ai connector's OAuth: the connectors were not
loaded by the strict-config session, and an inline http server with the same URL
is a separate server entry that needs its own sign-in.

I did not run `claude mcp login`: it needs a browser, and the plan says to stop
there and record it.

## Verdict

| Server  | Verdict                                                                                                                 |
| ------- | ----------------------------------------------------------------------------------------------------------------------- |
| Sentry  | Does not work non-interactively as-is; unauthenticated inline server, no tools. Untested: works after a one-time login. |
| PostHog | Same as Sentry.                                                                                                         |

Overall: **does not work out of the box.** The inline server does not borrow the
claude.ai connector's session. Whether a one-time `claude mcp login <name>`
makes the inline definition work in `-p` is untested; the docs suggest stored
tokens are shared by server name/URL, but that is unverified here.

## What it means

- **Row 1** (drop connectors from the dispatcher): the Sentry, PostHog and
  Cloudflare reads are lost unless 1b (opt-in passthrough) lands. Row 5 does
  not make the removal free yet.
- **Row 5**: blocked on a follow-up spike that does the interactive
  `claude mcp login sentry` (and posthog) once, then repeats run 2 with the
  same definition. Implementation notes for it: `mcpServers` is an array in
  `--agents` JSON, and the agent must not use a `tools` allowlist, which appeared to hide
  MCP tools (restrict with `disallowedTools`; allowlisting `mcp__<server>__*` is untested).
  Cloudflare was not tried.
