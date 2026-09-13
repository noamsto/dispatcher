import { test } from "node:test";
import assert from "node:assert/strict";
import {
  statusArgs,
  msgArgs,
  inboxArgs,
  rosterArgs,
  heartbeatBody,
  createThrottle,
  parseHeartbeatSeconds,
} from "../bus.ts";

test("statusArgs: state only", () => {
  assert.deepEqual(statusArgs("worker:x#s1", "working"), [
    "status",
    "worker:x#s1",
    "working",
  ]);
});

test("statusArgs: state + detail", () => {
  assert.deepEqual(statusArgs("worker:x#s1", "blocked", "waiting on ci"), [
    "status",
    "worker:x#s1",
    "blocked",
    "waiting on ci",
  ]);
});

test("statusArgs: state + pr with no detail fills an empty detail slot", () => {
  assert.deepEqual(
    statusArgs("worker:x#s1", "pr_open", undefined, "https://example/pr/1"),
    ["status", "worker:x#s1", "pr_open", "", "https://example/pr/1"],
  );
});

test("statusArgs: state + detail + pr", () => {
  assert.deepEqual(
    statusArgs("worker:x#s1", "pr_open", "opened", "https://example/pr/1"),
    ["status", "worker:x#s1", "pr_open", "opened", "https://example/pr/1"],
  );
});

test("msgArgs", () => {
  assert.deepEqual(msgArgs("worker:x#s1", "heartbeat:c1", "body text"), [
    "msg",
    "worker:x#s1",
    "heartbeat:c1",
    "body text",
  ]);
});

test("inboxArgs: no since", () => {
  assert.deepEqual(inboxArgs("worker:x#s1"), ["inbox", "worker:x#s1"]);
});

test("inboxArgs: with since", () => {
  assert.deepEqual(inboxArgs("worker:x#s1", 12345), [
    "inbox",
    "worker:x#s1",
    "--since",
    "12345",
  ]);
});

test("rosterArgs", () => {
  assert.deepEqual(rosterArgs(), ["roster"]);
});

test("heartbeatBody: compact JSON with event and ts", () => {
  assert.equal(
    heartbeatBody("turn_start", 42),
    '{"event":"turn_start","ts":42}',
  );
});

test("throttle: first post for any event is true", () => {
  const throttle = createThrottle(1000, () => 0);
  assert.equal(throttle.shouldPost("turn_start"), true);
});

test("throttle: suppressed within interval", () => {
  let now = 0;
  const throttle = createThrottle(1000, () => now);
  assert.equal(throttle.shouldPost("turn_start"), true);
  now = 500;
  assert.equal(throttle.shouldPost("turn_start"), false);
});

test("throttle: true again after interval elapses", () => {
  let now = 0;
  const throttle = createThrottle(1000, () => now);
  assert.equal(throttle.shouldPost("turn_start"), true);
  now = 1000;
  assert.equal(throttle.shouldPost("turn_start"), true);
});

test("throttle: agent_settled always bypasses the interval", () => {
  let now = 0;
  const throttle = createThrottle(1000, () => now);
  assert.equal(throttle.shouldPost("turn_start"), true);
  now = 1;
  assert.equal(throttle.shouldPost("agent_settled"), true);
});

test("throttle: agent_settled records its own post time for later throttling", () => {
  let now = 0;
  const throttle = createThrottle(1000, () => now);
  assert.equal(throttle.shouldPost("agent_settled"), true);
  now = 500;
  assert.equal(throttle.shouldPost("turn_start"), false);
});

test("parseHeartbeatSeconds: unset falls back to 60", () => {
  assert.equal(parseHeartbeatSeconds(undefined), 60);
});

test("parseHeartbeatSeconds: empty string falls back to 60", () => {
  assert.equal(parseHeartbeatSeconds(""), 60);
});

test("parseHeartbeatSeconds: non-numeric falls back to 60", () => {
  assert.equal(parseHeartbeatSeconds("abc"), 60);
});

test("parseHeartbeatSeconds: negative falls back to 60", () => {
  assert.equal(parseHeartbeatSeconds("-5"), 60);
});

test("parseHeartbeatSeconds: explicit 0 is honoured (no throttle)", () => {
  assert.equal(parseHeartbeatSeconds("0"), 0);
});

test("parseHeartbeatSeconds: valid value is used as-is", () => {
  assert.equal(parseHeartbeatSeconds("30"), 30);
});
