/**
 * hsup gate: turns pi tool calls into permission prompts a supervisor can
 * answer over RPC, and registers an ask_user tool for questions.
 *
 * Prompt shapes (extension_ui_request titles):
 *   select "hsup-permission <json>"  options allow_once | allow_session | deny
 *   input  "hsup-deny-reason"        follow-up after deny
 *   select "hsup-ask <json>"         ask_user
 */

import { realpathSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

const SAFE_BASH = new Set(["echo", "ls", "pwd", "cat", "true"]);
const READ_TOOLS = new Set(["read", "ls", "grep", "find"]);
const WRITE_TOOLS = new Set(["write", "edit"]);
const WEB_TOOLS = new Set(["web_search", "fetch_content"]);
// Shell metacharacters that make a command more than one word-list call.
const NOT_SIMPLE = /[;&|<>$`(){}\n\r]/;
// Bash expands quotes, backslashes, ~ and globs before running, so only words
// without them can be judged lexically.
const PLAIN_WORD = /^[A-Za-z0-9._/@%+=,-]+$/;
// Options carry no attached value, which a command could read as a file.
const PLAIN_OPTION = /^--?[A-Za-z0-9-]+$/;

interface Ask {
  kind: "bash" | "write" | "read" | "mcp" | "web" | "other";
  // Session-cache keys; empty means every call must be asked.
  keys: string[];
  command?: string;
  path?: string;
  requested?: string;
  url?: string;
  domain?: string;
  server?: string;
}

// Mirrors pi's normalizePath so the gate judges the path pi will touch.
function normalizePath(raw: string, cwd: string): string {
  let p = raw.replace(/[\u00A0\u2000-\u200A\u202F\u205F\u3000]/g, " ");
  if (p.startsWith("@")) p = p.slice(1);
  if (p.startsWith("file://")) p = fileURLToPath(p);
  if (p === "~") p = homedir();
  else if (p.startsWith("~/")) p = homedir() + p.slice(1);
  return resolve(cwd, p);
}

// realpath of the path, or of its nearest existing parent plus the rest.
function real(p: string): string {
  let head = p;
  let tail = "";
  for (;;) {
    try {
      return resolve(realpathSync(head), tail);
    } catch {
      const parent = dirname(head);
      if (parent === head) return p;
      tail = relative(parent, head) + (tail ? `/${tail}` : "");
      head = parent;
    }
  }
}

function outsideCwd(path: string, cwd: string): boolean {
  const rel = relative(real(cwd), real(path));
  return rel === ".." || rel.startsWith("../") || isAbsolute(rel);
}

function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

function classifyBash(command: string, cwd: string): Ask | undefined {
  if (NOT_SIMPLE.test(command)) return { kind: "bash", keys: [], command };
  const words = command.trim().split(/\s+/);
  const first = words[0] ?? "";
  const args = words.slice(1);
  // Claude's Bash(touch:*) prefix rule also keys on the first word, but it
  // checks each subcommand; only plain-word commands reach this key.
  const plain = PLAIN_WORD.test(first) && args.every((w) => PLAIN_WORD.test(w));
  const ask: Ask = {
    kind: "bash",
    keys: plain ? [`bash:${first}`] : [],
    command,
  };
  if (!plain || !SAFE_BASH.has(first)) return ask;
  const safe = args.every((w) =>
    w.startsWith("-")
      ? PLAIN_OPTION.test(w)
      : !outsideCwd(normalizePath(w, cwd), cwd),
  );
  return safe ? undefined : ask;
}

function classifyWeb(input: Record<string, unknown>): Ask {
  const urls: string[] = [];
  const one = str(input.url);
  if (one !== undefined) urls.push(one);
  if (Array.isArray(input.urls)) {
    for (const u of input.urls) if (typeof u === "string") urls.push(u);
  }
  const domains = urls.map((u) => {
    try {
      return new URL(u).hostname;
    } catch {
      return "";
    }
  });
  const keys =
    domains.length > 0 && domains.every((d) => d)
      ? domains.map((d) => `web:${d}`)
      : [];
  return { kind: "web", keys, url: urls[0] ?? "", domain: domains[0] ?? "" };
}

function classify(
  tool: string,
  input: Record<string, unknown>,
  cwd: string,
): Ask | undefined {
  if (tool === "ask_user") return undefined;
  if (tool === "bash") return classifyBash(str(input.command) ?? "", cwd);
  if (WRITE_TOOLS.has(tool)) {
    const requested = normalizePath(str(input.path) ?? "", cwd);
    const path = real(requested);
    return {
      kind: "write",
      keys: [`${tool}:${dirname(path)}`],
      path,
      requested,
    };
  }
  if (READ_TOOLS.has(tool)) {
    const requested = normalizePath(str(input.path) ?? ".", cwd);
    const path = real(requested);
    if (!outsideCwd(path, cwd)) return undefined;
    return {
      kind: "read",
      keys: [`${tool}:${dirname(path)}`],
      path,
      requested,
    };
  }
  if (tool.startsWith("mcp__")) {
    return { kind: "mcp", keys: [tool], server: tool.split("__")[1] };
  }
  if (WEB_TOOLS.has(tool)) return classifyWeb(input);
  return { kind: "other", keys: [`tool:${tool}`] };
}

export default function (pi: ExtensionAPI) {
  const allowed = new Set<string>();

  pi.on("tool_call", async (event, ctx) => {
    const ask = classify(event.toolName, event.input, ctx.cwd);
    if (!ask) return undefined;
    const { keys, ...detail } = ask;
    if (keys.length > 0 && keys.every((k) => allowed.has(k))) return undefined;

    const payload = JSON.stringify({
      ...detail,
      tool: event.toolName,
      input: event.input,
    });
    const choice = await ctx.ui.select(`hsup-permission ${payload}`, [
      "allow_once",
      "allow_session",
      "deny",
    ]);

    if (choice === "allow_once") return undefined;
    if (choice === "allow_session") {
      for (const k of keys) allowed.add(k);
      return undefined;
    }
    if (choice === "deny") {
      const reason = await ctx.ui.input("hsup-deny-reason", "");
      return { block: true, reason: reason || "denied" };
    }
    return { block: true, reason: "no decision" };
  });

  pi.registerTool({
    name: "ask_user",
    label: "Ask user",
    description:
      "Ask the user a question and let them pick one of the options. Use when you need input to proceed.",
    parameters: Type.Object({
      question: Type.String({ description: "The question to ask" }),
      options: Type.Array(Type.String(), {
        description: "Options to choose from",
      }),
    }),
    async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
      const answer = await ctx.ui.select(
        `hsup-ask ${JSON.stringify(params)}`,
        params.options,
      );
      return {
        content: [{ type: "text", text: answer ?? "no answer" }],
        details: { question: params.question, answer: answer ?? null },
      };
    },
  });
}
