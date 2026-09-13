export function statusArgs(
  id: string,
  state: string,
  detail?: string,
  pr?: string,
): string[] {
  const args = ["status", id, state];
  // crew.sh reads `status <from> <state> [detail] [pr]` positionally, so a pr
  // without a detail still needs the empty detail slot.
  if (pr !== undefined) {
    args.push(detail ?? "", pr);
  } else if (detail !== undefined) {
    args.push(detail);
  }
  return args;
}

export function msgArgs(from: string, to: string, body: string): string[] {
  return ["msg", from, to, body];
}

export function inboxArgs(id: string, since?: number): string[] {
  return since === undefined
    ? ["inbox", id]
    : ["inbox", id, "--since", String(since)];
}

export function rosterArgs(): string[] {
  return ["roster"];
}

export function heartbeatBody(event: string, nowMs: number): string {
  return JSON.stringify({ event, ts: nowMs });
}

export function parseHeartbeatSeconds(raw: string | undefined): number {
  if (raw === undefined || raw.trim() === "") return 60;
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? n : 60;
}

export function createThrottle(
  intervalMs: number,
  clock: () => number,
): { shouldPost(event: string): boolean } {
  let lastPostMs: number | undefined;
  return {
    shouldPost(event: string): boolean {
      const now = clock();
      // agent_settled marks the worker going idle, which a stall check must see.
      if (event === "agent_settled") {
        lastPostMs = now;
        return true;
      }
      if (lastPostMs === undefined || now - lastPostMs >= intervalMs) {
        lastPostMs = now;
        return true;
      }
      return false;
    },
  };
}
