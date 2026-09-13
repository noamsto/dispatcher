import { test, after } from "node:test";
import assert from "node:assert/strict";
import { execFile, execFileSync } from "node:child_process";
import { promisify } from "node:util";
import {
  mkdtempSync,
  writeFileSync,
  chmodSync,
  mkdirSync,
  readFileSync,
  existsSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import crewBus from "../crew-bus.ts";

const execFileAsync = promisify(execFile);

const worktreeRoot = fileURLToPath(new URL("../../..", import.meta.url));
const crewSh = path.join(worktreeRoot, "adapters/core/crew.sh");
const stallCheckSh = path.join(worktreeRoot, "spikes/pi-bus/stall-check.sh");

const ENV_KEYS = [
  "CREW_WORKER_ID",
  "CREW_BIN",
  "CREW_ID",
  "CREW_HEARTBEAT_S",
] as const;

function saveEnv(): Record<string, string | undefined> {
  const saved: Record<string, string | undefined> = {};
  for (const key of ENV_KEYS) saved[key] = process.env[key];
  return saved;
}

function restoreEnv(saved: Record<string, string | undefined>): void {
  for (const key of ENV_KEYS) {
    if (saved[key] === undefined) delete process.env[key];
    else process.env[key] = saved[key];
  }
}

const tempDirs: string[] = [];
after(() => {
  for (const dir of tempDirs) rmSync(dir, { recursive: true, force: true });
});

function makeTempRepo(withWorkerTask: boolean): string {
  const dir = mkdtempSync(path.join(tmpdir(), "crew-bus-test-"));
  tempDirs.push(dir);
  execFileSync("git", ["init", "-q"], { cwd: dir });
  if (withWorkerTask) {
    writeFileSync(path.join(dir, "WORKER_TASK.md"), "crew_id: test-crew\n");
  }
  return dir;
}

function makeCrewBin(dir: string): string {
  const binPath = path.join(dir, "crew-bin.sh");
  writeFileSync(binPath, `#!/usr/bin/env bash\nexec bash "${crewSh}" "$@"\n`);
  chmodSync(binPath, 0o755);
  return binPath;
}

type FakeExecOpts = { cwd?: string; timeout?: number; signal?: AbortSignal };
type FakeExecResult = {
  stdout: string;
  stderr: string;
  code: number;
  killed: boolean;
};

function makeFakePi(defaultCwd: string) {
  const hooks: Record<string, Array<() => void>> = {};
  const tools: any[] = [];
  const calls: Array<{ cmd: string; args: string[] }> = [];
  const pi = {
    on(name: string, fn: () => void) {
      (hooks[name] ??= []).push(fn);
    },
    registerTool(def: any) {
      tools.push(def);
    },
    async exec(
      cmd: string,
      args: string[],
      opts?: FakeExecOpts,
    ): Promise<FakeExecResult> {
      calls.push({ cmd, args });
      try {
        const { stdout, stderr } = await execFileAsync(cmd, args, {
          cwd: opts?.cwd ?? defaultCwd,
          timeout: opts?.timeout,
          signal: opts?.signal,
        });
        return { stdout, stderr, code: 0, killed: false };
      } catch (err: any) {
        return {
          stdout: err.stdout ?? "",
          stderr: err.stderr ?? "",
          code: typeof err.code === "number" ? err.code : 1,
          killed: Boolean(err.killed),
        };
      }
    },
  };
  return { pi, hooks, tools, calls };
}

function readRows(dir: string): any[] {
  const logPath = path.join(dir, ".git/crew/events.jsonl");
  if (!existsSync(logPath)) return [];
  return readFileSync(logPath, "utf8")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

function countHeartbeatRows(dir: string): number {
  return readRows(dir).filter(
    (r) =>
      r.kind === "msg" &&
      typeof r.to === "string" &&
      r.to.startsWith("heartbeat:"),
  ).length;
}

async function waitFor(fn: () => boolean, timeoutMs = 3000): Promise<void> {
  const start = Date.now();
  while (!fn()) {
    if (Date.now() - start > timeoutMs)
      throw new Error("timed out waiting for condition");
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

test("no CREW_WORKER_ID: registers nothing", async () => {
  const dir = makeTempRepo(true);
  const bin = makeCrewBin(dir);
  const { pi, hooks, tools } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    delete process.env.CREW_WORKER_ID;
    process.env.CREW_BIN = bin;
    await crewBus(pi as any);
    assert.equal(tools.length, 0);
    assert.equal(Object.keys(hooks).length, 0);
  } finally {
    restoreEnv(saved);
  }
});

test("CREW_WORKER_ID set: registers 4 tools and 3 hooks", async () => {
  const dir = makeTempRepo(true);
  const bin = makeCrewBin(dir);
  const { pi, hooks, tools } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    assert.equal(tools.length, 4);
    assert.deepEqual(
      new Set(tools.map((t) => t.name)),
      new Set(["crew_status", "crew_msg", "crew_inbox", "crew_roster"]),
    );
    assert.deepEqual(
      new Set(Object.keys(hooks)),
      new Set(["turn_start", "tool_execution_start", "agent_settled"]),
    );
  } finally {
    restoreEnv(saved);
  }
});

test("factory returns without calling `crew id`", async () => {
  const dir = makeTempRepo(true);
  const bin = makeCrewBin(dir);
  const { pi, tools, calls } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    assert.equal(tools.length, 4);
    assert.ok(
      !calls.some((c) => c.args[0] === "id"),
      "factory must not call `crew id` before any event fires",
    );
  } finally {
    restoreEnv(saved);
  }
});

test("crew_status execute writes a status row", async () => {
  const dir = makeTempRepo(true);
  const bin = makeCrewBin(dir);
  const { pi, tools } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    const statusTool = tools.find((t) => t.name === "crew_status");
    const result = await statusTool.execute(
      "tc1",
      { state: "working" },
      undefined,
      undefined,
      { cwd: dir },
    );
    assert.equal(result.details.code, 0);
    const statusRows = readRows(dir).filter((r) => r.kind === "status");
    assert.equal(statusRows.length, 1);
    assert.equal(statusRows[0].from, "worker:feat/x#s1");
    assert.equal(statusRows[0].body.state, "working");
  } finally {
    restoreEnv(saved);
  }
});

test("heartbeats: two quick turn_start + one agent_settled write exactly 2 rows", async () => {
  const dir = makeTempRepo(true);
  const bin = makeCrewBin(dir);
  const { pi, hooks } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    process.env.CREW_HEARTBEAT_S = "60";
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    hooks.turn_start[0]();
    hooks.turn_start[0]();
    hooks.agent_settled[0]();
    await waitFor(() => countHeartbeatRows(dir) >= 2);
    await new Promise((resolve) => setTimeout(resolve, 200));
    assert.equal(countHeartbeatRows(dir), 2);
  } finally {
    restoreEnv(saved);
  }
});

test("heartbeat msg row has the same key set as a row `crew msg` writes directly", async () => {
  const dir = makeTempRepo(true);
  const bin = makeCrewBin(dir);
  const { pi, hooks } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    hooks.agent_settled[0]();
    await waitFor(() => countHeartbeatRows(dir) >= 1);
    await execFileAsync(
      bin,
      ["msg", "worker:feat/x#s1", "heartbeat:test-crew", "hello"],
      { cwd: dir },
    );
    const hbRows = readRows(dir).filter(
      (r) => r.kind === "msg" && r.to === "heartbeat:test-crew",
    );
    assert.equal(hbRows.length, 2);
    assert.deepEqual(
      Object.keys(hbRows[0]).sort(),
      Object.keys(hbRows[1]).sort(),
    );
  } finally {
    restoreEnv(saved);
  }
});

test("no crew id resolvable: heartbeats disabled but tools still register", async () => {
  const dir = makeTempRepo(false);
  const bin = makeCrewBin(dir);
  const { pi, hooks, tools } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    assert.equal(tools.length, 4);
    hooks.turn_start[0]();
    hooks.agent_settled[0]();
    await new Promise((resolve) => setTimeout(resolve, 300));
    assert.equal(countHeartbeatRows(dir), 0);
  } finally {
    restoreEnv(saved);
  }
});

test("a later heartbeat succeeds after an initial resolution failure", async () => {
  const dir = makeTempRepo(false);
  const bin = makeCrewBin(dir);
  const { pi, hooks } = makeFakePi(dir);
  const saved = saveEnv();
  try {
    process.env.CREW_WORKER_ID = "worker:feat/x#s1";
    process.env.CREW_BIN = bin;
    delete process.env.CREW_ID;
    await crewBus(pi as any);
    hooks.agent_settled[0]();
    await new Promise((resolve) => setTimeout(resolve, 300));
    assert.equal(countHeartbeatRows(dir), 0);

    writeFileSync(path.join(dir, "WORKER_TASK.md"), "crew_id: test-crew\n");
    hooks.agent_settled[0]();
    await waitFor(() => countHeartbeatRows(dir) >= 1);
    assert.equal(countHeartbeatRows(dir), 1);
  } finally {
    restoreEnv(saved);
  }
});

test("stall-check.sh: no-heartbeat for a status-only worker, stale for a worker with an old heartbeat", async () => {
  const dir = makeTempRepo(true);
  const logPath = path.join(dir, ".git/crew/events.jsonl");
  mkdirSync(path.dirname(logPath), { recursive: true });
  const now = Date.now();
  const rows = [
    {
      ts: now,
      crew_id: "test-crew",
      from: "worker:feat/a#s1",
      to: "dispatcher:test-crew",
      kind: "status",
      body: { state: "working" },
    },
    {
      ts: now - 5000,
      crew_id: "test-crew",
      from: "worker:feat/b#s2",
      to: "dispatcher:test-crew",
      kind: "status",
      body: { state: "working" },
    },
    {
      ts: now - 5000,
      crew_id: "test-crew",
      from: "worker:feat/b#s2",
      to: "heartbeat:test-crew",
      kind: "msg",
      body: JSON.stringify({ event: "turn_start", ts: now - 5000 }),
    },
  ];
  writeFileSync(logPath, rows.map((r) => JSON.stringify(r)).join("\n") + "\n");
  const { stdout } = await execFileAsync("bash", [stallCheckSh, "0", logPath]);
  const lines = stdout.trim().split("\n");
  assert.ok(lines.includes("no-heartbeat worker:feat/a"));
  assert.ok(lines.some((l) => l.startsWith("stale worker:feat/b ")));
});

test("stall-check.sh: a non-object status body doesn't crash the script or hide other workers", async () => {
  const dir = makeTempRepo(true);
  const logPath = path.join(dir, ".git/crew/events.jsonl");
  mkdirSync(path.dirname(logPath), { recursive: true });
  const now = Date.now();
  const rows = [
    {
      ts: now,
      crew_id: "test-crew",
      from: "worker:feat/a#s1",
      to: "dispatcher:test-crew",
      kind: "status",
      body: { state: "working" },
    },
    {
      ts: now,
      crew_id: "test-crew",
      from: "worker:feat/c#s3",
      to: "dispatcher:test-crew",
      kind: "status",
      body: "not-an-object",
    },
  ];
  writeFileSync(logPath, rows.map((r) => JSON.stringify(r)).join("\n") + "\n");
  const { stdout } = await execFileAsync("bash", [stallCheckSh, "0", logPath]);
  const lines = stdout.trim().split("\n");
  assert.ok(lines.includes("no-heartbeat worker:feat/a"));
  assert.ok(lines.includes("no-heartbeat worker:feat/c"));
});
