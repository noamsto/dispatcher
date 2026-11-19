import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { crewId, finishBody, nextRunId, postSubagent, spawnBody, splitModel, subagentLabel, type SpawnFn } from "./crewbus.ts";

function scratch(): string {
	return fs.mkdtempSync(path.join(os.tmpdir(), "crewbus-"));
}

function repoWithTask(header: string): { top: string; sub: string } {
	const top = scratch();
	fs.mkdirSync(path.join(top, ".git"));
	fs.writeFileSync(path.join(top, "WORKER_TASK.md"), header);
	const sub = path.join(top, "a", "b");
	fs.mkdirSync(sub, { recursive: true });
	return { top, sub };
}

interface Call {
	command: string;
	args: string[];
	options: Record<string, unknown>;
}

function recorder(calls: Call[], unrefs: string[] = []): SpawnFn {
	return (command, args, options) => {
		calls.push({ command, args, options: { ...options } });
		return {
			on: (event: string) => {
				unrefs.push(`on:${event}`);
			},
			unref: () => {
				unrefs.push("unref");
			},
		} as never;
	};
}

const WORKER_ENV = { CREW_WORKER_ID: "worker:feat/x#s1", CREW_ID: "c1" };

test("body builders drop absent fields", () => {
	assert.deepEqual(spawnBody({ id: "i", engine: "pi", label: "", agent: undefined, model: "m" }), {
		subagent: "spawn",
		id: "i",
		engine: "pi",
		model: "m",
	});
	assert.deepEqual(finishBody({ id: "i", outcome: "ok", model: undefined, effort: "" }), {
		subagent: "finish",
		id: "i",
		outcome: "ok",
	});
});

test("spawn label is clipped to 80 chars", () => {
	assert.equal(spawnBody({ id: "i", label: "x".repeat(200) }).label?.length, 80);
});

test("splitModel", () => {
	assert.deepEqual(splitModel("anthropic/sonnet:high"), { model: "anthropic/sonnet", effort: "high" });
	assert.deepEqual(splitModel("anthropic/sonnet"), { model: "anthropic/sonnet", effort: undefined });
	assert.deepEqual(splitModel("local:8080"), { model: "local:8080", effort: undefined });
	assert.deepEqual(splitModel("m", "low"), { model: "m", effort: "low" });
	assert.deepEqual(splitModel(undefined, "low"), { model: undefined, effort: "low" });
	assert.deepEqual(splitModel("m:xhigh", "low"), { model: "m", effort: "xhigh" });
});

test("subagentLabel", () => {
	assert.equal(subagentLabel("scout", "\n  find   the\tthing \nmore"), "scout find the thing");
	assert.equal(subagentLabel("scout", "go", 0), "scout #1 go");
	assert.equal(subagentLabel("scout", "   "), "scout");
	assert.equal(subagentLabel("scout", "y".repeat(200)).length, 80);
});

test("crewId reads the task header at the git toplevel from a subdir", () => {
	const { sub } = repoWithTask("tier: standard\ncrew_id: from-doc\n\ncrew_id: in-body\n");
	assert.equal(crewId({ CREW_ID: "env" }, sub), "from-doc");
});

test("crewId ignores crew_id after the header", () => {
	const { sub } = repoWithTask("tier: standard\n\ncrew_id: in-body\n");
	assert.equal(crewId({ CREW_ID: "env" }, sub), "env");
});

test("crewId falls back to CREW_ID, else undefined", () => {
	const top = scratch();
	fs.mkdirSync(path.join(top, ".git"));
	assert.equal(crewId({ CREW_ID: "env" }, top), "env");
	assert.equal(crewId({}, top), undefined);
});

test("nextRunId is pid-scoped and increasing", () => {
	const a = nextRunId();
	const b = nextRunId();
	assert.match(a, new RegExp(`^pi-${process.pid}-\\d+$`));
	assert.notEqual(a, b);
});

test("postSubagent spawns crew msg with exact argv and detaches", () => {
	const { sub } = repoWithTask("crew_id: c1\n");
	const calls: Call[] = [];
	const events: string[] = [];
	const spawnBodyValue = spawnBody({ id: "pi-1-1", engine: "pi", label: "scout go" });
	postSubagent(WORKER_ENV, sub, spawnBodyValue, recorder(calls, events));
	postSubagent(WORKER_ENV, sub, finishBody({ id: "pi-1-1", outcome: "ok" }), recorder(calls));
	assert.deepEqual(calls[0].args, ["msg", "worker:feat/x#s1", "subagent:c1", JSON.stringify(spawnBodyValue)]);
	assert.deepEqual(calls[1].args, [
		"msg",
		"worker:feat/x#s1",
		"subagent:c1",
		'{"subagent":"finish","id":"pi-1-1","outcome":"ok"}',
	]);
	assert.equal(calls[0].command, "crew");
	assert.equal(calls[0].options.cwd, sub);
	assert.equal(calls[0].options.stdio, "ignore");
	assert.equal(calls[0].options.shell, false);
	assert.deepEqual(events, ["on:error", "unref"]);
});

test("postSubagent does not spawn under any guard", () => {
	const { sub } = repoWithTask("crew_id: c1\n");
	const body = spawnBody({ id: "i" });
	const cases: Record<string, NodeJS.ProcessEnv> = {
		"no worker id": { CREW_ID: "c1" },
		"role pane": { ...WORKER_ENV, CREW_ROLE_ID: "r" },
		"subagent child": { ...WORKER_ENV, CREW_SUBAGENT: "1" },
	};
	for (const [name, env] of Object.entries(cases)) {
		const calls: Call[] = [];
		postSubagent(env, sub, body, recorder(calls));
		assert.equal(calls.length, 0, name);
	}
	const bare = scratch();
	fs.mkdirSync(path.join(bare, ".git"));
	const calls: Call[] = [];
	postSubagent({ CREW_WORKER_ID: "w" }, bare, body, recorder(calls));
	assert.equal(calls.length, 0, "no crew id");
});

test("a throwing spawn function does not throw", () => {
	const { sub } = repoWithTask("crew_id: c1\n");
	assert.doesNotThrow(() =>
		postSubagent(WORKER_ENV, sub, spawnBody({ id: "i" }), () => {
			throw new Error("boom");
		}),
	);
});
