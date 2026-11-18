// The subagent child's exit handling, split out of index.ts so a test can run it
// against real child processes without pi: the SIGTERM-then-SIGKILL escalation and
// how an exit code/signal pair classifies as a failure. Import nothing but Node.

import type { ChildProcess } from "node:child_process";

// How long a SIGTERM'd child gets to die before the escalation to SIGKILL.
export const KILL_GRACE_MS = 5000;

export interface ProcessOutcome {
	code: number | null;
	signal: NodeJS.Signals | null;
}

/**
 * SIGTERM the child, then SIGKILL it if it is still running after `graceMs`.
 * Returns a function that cancels the pending SIGKILL, or null when the child
 * already exited or could not be signalled. proc.killed is set as soon as SIGTERM
 * is sent, so it cannot gate the escalation.
 */
export function terminateChild(proc: ChildProcess, graceMs: number = KILL_GRACE_MS): (() => void) | null {
	if (proc.exitCode !== null || proc.signalCode !== null) return null;
	if (!proc.kill("SIGTERM")) return null;
	const timer = setTimeout(() => {
		if (proc.exitCode === null && proc.signalCode === null) proc.kill("SIGKILL");
	}, graceMs);
	return () => clearTimeout(timer);
}

/** Result fields `recordOutcome` and `isFailedResult` read; `SingleResult` satisfies it. */
export interface ResultStatus {
	exitCode: number;
	stopReason?: string;
	terminationSignal?: NodeJS.Signals;
	errorMessage?: string;
}

/**
 * Fold a child's exit into a result. A signal exit is never a success: the signal
 * is recorded and the exit code forced non-zero. Keeps an errorMessage the JSON
 * stream already set.
 */
export function recordOutcome(result: ResultStatus, outcome: ProcessOutcome): void {
	result.exitCode = outcome.code ?? 1;
	result.terminationSignal = outcome.signal ?? undefined;
	if (outcome.signal) result.errorMessage ||= `Subagent terminated by signal ${outcome.signal}`;
}

export function isFailedResult(result: ResultStatus): boolean {
	return (
		result.exitCode !== 0 ||
		result.stopReason === "error" ||
		result.stopReason === "aborted" ||
		result.terminationSignal !== undefined
	);
}
