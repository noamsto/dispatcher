import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { KILL_GRACE_MS, terminateChild, recordOutcome, isFailedResult, type ResultStatus } from "./outcome.ts";

const IGNORE_SIGTERM = `process.on("SIGTERM", () => {});`;
const LONG_RUNNING = `setInterval(() => {}, 1000);`;

function spawnChild(script: string): ChildProcess {
	return spawn(process.execPath, ["-e", script], {
		stdio: ["ignore", "pipe", "pipe"],
	});
}

function onceReady(proc: ChildProcess): Promise<void> {
	return new Promise((resolve, reject) => {
		const onData = (): void => {
			proc.stdout?.off("data", onData);
			resolve();
		};
		proc.stdout?.on("data", onData);
		proc.once("error", reject);
	});
}

// The child installs its signal handlers before printing `ready`, so waiting for
// that line is what keeps the test from racing the handler install.
function onceClose(proc: ChildProcess): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
	return new Promise((resolve) => {
		proc.once("close", (code, signal) => resolve({ code, signal }));
	});
}

test("a child that ignores SIGTERM is escalated to SIGKILL after the grace period", { timeout: 5000 }, async () => {
	const proc = spawnChild(`${IGNORE_SIGTERM} console.log("ready"); ${LONG_RUNNING}`);
	const closing = onceClose(proc);
	await onceReady(proc);
	const started = Date.now();
	const cancel = terminateChild(proc, 50);
	assert.equal(typeof cancel, "function");
	const { code, signal } = await closing;
	const elapsed = Date.now() - started;
	assert.equal(signal, "SIGKILL");
	assert.equal(code, null);
	assert.ok(elapsed >= 50, `expected the 50 ms grace to be honoured, took ${elapsed} ms`);
	assert.ok(elapsed < 5000, `expected the passed-in grace, took ${elapsed} ms`);
});

test("a child that handles SIGTERM exits by itself and is never SIGKILLed", { timeout: 5000 }, async () => {
	const proc = spawnChild(`process.on("SIGTERM", () => process.exit(0)); console.log("ready"); ${LONG_RUNNING}`);
	const closing = onceClose(proc);
	await onceReady(proc);
	const cancel = terminateChild(proc, 5000);
	assert.equal(typeof cancel, "function");
	const { code, signal } = await closing;
	cancel?.();
	assert.equal(signal, null);
	assert.equal(code, 0);
});

test("cancel() stops the pending SIGKILL", { timeout: 5000 }, async () => {
	const proc = spawnChild(`${IGNORE_SIGTERM} console.log("ready"); ${LONG_RUNNING}`);
	const closing = onceClose(proc);
	await onceReady(proc);
	const cancel = terminateChild(proc, 40);
	assert.equal(typeof cancel, "function");
	cancel?.();
	await new Promise((resolve) => setTimeout(resolve, 250));
	assert.equal(proc.exitCode, null);
	assert.equal(proc.signalCode, null);
	proc.kill("SIGKILL");
	await closing;
});

test("a signal-killed child is a failed result carrying its termination signal", { timeout: 5000 }, async () => {
	for (const sig of ["SIGKILL", "SIGTERM"] as const) {
		const proc = spawnChild(`console.log("ready"); ${LONG_RUNNING}`);
		const closing = onceClose(proc);
		await onceReady(proc);
		proc.kill(sig);
		const outcome = await closing;
		assert.equal(outcome.signal, sig);
		const result: ResultStatus = { exitCode: 0 };
		recordOutcome(result, outcome);
		assert.equal(isFailedResult(result), true);
		assert.equal(result.terminationSignal, sig);
		assert.equal(result.exitCode, 1);
		assert.match(result.errorMessage ?? "", /terminated by signal/);
	}
});

test("a normal exit 0 is a success", { timeout: 5000 }, async () => {
	const proc = spawnChild(`console.log("ready"); process.exit(0);`);
	const closing = onceClose(proc);
	await onceReady(proc);
	const outcome = await closing;
	const result: ResultStatus = { exitCode: 0 };
	recordOutcome(result, outcome);
	assert.equal(isFailedResult(result), false);
	assert.equal(result.terminationSignal, undefined);
	assert.equal(result.exitCode, 0);
});

test("isFailedResult keeps the exit-code and stopReason rules", () => {
	assert.equal(isFailedResult({ exitCode: 0 }), false);
	assert.equal(isFailedResult({ exitCode: 1 }), true);
	assert.equal(isFailedResult({ exitCode: 0, stopReason: "aborted" }), true);
	assert.equal(isFailedResult({ exitCode: 0, stopReason: "error" }), true);
	assert.equal(isFailedResult({ exitCode: 0, terminationSignal: "SIGTERM" }), true);
});

test("the default grace is 5000 ms", () => {
	assert.equal(KILL_GRACE_MS, 5000);
});
