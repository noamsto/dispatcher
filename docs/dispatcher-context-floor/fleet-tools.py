#!/usr/bin/env python3
"""Tool use in dispatcher sessions (#1271, Q2 and Q5).

Usage: python3 docs/dispatcher-context-floor/fleet-tools.py [--since DAYS]

Loads transcripts with scripts/context-budget.py's own loader and classifier,
keeps the sessions it classes as `dispatcher`, and prints:

1. tool calls by name (MCP tools folded to their server), and
2. context-budget.py's `tools` table (tool-result carry by class), run on the
   dispatcher sessions instead of worker leads.

Read-only. Prints names, counts and aggregates, never transcript text.
"""

import argparse
import collections
import importlib.util
import json
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("context_budget", root / "scripts/context-budget.py")
cb = importlib.util.module_from_spec(spec)
sys.modules["context_budget"] = cb  # multiprocessing pickles cb.parse_file by module name
spec.loader.exec_module(cb)

ap = argparse.ArgumentParser()
ap.add_argument("--since", type=float, default=7)
opts = ap.parse_args()

ns = argparse.Namespace(since=opts.since, root="~/.claude/projects", ratio=2.7, json=False,
                        reorient=10000, restart_every="50,100")
sessions, meta = cb.load(ns)
disp = [s for s in sessions if s["class"] == "dispatcher"]

calls = collections.Counter()
for s in disp:
    # A tool_use block repeats across transcript lines; count each id once, as
    # context-budget.py's parse_file does.
    seen = set()
    with open(s["path"], encoding="utf-8", errors="replace") as f:
        for line in f:
            if '"tool_use"' not in line:
                continue
            try:
                o = json.loads(line)
            except ValueError:
                continue
            for b in (o.get("message") or {}).get("content") or []:
                if isinstance(b, dict) and b.get("type") == "tool_use":
                    tid = b.get("id")
                    if tid in seen:
                        continue
                    seen.add(tid)
                    n = b.get("name", "")
                    calls["__".join(n.split("__")[:2]) if n.startswith("mcp__") else n] += 1

print(f"dispatcher sessions: {len(disp)} (window: {opts.since:g} days)")
print()
print("tool calls by name")
for name, n in calls.most_common():
    print(f"{n:7d}  {name}")
print()
print("tool-result carry by class (context-budget.py `tools`, dispatcher sessions;")
print("the 'worker-lead' labels below mean dispatcher; shares are relative only)")
sys.stdout.flush()

# Re-run the script's own `tools` report with dispatchers in the worker-lead slot.
orig_load = cb.load


def load_dispatchers(args):
    ss, m = orig_load(args)
    for s in ss:
        if s["class"] == "worker-lead":
            s["class"] = "x-worker-lead"
        elif s["class"] == "dispatcher":
            s["class"] = "worker-lead"
    return ss, m


cb.load = load_dispatchers
sys.argv = ["context-budget.py", "--since", str(opts.since), "tools"]
cb.main()
