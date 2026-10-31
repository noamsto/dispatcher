#!/usr/bin/env python3
# Stitched worker runs in `context-budget.py growth`: a synthetic transcript root and bus log
# with one worker relaunched fresh at the execute and review seams, plus an unmapped session.
#
#   python3 -I tests/context-budget-stitch.py
import json
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "context-budget.py"

HB = 'crew status "$CREW_WORKER_ID" working "{}"'
PLAN = [(20_000, [HB.format("plan")]), (25_000, ["ls"]), (30_000, ["ls"]), (35_000, ["ls"])]
EXEC = [(12_000, [HB.format("execute")]), (18_000, ["ls"]), (24_000, ["ls"])]
REVIEW = [(14_000, [HB.format("review")]), (18_000, ["gh pr create --assignee @me"]), (22_000, ["ls"])]
SOLO = [(9_000, [HB.format("execute")]), (15_000, ["gh pr create --assignee @me"]), (21_000, ["ls"])]


def write_bus(repo, rows):
    ev = repo / ".git" / "crew" / "events.jsonl"
    ev.parent.mkdir(parents=True)
    ev.write_text("".join(json.dumps(r, separators=(",", ":")) + "\n" for r in rows))


def write_session(projects, repo, sid, ts, steps):
    cwd = str(repo)
    recs = [{"type": "user", "cwd": cwd, "timestamp": ts,
             "message": {"role": "user", "content": "Read WORKER_TASK.md and run it end-to-end."}}]
    for i, (ctx, cmds) in enumerate(steps):
        content = [{"type": "tool_use", "id": f"{sid}-tu{i}-{j}", "name": "Bash", "input": {"command": c}}
                   for j, c in enumerate(cmds)]
        recs.append({"type": "assistant", "cwd": cwd, "timestamp": ts,
                     "message": {"id": f"{sid}-msg{i}", "usage": {"input_tokens": ctx}, "content": content}})
    projects.mkdir(parents=True, exist_ok=True)
    (projects / f"{sid}.jsonl").write_text("".join(json.dumps(r, separators=(",", ":")) + "\n" for r in recs))


def build(tmp, solo=False):
    """-> transcript root. The three relaunched sessions share one (crew_id, worker_id)."""
    root = Path(tmp)
    repo = root / "repo"
    projects = root / "projects"
    worker = {"crew_id": "1-2", "worker_id": "worker:feat/x#s1"}
    write_bus(repo, [
        {"kind": "dispatch", "engine_session": "b-plan", **worker},
        {"kind": "relaunch", "stage": "execute", "engine_session": "a-exec", **worker},
        {"kind": "relaunch", "stage": "review", "engine_session": "c-review", **worker},
    ])
    # Path order (a-exec, b-plan, c-review) differs from time order: the stitch must sort by time.
    write_session(projects, repo, "b-plan", "2026-10-30T10:00:00Z", PLAN)
    write_session(projects, repo, "a-exec", "2026-10-30T10:30:00Z", EXEC)
    write_session(projects, repo, "c-review", "2026-10-30T11:00:00Z", REVIEW)
    if solo:
        write_session(projects, repo, "d-solo", "2026-10-30T12:00:00Z", SOLO)
    return projects


def growth(projects):
    out = subprocess.run([sys.executable, "-I", str(SCRIPT), "growth", "--root", str(projects), "--json"],
                         capture_output=True, text=True, check=True)
    return json.loads(out.stdout)


def test_three_sessions_stitch_into_one_run():
    with tempfile.TemporaryDirectory() as tmp:
        obj = growth(build(tmp))
    s = obj["summary"]
    assert s["n_sessions"] == 3, s
    assert s["n_stitched"] == 1, s
    assert s["n_total"] == 1, s
    assert s["n_completed"] == 1, s
    assert s["turns_median"] == 4 + 3 + 3, s
    phases = {k: v["turns_median"] for k, v in obj["phases"].items()}
    assert phases == {"start": 1, "plan": 4, "execute": 3, "review": 1, "pr": 1}, phases
    # No delta may span a relaunch: plan grew 25k->35k inside its own session.
    deltas = {k: v["delta_median"] for k, v in obj["phases"].items()}
    assert deltas == {"start": 5_000, "plan": 16_000, "execute": 10_000, "review": 4_000, "pr": 0}, deltas


def test_unmapped_session_is_its_own_run():
    with tempfile.TemporaryDirectory() as tmp:
        s = growth(build(tmp, solo=True))["summary"]
    assert s["n_sessions"] == 4, s
    assert s["n_total"] == 2, s
    assert s["n_stitched"] == 1, s
    assert s["n_completed"] == 2, s


def main():
    tests = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    failed = 0
    for t in tests:
        try:
            t()
        except AssertionError as e:
            failed += 1
            print(f"FAIL {t.__name__}: {e}")
        else:
            print(f"ok   {t.__name__}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
