// Crew-bus records of a subagent run: the body schema, the lead's crew id lookup and a
// fire-and-forget `crew msg` post. index.ts imports it; keep this free of pi imports so
// the pi-subagent-behaviour check can run it under node's test runner.

import { spawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";

const LABEL_MAX = 80;
const EFFORT_LEVELS = new Set(["off", "minimal", "low", "medium", "high", "xhigh"]);

export interface SubagentBody {
	subagent: "spawn" | "finish";
	id?: string;
	agent_id?: string;
	engine?: string;
	label?: string;
	agent?: string;
	model?: string;
	effort?: string;
	outcome?: "ok" | "error" | "aborted";
}

function compact(body: SubagentBody): SubagentBody {
	const out: Record<string, unknown> = {};
	for (const [key, value] of Object.entries(body)) {
		if (value !== undefined && value !== "") out[key] = value;
	}
	return out as unknown as SubagentBody;
}

function clip(text: string, max: number): string {
	return text.length > max ? text.slice(0, max) : text;
}

export function spawnBody(fields: Omit<SubagentBody, "subagent" | "outcome">): SubagentBody {
	return compact({ subagent: "spawn", ...fields, label: fields.label && clip(fields.label, LABEL_MAX) });
}

export function finishBody(fields: Omit<SubagentBody, "subagent">): SubagentBody {
	return compact({ subagent: "finish", ...fields });
}

/** A trailing `:<level>` is the effort; any other suffix (a port, a tag) stays in the model. */
export function splitModel(model?: string, inheritedThinking?: string): { model?: string; effort?: string } {
	if (model) {
		const colon = model.lastIndexOf(":");
		if (colon > 0 && EFFORT_LEVELS.has(model.slice(colon + 1))) {
			return { model: model.slice(0, colon), effort: model.slice(colon + 1) };
		}
	}
	return { model, effort: inheritedThinking };
}

/** `step` is the 0-based chain index. */
export function subagentLabel(agentName: string, task: string, step?: number): string {
	const firstLine = task.split("\n").find((line) => line.trim() !== "") ?? "";
	const parts = [agentName];
	if (step !== undefined) parts.push(`#${step + 1}`);
	const summary = firstLine.replace(/\s+/g, " ").trim();
	if (summary) parts.push(summary);
	return clip(parts.join(" "), LABEL_MAX);
}

function gitToplevel(cwd: string): string | undefined {
	let dir = path.resolve(cwd);
	for (;;) {
		if (fs.existsSync(path.join(dir, ".git"))) return dir;
		const parent = path.dirname(dir);
		if (parent === dir) return undefined;
		dir = parent;
	}
}

/** Resolves like `crew id`: WORKER_TASK.md at the git toplevel, then CREW_ID. */
export function crewId(env: NodeJS.ProcessEnv, cwd: string): string | undefined {
	const top = gitToplevel(cwd);
	if (top) {
		try {
			const text = fs.readFileSync(path.join(top, "WORKER_TASK.md"), "utf8");
			for (const line of text.split("\n")) {
				if (line.trim() === "") break;
				const match = /^crew_id:\s*(\S+)/.exec(line);
				if (match) return match[1];
			}
		} catch {
			/* no task doc: fall through to CREW_ID */
		}
	}
	return env.CREW_ID || undefined;
}

export type SpawnFn = (command: string, args: string[], options: SpawnOptions) => Pick<ChildProcess, "on" | "unref">;

let seq = 0;

export function nextRunId(): string {
	seq += 1;
	return `pi-${process.pid}-${seq}`;
}

/** Never awaits and never throws: a lost record must not disturb the subagent run. */
export function postSubagent(env: NodeJS.ProcessEnv, cwd: string, body: SubagentBody, spawnFn: SpawnFn = spawn): void {
	try {
		const worker = env.CREW_WORKER_ID;
		if (!worker || env.CREW_ROLE_ID || env.CREW_SUBAGENT) return;
		const crew = crewId(env, cwd);
		if (!crew) return;
		const child = spawnFn("crew", ["msg", worker, `subagent:${crew}`, JSON.stringify(body)], {
			cwd,
			stdio: "ignore",
			shell: false,
		});
		child.on("error", () => {});
		child.unref();
	} catch {
		/* best effort */
	}
}
