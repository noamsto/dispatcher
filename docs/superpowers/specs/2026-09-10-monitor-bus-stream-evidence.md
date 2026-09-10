# Monitor as the claude bus lane — measured behaviour

**Status:** evidence for #127
**Date:** 2026-09-10

Everything below was run against the real Claude Code `Monitor` tool in the
session that implemented #127. It is recorded because the design's central claim
— _a dead stream is detectable_ — is worth nothing asserted and everything
measured.

## P1 — one stdout line is one notification, delivered verbatim

```
Monitor(
  command: printf '%s\n' '{"cursor":1,"events":[{"a":1}]}'
           sleep 2
           printf '%s\n' '{"heartbeat":2}'
           sleep 60,
  persistent: false, timeout_ms: 20000)
```

Two separate notifications arrived, in order:

```
<event>{"cursor":1,"events":[{"a":1}]}</event>
<event>{"heartbeat":2}</event>
```

Each carried exactly one line, unparsed and untruncated. `crew watch` prints a
whole batch as one line, so `{"cursor":…,"events":[…]}` reaches the dispatcher
intact and the existing rule — handle the entire `events[]` in **one** turn —
survives with nothing added to enforce it.

## P2 — a monitor that dies silently still speaks

```
Monitor(command: sleep 3; exit 9, persistent: false, timeout_ms: 60000)
```

The command wrote **nothing** to stdout. The notification still arrived:

```
<task-id>bj3hihre0</task-id>
<status>failed</status>
<summary>Monitor "probe A: silent monitor that dies non-zero" script failed (exit 9)</summary>
```

This is the whole answer to Monitor's own "silence is not success" warning, and
it is the structural equivalent of the one-shot lane's G4 self-heal: the death of
the stream is itself the event that re-invokes the dispatcher, which re-arms. No
external supervisor, exactly as before.

## P3 — a monitor the harness stops also speaks

P1's command ended in `sleep 60` against a 20 s `timeout_ms`, so the harness
killed it. That produced a third notification:

```
<event>[Monitor timed out — re-arm if needed.]</event>
```

It is recorded because it closes the remaining "stopped, not crashed" case: the
harness does not stop a monitor quietly. Whether `persistent: true` — what the
claude lane actually arms — exempts a monitor from this path is measured
separately in P5; it is not assumed here.

## P5 — `persistent: true` ignores `timeout_ms`

```
Monitor(
  command: i=0; while [ "$i" -lt 6 ]; do sleep 10; i=$((i+1)); done
           printf '%s\n' '{"stream":"p5-command-finished"}',
  persistent: true, timeout_ms: 5000)
```

The tool's own acknowledgement read `persistent — runs until TaskStop or session
end`, with no mention of the 5 s deadline. The command then ran its full ~60 s,
emitted its line, and the stream ended normally:

```
<event>{"stream":"p5-command-finished"}</event>
<status>completed</status>
<summary>Monitor "P5: persistent monitor given a 5s timeout_ms" stream ended</summary>
```

So `persistent: true` genuinely exempts the monitor from `timeout_ms`, and P3's
kill path does not apply to the armed claude lane. "Arms once per session" rests
on this, and it is now measured rather than assumed.

## P6 — a live `crew stream`, all four signals

Run against a throwaway repo and bus, with short timings for legibility. All
output below is verbatim.

**A batch, verbatim from `watch`.** With `--park 2 --coalesce 1`, posting one
qualifying status produced exactly one line:

```
{"cursor":1789050640352,"events":[{"ts":1789050640352,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"blocked","detail":"needs a decision"}}]}
```

**A heartbeat per `--heartbeat` of accumulated quiet**, not per park — with
`--park 2 --heartbeat 2`:

```
{"stream":"heartbeat","crew":"c1","quiet_s":2,"ts":1789050643433}
{"stream":"heartbeat","crew":"c1","quiet_s":2,"ts":1789050645485}
```

**An error, emitted once and then suppressed.** A live holder was planted in the
crew's `watch.lock.d`, so every inner watch fails identically. Over 5 s at
`--retry 1` — five failures — the stream emitted **one** line:

```
{"stream":"error","crew":"c1","rc":1,"detail":"crew: another watch is already running for this crew (c1)","ts":1789050815199}
```

and kept turning: `stream.tick` advanced across the next 2 s, and
`crew stream --status` reported `{"stream":"status","state":"alive",…}` at exit 0
throughout. A persistent fault is therefore visible once, not a firehose, and it
does not kill the loop.

**A TERM leaves nothing behind.** With `--park 20`, the stream's own inner watch
was identified by parentage (`ps --ppid`), then the stream was TERM'd:

```
stream=3142339  its inner watch=3142351
inner watch reaped
watch.lock.d released
stream.lock.d released
```

**Nothing on the stream's own stderr** in any of these runs — the inner watch's
per-expiry stderr line stays in the stream's `$errf`, so the JSON event stream is
never polluted.

One methodological note worth recording, because it cost time: an earlier probe
left a `crew stream` running, and its fresh inner watch every couple of seconds
matched a later `pgrep`, which read as an orphaned child of the run under test.
Parentage (`ps --ppid`), not a command-line pattern, is what actually identifies
a stream's own child.

## What was _not_ verified

- **Rate-limit auto-stop.** The tool contract states a monitor producing too many
  events is stopped automatically. This was not reproduced. The design avoids the
  trigger rather than relying on knowing its threshold: one line per _batch_
  (never per event), a coalescing window that caps the batch-line rate, and a
  separate heartbeat interval that caps the quiet-bus line rate.
- **cursor and codex streaming equivalents.** `cursor-agent` 2026.09.08 exposes
  no streaming/monitor flag on its CLI, but a CLI surface says nothing about an
  agent's in-session tool registry, and that registry is not enumerable from
  outside. `codex` is not installed on the machine where this was written, so
  nothing about it was checked at all. No claim is made either way: the protocol
  keeps describing cursor and codex as it already did, and #127 changes neither
  lane.
