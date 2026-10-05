#!/usr/bin/env python3
"""Turn one strace process trace into a non-overlapping Bats case split."""

from __future__ import annotations

import argparse
import csv
import json
import os
import re
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path


LINE = re.compile(r"^(\d+)\s+(\d+(?:\.\d+)?)\s+(.*)$")
EXEC = re.compile(r'^execve\("((?:\\.|[^"\\])*)"')
CHILD = re.compile(r"\)\s+=\s+(\d+)\s+<")
DURATION = re.compile(r"<([0-9.]+)>$")
QUOTED = re.compile(r'"(?:\\.|[^"\\])*"')

HEADER = [
    "case_id",
    "source_file",
    "family",
    "status",
    "wall_s",
    "user_s",
    "sys_s",
    "bucket_harness_s",
    "bucket_production_s",
    "bucket_wait_s",
    "bucket_residual_s",
    "diagnostic_production_wait_s",
    "diagnostic_sleep_s",
    "diagnostic_timeout_s",
    "diagnostic_git_s",
    "diagnostic_jq_s",
    "diagnostic_tmux_s",
    "diagnostic_shell_script_s",
]


@dataclass(frozen=True)
class Segment:
    pid: int
    start: float
    end: float
    path: str
    call: str


def union(intervals: list[tuple[float, float]]) -> list[tuple[float, float]]:
    merged: list[list[float]] = []
    for start, end in sorted(intervals):
        if end <= start:
            continue
        if merged and start <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], end)
        else:
            merged.append([start, end])
    return [(start, end) for start, end in merged]


def intersection(
    left: list[tuple[float, float]], right: list[tuple[float, float]]
) -> list[tuple[float, float]]:
    result: list[tuple[float, float]] = []
    i = j = 0
    left = union(left)
    right = union(right)
    while i < len(left) and j < len(right):
        start = max(left[i][0], right[j][0])
        end = min(left[i][1], right[j][1])
        if start < end:
            result.append((start, end))
        if left[i][1] < right[j][1]:
            i += 1
        else:
            j += 1
    return result


def length(intervals: list[tuple[float, float]]) -> float:
    return sum(end - start for start, end in union(intervals))


def clipped(
    intervals: list[tuple[float, float]], bounds: tuple[float, float]
) -> list[tuple[float, float]]:
    return intersection(intervals, [bounds])


def basename(path: str) -> str:
    return path.rstrip("/").rsplit("/", 1)[-1]


def production_path(candidate: str, repo_root: str) -> bool:
    if not candidate.startswith("/"):
        return False
    normalized = os.path.normpath(candidate)
    try:
        relative = os.path.relpath(normalized, repo_root)
    except ValueError:
        return False
    if relative == ".." or relative.startswith("../"):
        return False
    if relative.startswith("adapters/core/") or relative.startswith("dash/"):
        return True
    if relative.startswith("scripts/"):
        script = relative.removeprefix("scripts/").split("/", 1)[0]
        return not script.startswith("harness-")
    return False


def is_production_call(call: str, repo_root: str) -> bool:
    arguments: list[str] = []
    for token in QUOTED.findall(call):
        try:
            candidate = json.loads(token)
        except json.JSONDecodeError:
            continue
        if isinstance(candidate, str):
            arguments.append(candidate)
    if not arguments:
        return False
    if production_path(arguments[0], repo_root):
        return True
    executor = basename(arguments[0])
    interpreter = executor in {"bash", "sh", "dash", "zsh", "env", "timeout", "gtimeout"}
    interpreter = interpreter or executor.startswith("python")
    return interpreter and any(production_path(argument, repo_root) for argument in arguments[1:])


def parse_trace(path: Path) -> tuple[list[Segment], dict[int, int], int, float, float]:
    parents: dict[int, int] = {}
    active: dict[int, tuple[float, str, str]] = {}
    segments: list[Segment] = []
    root_pid: int | None = None
    first = float("inf")
    last = float("-inf")

    def close(pid: int, timestamp: float) -> None:
        current = active.pop(pid, None)
        if current is not None:
            start, command, call = current
            segments.append(Segment(pid, start, timestamp, command, call))

    for raw_line in path.read_text().splitlines():
        match = LINE.match(raw_line)
        if not match:
            continue
        pid = int(match.group(1))
        timestamp = float(match.group(2))
        call = match.group(3)
        first = min(first, timestamp)
        duration = DURATION.search(call)
        last = max(last, timestamp + (float(duration.group(1)) if duration else 0.0))

        starts_process = call.startswith(("clone(", "clone3(", "fork(", "vfork("))
        resumes_process = call.startswith(
            ("<... clone resumed>", "<... clone3 resumed>", "<... fork resumed>", "<... vfork resumed>")
        )
        if (starts_process or resumes_process) and " = -1 " not in call:
            child = CHILD.search(call)
            if child:
                child_pid = int(child.group(1))
                parents[child_pid] = pid
                if pid in active:
                    _, command, parent_call = active[pid]
                    active[child_pid] = (timestamp, command, parent_call)

        executable = EXEC.match(call)
        if executable and " = 0 " in call:
            command = bytes(executable.group(1), "utf-8").decode("unicode_escape")
            close(pid, timestamp)
            active[pid] = (timestamp, command, call)
            if basename(command) == "bats-exec-test":
                if root_pid is not None and root_pid != pid:
                    raise ValueError("trace contains more than one bats-exec-test root")
                root_pid = pid
        elif "+++ exited with " in call or "+++ killed by " in call:
            close(pid, timestamp)

    for pid in list(active):
        close(pid, last)
    if root_pid is None:
        raise ValueError("trace has no successful bats-exec-test exec")
    return segments, parents, root_pid, first, last


def analyze(trace: Path, wall: float, repo_root: str) -> dict[str, float]:
    segments, parents, root_pid, trace_start, _ = parse_trace(trace)
    wall_end = trace_start + wall
    bounds = (trace_start, wall_end)

    descendants = {root_pid}
    changed = True
    while changed:
        changed = False
        for child, parent in parents.items():
            if parent in descendants and child not in descendants:
                descendants.add(child)
                changed = True

    case_segments = [segment for segment in segments if segment.pid in descendants]
    production_roots = {
        segment.pid
        for segment in case_segments
        if is_production_call(segment.call, repo_root)
    }
    production_pids = set(production_roots)
    production_start = {
        pid: min(
            segment.start
            for segment in case_segments
            if segment.pid == pid
            and is_production_call(segment.call, repo_root)
        )
        for pid in production_roots
    }
    changed = True
    while changed:
        changed = False
        for child, parent in parents.items():
            if parent in production_pids and child in descendants and child not in production_pids:
                production_pids.add(child)
                production_start[child] = production_start[parent]
                changed = True

    root_segments = [
        (segment.start, segment.end) for segment in case_segments if segment.pid == root_pid
    ]
    root_bounds = (
        min(start for start, _ in root_segments),
        max(end for _, end in root_segments),
    )
    production = clipped(
        [
            (max(segment.start, production_start[segment.pid]), segment.end)
            for segment in case_segments
            if segment.pid in production_pids and segment.end > production_start[segment.pid]
        ],
        bounds,
    )

    wait: list[tuple[float, float]] = []
    tools: dict[str, list[tuple[float, float]]] = defaultdict(list)
    for segment in case_segments:
        name = basename(segment.path)
        interval = (segment.start, segment.end)
        if name == "sleep":
            wait.append(interval)
            tools["sleep"].append(interval)
        if name in {"timeout", "gtimeout"}:
            tools["timeout"].append(interval)
        if name in {"git", "jq", "tmux"}:
            tools[name].append(interval)
        if re.search(r'\.sh(?:"|\\")', segment.call):
            tools["shell_script"].append(interval)

    for raw_line in trace.read_text().splitlines():
        match = LINE.match(raw_line)
        if not match:
            continue
        pid = int(match.group(1))
        if pid not in descendants:
            continue
        timestamp = float(match.group(2))
        call = match.group(3)
        if call.startswith(("nanosleep(", "clock_nanosleep(")):
            duration = DURATION.search(call)
            if duration:
                wait.append((timestamp, timestamp + float(duration.group(1))))

    wait = clipped(wait, bounds)
    root = clipped([root_bounds], bounds)
    production_wait = intersection(
        wait,
        production,
    )
    wait_s = length(wait)
    production_s = length(production) - length(intersection(production, wait))
    harness_s = length(root) - length(intersection(root, union(production + wait)))
    residual_s = max(0.0, wall - wait_s - production_s - harness_s)

    result = {
        "bucket_harness_s": harness_s,
        "bucket_production_s": production_s,
        "bucket_wait_s": wait_s,
        "bucket_residual_s": residual_s,
        "diagnostic_production_wait_s": length(production_wait),
    }
    for tool in ("sleep", "timeout", "git", "jq", "tmux", "shell_script"):
        result[f"diagnostic_{tool}_s"] = length(clipped(tools[tool], bounds))
    return result


def summarize(path: Path) -> None:
    rows = list(csv.DictReader(path.open(), delimiter="\t"))
    numeric = HEADER[4:]
    writer = csv.writer(sys.stdout, delimiter="\t", lineterminator="\n")
    writer.writerow(["group_type", "group", "cases", *numeric])
    for group_type, key in (("file", "source_file"), ("family", "family")):
        groups: dict[str, list[dict[str, str]]] = defaultdict(list)
        for row in rows:
            groups[row[key]].append(row)
        for group, members in sorted(groups.items()):
            writer.writerow(
                [group_type, group, len(members)]
                + [f"{sum(float(row[column]) for row in members):.6f}" for column in numeric]
            )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--summary", type=Path)
    parser.add_argument("--trace", type=Path)
    parser.add_argument("--repo-root")
    parser.add_argument("--case-id")
    parser.add_argument("--source-file")
    parser.add_argument("--family")
    parser.add_argument("--status", type=int, default=0)
    parser.add_argument("--wall", type=float)
    parser.add_argument("--user", type=float, default=0.0)
    parser.add_argument("--sys", type=float, default=0.0)
    parser.add_argument("--header", action="store_true")
    args = parser.parse_args()
    if args.summary:
        summarize(args.summary)
        return
    required = (args.trace, args.repo_root, args.case_id, args.source_file, args.family, args.wall)
    if any(value is None for value in required):
        parser.error(
            "trace mode requires --trace, --repo-root, --case-id, --source-file, --family, and --wall"
        )
    result = analyze(args.trace, args.wall, os.path.normpath(args.repo_root))
    writer = csv.writer(sys.stdout, delimiter="\t", lineterminator="\n")
    if args.header:
        writer.writerow(HEADER)
    row = {
        "case_id": args.case_id,
        "source_file": args.source_file,
        "family": args.family,
        "status": args.status,
        "wall_s": args.wall,
        "user_s": args.user,
        "sys_s": args.sys,
        **result,
    }
    writer.writerow(
        [row[column] if column in HEADER[:4] else f"{float(row[column]):.6f}" for column in HEADER]
    )


if __name__ == "__main__":
    main()
