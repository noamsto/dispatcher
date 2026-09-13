import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import {
  statusArgs,
  msgArgs,
  inboxArgs,
  rosterArgs,
  heartbeatBody,
  createThrottle,
  parseHeartbeatSeconds,
} from "./bus.ts";

const STATES = ["working", "blocked", "pr_open", "done", "failed"] as const;

export default function (pi: ExtensionAPI) {
  const workerId = process.env.CREW_WORKER_ID;
  if (!workerId) return; // dispatch sets this per window; no-op everywhere else

  const bin = process.env.CREW_BIN || "crew";
  const heartbeatS = parseHeartbeatSeconds(process.env.CREW_HEARTBEAT_S);

  const throttle = createThrottle(heartbeatS * 1000, Date.now);

  async function run(args: string[], cwd: string, signal?: AbortSignal) {
    const result = await pi.exec(bin, args, { cwd, timeout: 30000, signal });
    if (result.code !== 0) {
      throw new Error(
        result.stderr || `${bin} ${args.join(" ")} exited ${result.code}`,
      );
    }
    return result;
  }

  pi.registerTool({
    name: "crew_status",
    label: "Crew Status",
    description: "Post this worker's status to the crew bus.",
    promptGuidelines: [
      "Use crew_status instead of running `crew status` in bash.",
    ],
    parameters: {
      type: "object",
      properties: {
        state: { type: "string", enum: STATES },
        detail: { type: "string" },
        pr: { type: "string" },
      },
      required: ["state"],
    },
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      const { state, detail, pr } = params as {
        state: string;
        detail?: string;
        pr?: string;
      };
      const result = await run(
        statusArgs(workerId, state, detail, pr),
        ctx.cwd,
        signal,
      );
      return {
        content: [{ type: "text", text: result.stdout || "ok" }],
        details: { code: result.code },
      };
    },
  });

  pi.registerTool({
    name: "crew_msg",
    label: "Crew Message",
    description: "Send a message to another crew agent or sink over the bus.",
    promptGuidelines: ["Use crew_msg instead of running `crew msg` in bash."],
    parameters: {
      type: "object",
      properties: {
        to: { type: "string" },
        body: { type: "string" },
      },
      required: ["to", "body"],
    },
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      const { to, body } = params as { to: string; body: string };
      const result = await run(msgArgs(workerId, to, body), ctx.cwd, signal);
      return {
        content: [{ type: "text", text: result.stdout || "ok" }],
        details: { code: result.code },
      };
    },
  });

  pi.registerTool({
    name: "crew_inbox",
    label: "Crew Inbox",
    description: "Read this worker's pending bus messages.",
    promptGuidelines: [
      "Use crew_inbox instead of running `crew inbox` in bash.",
    ],
    parameters: {
      type: "object",
      properties: {
        since: { type: "number" },
      },
    },
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      const { since } = params as { since?: number };
      const result = await run(inboxArgs(workerId, since), ctx.cwd, signal);
      return {
        content: [{ type: "text", text: result.stdout || "ok" }],
        details: { code: result.code },
      };
    },
  });

  pi.registerTool({
    name: "crew_roster",
    label: "Crew Roster",
    description: "Read the crew roster (latest status per worker).",
    promptGuidelines: [
      "Use crew_roster instead of running `crew roster` in bash.",
    ],
    parameters: { type: "object", properties: {} },
    async execute(_toolCallId, _params, signal, _onUpdate, ctx) {
      const result = await run(rosterArgs(), ctx.cwd, signal);
      return {
        content: [{ type: "text", text: result.stdout || "ok" }],
        details: { code: result.code },
      };
    },
  });

  // Resolved on the first heartbeat, not at load: pi awaits extension
  // factories serially, so a `crew id` here would delay session start.
  // Cached only on success, so a missing WORKER_TASK.md is retried later.
  let crewId: string | undefined;

  async function resolveCrewId(): Promise<string | undefined> {
    if (crewId) return crewId;
    try {
      const idResult = await pi.exec(bin, ["id"], { timeout: 10000 });
      if (idResult.code === 0 && idResult.stdout.trim()) {
        crewId = idResult.stdout.trim();
        return crewId;
      }
    } catch {}
    if (process.env.CREW_ID) crewId = process.env.CREW_ID;
    return crewId;
  }

  async function postHeartbeat(event: string) {
    const id = await resolveCrewId();
    // Check the id first so a failed lookup doesn't use up the throttle window.
    if (!id || !throttle.shouldPost(event)) return;
    try {
      await pi.exec(
        bin,
        msgArgs(workerId, `heartbeat:${id}`, heartbeatBody(event, Date.now())),
        { timeout: 10000 },
      );
    } catch {
      // heartbeats are best-effort; a hook must never throw
    }
  }

  pi.on("turn_start", () => {
    void postHeartbeat("turn_start");
  });
  pi.on("tool_execution_start", () => {
    void postHeartbeat("tool_execution_start");
  });
  pi.on("agent_settled", () => {
    void postHeartbeat("agent_settled");
  });
}
