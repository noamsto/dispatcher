#!/usr/bin/env python3
# context-budget — read-only context-budget report across Claude Code transcripts.
#
# Where does a session's context (and therefore its carried-forward token cost)
# go? Classifies sessions (worker-lead, role-pane, subagent, dispatcher, ...),
# and breaks worker-lead context growth down by phase and by tool-result class.
# Transcripts may contain secrets: this classifies text in memory only and
# prints aggregates (counts, token sums, percentiles, sizes, class names).
#
# Read-only: never writes a transcript or touches the crew bus.
#
#   context-budget.py fleet                  per-class share of context integral
#   context-budget.py growth                 worker-lead context growth per phase
#   context-budget.py tools                  worker-lead tool-result carry cost
#   context-budget.py sections FILE          markdown ##/### section sizes
#   context-budget.py dispatcher             dispatcher wake/idle context cost
#   (global) --since DAYS  --json  --ratio R  --root DIR
import argparse
import json
import math
import os
import re
import statistics
import time
import sys
import multiprocessing
from pathlib import Path

DISPATCHER_BASH = re.compile(r"(^|[;&|\n]\s*)(dispatch\s|crew (watch|roster|stream)\b)")
IDLE_BASH = re.compile(r"(^|\s)dispatch\s|gh pr (merge|create|edit|comment)|crew (msg|reply|release)")
IDLE_TOOLS = {"Agent", "Task", "Edit", "Write", "SendMessage"}
WAKE_ORIGINS = {"task-notification", "peer", "coordinator"}
NON_PROMPT_PREFIX = ("<system-reminder", "<local-command", "<bash-", "<task-notification")
CMD_CAP = 6000

PHASES = ["start", "spec", "plan", "execute", "gate", "review", "pr"]
CLASSES = ["worker-lead", "role-pane", "subagent", "interactive-worktree", "dispatcher", "interactive"]
LONG_CTX = 150_000

STAGE_RE = re.compile(r"""crew status[^\n]*?\sworking[ \t]+(?:"([^"]*)"|'([^']*)'|(?!\d*[<>])([^\s;&|<>"'][^;&|<>\n]*))""")
SEAM_RE = re.compile(r"""seam\\?"\s*:\s*\\?"(review|deslop)""")


# ---------------------------------------------------------------- parsing

def text_of(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(b.get("text", "") for b in content
                         if isinstance(b, dict) and b.get("type") == "text")
    return ""


def result_size(content):
    if isinstance(content, str):
        return len(content), 0
    chars = images = 0
    if isinstance(content, list):
        for b in content:
            if not isinstance(b, dict):
                continue
            if b.get("type") == "text":
                chars += len(b.get("text", ""))
            elif b.get("type") == "image":
                images += 1
    return chars, images


def tool_arg(name, inp):
    if not isinstance(inp, dict):
        return ""
    if name == "Bash":
        return str(inp.get("command", ""))[:CMD_CAP]
    if name == "Read":
        return str(inp.get("file_path", ""))
    if name in ("Agent", "Task"):
        return str(inp.get("subagent_type", ""))
    if name == "Skill":
        return str(inp.get("skill", ""))
    return ""


def parse_file(path):
    """One transcript -> compact session dict (no message text retained)."""
    turns = []      # context tokens per deduped assistant turn
    uses = []       # per turn: [(tool_use_id, name, arg)]
    seen = {}       # message.id -> turn index
    marks = []      # (turn_count_at_event, 'H'|'W', subkind)
    results = []    # (turn_count_at_event, tool_use_id, chars, images)
    tin = []        # (turn_idx, name, arg, input_chars) per tool_use
    asst = []       # (turn_idx, 'text'|'thinking', chars)
    seen_blocks = set()
    cwds = set()
    first_prompt = None
    p_role = p_worker = False
    dispatcher_flag = False
    try:
        fh = open(path, "r", encoding="utf-8", errors="replace")
    except OSError:
        return None
    with fh:
        for line in fh:
            if '"type":"assistant"' not in line and '"type":"user"' not in line:
                continue
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            typ = rec.get("type")
            cwd = rec.get("cwd")
            if cwd:
                cwds.add(cwd)
            msg = rec.get("message")
            if not isinstance(msg, dict):
                continue
            if typ == "assistant":
                usage = msg.get("usage")
                if not isinstance(usage, dict):
                    continue
                mid = msg.get("id") or rec.get("uuid")
                idx = seen.get(mid)
                if idx is None:
                    ctx = ((usage.get("input_tokens") or 0)
                           + (usage.get("cache_read_input_tokens") or 0)
                           + (usage.get("cache_creation_input_tokens") or 0))
                    if ctx <= 0:
                        continue
                    idx = len(turns)
                    seen[mid] = idx
                    turns.append(ctx)
                    uses.append([])
                content = msg.get("content")
                if isinstance(content, list):
                    abi = rec.get("apiBlockIndex")
                    for bi, b in enumerate(content):
                        if not isinstance(b, dict):
                            continue
                        btype = b.get("type")
                        if btype in ("text", "thinking"):
                            key = (mid, abi, bi, btype)
                            if abi is not None and key in seen_blocks:
                                continue
                            seen_blocks.add(key)
                            asst.append((idx, btype, len(b.get(btype) or "")))
                            continue
                        if btype != "tool_use":
                            continue
                        tid = b.get("id", "")
                        if ("tu", tid) in seen_blocks:
                            continue
                        seen_blocks.add(("tu", tid))
                        name = b.get("name", "")
                        inp = b.get("input")
                        arg = tool_arg(name, inp)
                        uses[idx].append((tid, name, arg))
                        tin.append((idx, name, arg, len(json.dumps(inp, ensure_ascii=False))))
                        if not dispatcher_flag and (
                                (name == "Bash" and DISPATCHER_BASH.search(arg))
                                or (name == "Skill" and arg == "dispatcher:dispatcher")):
                            dispatcher_flag = True
            elif typ == "user":
                content = msg.get("content")
                has_result = False
                if isinstance(content, list):
                    for b in content:
                        if isinstance(b, dict) and b.get("type") == "tool_result":
                            has_result = True
                            chars, images = result_size(b.get("content"))
                            results.append((len(turns), b.get("tool_use_id", ""), chars, images))
                if has_result:
                    continue
                origin = rec.get("origin")
                okind = origin.get("kind") if isinstance(origin, dict) else None
                text = text_of(content)
                if okind in WAKE_ORIGINS:
                    marks.append((len(turns), "W", okind))
                    continue
                if "<task-notification>" in text or text.startswith("<monitor"):
                    marks.append((len(turns), "W", "tag"))
                    continue
                if rec.get("isMeta"):
                    continue
                marks.append((len(turns), "H", ""))
                if first_prompt is None and text and not text.startswith(NON_PROMPT_PREFIX):
                    first_prompt = True
                    p_role = "role pane in this task grid" in text or "park for an assignment" in text
                    p_worker = "WORKER_TASK.md" in text
    if not turns:
        return None
    p = Path(path)
    return {
        "path": str(p), "stem": p.stem, "turns": turns, "uses": uses,
        "marks": marks, "results": results, "tin": tin, "asst": asst, "cwds": sorted(cwds),
        "p_role": p_role, "p_worker": p_worker, "dispatcher_flag": dispatcher_flag,
        "subagent": "subagents" in p.parts,
        "worktree": "--worktrees-" in p.parent.name,
    }


# ------------------------------------------------------------- bus events

def main_git_dir(cwd):
    d = Path(cwd)
    if not d.exists():
        # removed worktree: <base>/.worktrees/<host>/<repo>/<branch> -> ~/<host>/<repo>
        parts = d.parts
        if ".worktrees" not in parts:
            return None
        i = parts.index(".worktrees")
        if len(parts) <= i + 2:
            return None
        cand = Path.home() / parts[i + 1] / parts[i + 2] / ".git"
        return cand if cand.is_dir() else None
    for anc in [d, *d.parents]:
        g = anc / ".git"
        if g.is_dir():
            return g
        if g.is_file():
            try:
                first = g.read_text(errors="replace").strip()
            except OSError:
                return None
            if not first.startswith("gitdir:"):
                return None
            gd = Path(first[len("gitdir:"):].strip())
            return Path(*gd.parts[:gd.parts.index("worktrees")]) if "worktrees" in gd.parts else gd
    return None


def bus_engine_sessions(sessions):
    """engine_session ids of bus `dispatch` events; returns (set, n_logs, n_cwds, n_cwds_resolved)."""
    cwds = {c for s in sessions for c in s["cwds"]}
    gits = set()
    resolved = 0
    for c in cwds:
        g = main_git_dir(c)
        if g is not None:
            resolved += 1
            gits.add(g)
    ids = set()
    logs = 0
    for g in sorted(gits):
        ev = g / "crew" / "events.jsonl"
        if not ev.is_file():
            continue
        logs += 1
        with open(ev, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                if '"dispatch"' not in line:
                    continue
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                if e.get("kind") == "dispatch" and e.get("engine_session"):
                    ids.add(str(e["engine_session"]))
    return ids, logs, len(cwds), resolved


def classify(s, engine_ids):
    """-> (class, worker_route) ; worker_route in engine|prompt|both|None."""
    if s["subagent"]:
        return "subagent", None
    by_engine = s["stem"] in engine_ids
    if s["p_role"] and not by_engine:
        return "role-pane", None
    if by_engine or s["p_worker"]:
        return "worker-lead", ("both" if by_engine and s["p_worker"] else "engine" if by_engine else "prompt")
    if s["worktree"]:
        return "interactive-worktree", None
    return ("dispatcher" if s["dispatcher_flag"] else "interactive"), None


# ----------------------------------------------------------------- loading

def load(args):
    root = Path(os.path.expanduser(args.root))
    cutoff = time.time() - args.since * 86400
    files = []
    for dirpath, _, names in os.walk(root):
        for n in names:
            if n.endswith(".jsonl"):
                p = os.path.join(dirpath, n)
                try:
                    if os.stat(p).st_mtime >= cutoff:
                        files.append(p)
                except OSError:
                    pass
    files.sort()
    with multiprocessing.get_context("fork").Pool(min(16, os.cpu_count() or 1)) as pool:
        sessions = [s for s in pool.imap_unordered(parse_file, files, chunksize=4) if s]
    sessions.sort(key=lambda s: s["path"])
    engine_ids, logs, n_cwds, n_res = bus_engine_sessions(sessions)
    for s in sessions:
        s["class"], s["route"] = classify(s, engine_ids)
    meta = {"files": len(files), "sessions": len(sessions), "bus_logs": logs,
            "cwds": n_cwds, "cwds_resolved": n_res, "engine_sessions": len(engine_ids)}
    return sessions, meta


# ----------------------------------------------------------------- helpers

def pct(vals, q):
    if not vals:
        return None
    v = sorted(vals)
    if len(v) == 1:
        return float(v[0])
    k = (len(v) - 1) * q
    lo = math.floor(k)
    hi = math.ceil(k)
    return v[lo] + (v[hi] - v[lo]) * (k - lo)


def med(vals):
    return statistics.median(vals) if vals else None


def fmt(x, nd=0):
    if x is None:
        return "-"
    if isinstance(x, float):
        return f"{x:,.{nd}f}"
    return f"{x:,}" if isinstance(x, int) else str(x)


def table(headers, rows):
    rows = [[str(c) for c in r] for r in rows]
    widths = [max(len(h), *(len(r[i]) for r in rows)) if rows else len(h) for i, h in enumerate(headers)]
    out = ["  ".join(h.ljust(widths[i]) if i == 0 else h.rjust(widths[i]) for i, h in enumerate(headers))]
    out.append("  ".join("-" * w for w in widths))
    for r in rows:
        out.append("  ".join(c.ljust(widths[i]) if i == 0 else c.rjust(widths[i]) for i, c in enumerate(r)))
    return "\n".join(out)


def emit(args, obj, text):
    if args.json:
        json.dump(obj, sys.stdout, indent=2)
        print()
    else:
        print(text)


def share(part, whole):
    return 100.0 * part / whole if whole else 0.0


def hdr(meta):
    return (f"transcripts={meta['files']} sessions(with turns)={meta['sessions']} "
            f"bus_logs={meta['bus_logs']} cwds={meta['cwds']} (resolved {meta['cwds_resolved']}) "
            f"bus_engine_sessions={meta['engine_sessions']}")


# ------------------------------------------------------------------- fleet

def cmd_fleet(args):
    sessions, meta = load(args)
    total = sum(sum(s["turns"]) for s in sessions)
    rows, out = [], {}
    for cls in CLASSES:
        ss = [s for s in sessions if s["class"] == cls]
        turns = [c for s in ss for c in s["turns"]]
        integral = sum(turns)
        firsts = [s["turns"][0] for s in ss]
        entry = {
            "sessions": len(ss), "turns": len(turns), "integral": integral,
            "integral_share_pct": share(integral, total),
            "turns_over_150k_pct": share(sum(1 for c in turns if c > LONG_CTX), len(turns)),
            "first_ctx_median": med(firsts), "first_ctx_p90": pct(firsts, 0.9),
            "max_ctx": max(turns) if turns else None,
        }
        out[cls] = entry
        rows.append([cls, fmt(entry["sessions"]), fmt(entry["turns"]), fmt(integral),
                     f"{entry['integral_share_pct']:.1f}", f"{entry['turns_over_150k_pct']:.1f}",
                     fmt(entry["first_ctx_median"]), fmt(entry["first_ctx_p90"]), fmt(entry["max_ctx"])])
    wl = [s for s in sessions if s["class"] == "worker-lead"]
    routes = {r: sum(1 for s in wl if s["route"] == r) for r in ("engine", "prompt", "both")}
    matched = {"by_engine_session": routes["engine"] + routes["both"],
               "by_prompt_only": routes["prompt"], "by_both": routes["both"], "total": len(wl)}
    obj = {"meta": meta, "total_integral": total, "classes": out, "worker_lead_match": matched}
    text = hdr(meta) + "\n\n" + table(
        ["class", "sessions", "turns", "integral", "share%", ">150k%", "first_med", "first_p90", "max_ctx"], rows)
    text += (f"\n\ntotal integral: {fmt(total)}\nworker-leads: {matched['total']} "
             f"(by engine_session: {matched['by_engine_session']}, by prompt only: {matched['by_prompt_only']}, "
             f"matched by both: {matched['by_both']})")
    emit(args, obj, text)


# ------------------------------------------------------------------ growth

STAGE_KEYWORDS = [
    ("pr", re.compile(r"\b(deslop|push|pr|pr_open)\b|pre-push")),
    ("review", re.compile(r"\breview")),
    ("gate", re.compile(r"\bgate\b")),
    ("execute", re.compile(r"\bexecut")),
    ("plan", re.compile(r"\bplan")),
    ("spec", re.compile(r"\bspec")),
]
CRITIC_RE = re.compile(r"\b(spec|plan)[- ]critic")
DONE_RE = re.compile(r"\b(done|green|accepted|passed|complete|approved)\b")


def stage_phase(stage):
    """Phase a heartbeat label puts the run in. A label naming only a stage that
    just finished ("fast gate green") means the next phase has begun."""
    s = stage.lower()
    if "post-review" in s:
        return "pr"
    m = CRITIC_RE.search(s)
    hits = [m.group(1)] if m else [p for p, rx in STAGE_KEYWORDS if rx.search(s)]
    if not hits:
        return None
    phase = hits[0]
    if len(hits) == 1 and phase != "pr" and DONE_RE.search(s):
        return PHASES[PHASES.index(phase) + 1]
    return phase


def lead_markers(s):
    """-> (markers [(turn_idx, phase)], used_fallback)."""
    heartbeat = []
    other = []
    fallback = []
    for t, us in enumerate(s["uses"]):
        for _, name, arg in us:
            if name == "Bash":
                if "crew status" in arg and "working" in arg:
                    m = STAGE_RE.search(arg)
                    if m:
                        heartbeat.append((t, stage_phase(next(g for g in m.groups() if g is not None))))
                if "crew msg" in arg:
                    m = SEAM_RE.search(arg)
                    if m:
                        other.append((t, "review" if m.group(1) == "review" else "pr"))
                if "gh pr create" in arg:
                    other.append((t, "pr"))
            elif name in ("Agent", "Task"):
                if "spec-critic" in arg:
                    fallback.append((t, "spec"))
                elif "plan-critic" in arg:
                    fallback.append((t, "plan"))
                elif arg.endswith("-reviewer"):
                    fallback.append((t, "review"))
            elif name == "Skill" and "deslop" in arg:
                fallback.append((t, "pr"))
    hb = [(t, p) for t, p in heartbeat if p]
    used_fallback = not hb
    markers = hb + other + (fallback if used_fallback else [])
    markers.sort(key=lambda x: x[0])
    return markers, used_fallback


def run_phases(s):
    """Per-turn phase array honouring 'effective from the next turn'."""
    markers, used_fallback = lead_markers(s)
    by_turn = {}
    for t, p in markers:
        by_turn[t] = p
    cur, phases = "start", []
    for t in range(len(s["turns"])):
        phases.append(cur)
        cur = by_turn.get(t, cur)
    return phases, used_fallback


def phase_stats(ctxs, phases):
    """Segment the run; per-phase turns, first/last ctx, delta, integral."""
    segs = []
    for t, p in enumerate(phases):
        if segs and segs[-1][0] == p:
            segs[-1][2] = t
        else:
            segs.append([p, t, t])
    per = {}
    for i, (p, a, b) in enumerate(segs):
        nxt = ctxs[segs[i + 1][1]] if i + 1 < len(segs) else ctxs[-1]
        d = per.setdefault(p, {"turns": 0, "first": ctxs[a], "last": ctxs[b], "delta": 0, "integral": 0})
        d["turns"] += b - a + 1
        d["last"] = ctxs[b]
        d["delta"] += nxt - ctxs[a]
        d["integral"] += sum(ctxs[a:b + 1])
    return per


def restart_saving(ctxs, reorient, seams):
    """Context integral saved if the session restarted fresh at each seam turn.

    Turn t >= seam runs at F + R + (ctx_t - ctx_seam) instead of ctx_t (so the seam
    turn itself runs at F + R), where F is the run's first-turn context and R the
    re-orientation cost. Per-turn saving is clamped to [0, ctx_t - F - R] so a
    context drop (compaction) never yields a negative or larger-than-the-turn saving.
    """
    seams = sorted(seams)
    if not seams:
        return 0
    first, saving, j, base = ctxs[0], 0, 0, 0
    for t in range(seams[0], len(ctxs)):
        while j < len(seams) and seams[j] <= t:
            base = ctxs[seams[j]]
            j += 1
        saving += max(0, min(base - first - reorient, ctxs[t] - first - reorient))
    return saving


def cmd_growth(args):
    sessions, meta = load(args)
    wl = [s for s in sessions if s["class"] == "worker-lead"]
    runs, n_fallback, n_fallback_completed = [], 0, 0
    for s in wl:
        phases, used_fb = run_phases(s)
        n_fallback += used_fb
        if "pr" not in phases:
            continue
        n_fallback_completed += used_fb
        ctxs = s["turns"]
        per = phase_stats(ctxs, phases)
        partition_ok = (sum(v["turns"] for v in per.values()) == len(ctxs)
                        and sum(v["integral"] for v in per.values()) == sum(ctxs))
        first_idx = {}
        for t, ph in enumerate(phases):
            first_idx.setdefault(ph, t)
        runs.append({"per": per, "ctxs": ctxs, "first_idx": first_idx, "first": ctxs[0], "peak": max(ctxs), "turns": len(ctxs),
                     "end": ctxs[-1], "partition_ok": partition_ok,
                     "integral": sum(ctxs)})
    total_int = sum(r["integral"] for r in runs)
    agg, rows = {}, []
    for p in PHASES:
        rs = [r["per"][p] for r in runs if p in r["per"]]
        if not rs:
            continue
        e = {"n": len(rs)}
        for k in ("turns", "delta", "integral"):
            vals = [r[k] for r in rs]
            e[k + "_median"] = med(vals)
            e[k + "_p90"] = pct(vals, 0.9)
        e["first_ctx_median"] = med([r["first"] for r in rs])
        e["integral_share_pct"] = share(sum(r["integral"] for r in rs), total_int)
        agg[p] = e
        rows.append([p, e["n"], fmt(e["turns_median"], 1), fmt(e["turns_p90"], 1),
                     fmt(e["delta_median"]), fmt(e["delta_p90"]),
                     fmt(e["integral_median"]), fmt(e["integral_p90"]),
                     f"{e['integral_share_pct']:.1f}", fmt(e["first_ctx_median"])])
    summary = {
        "n_total": len(wl), "n_completed": len(runs),
        "n_fallback_total": n_fallback, "n_fallback_completed": n_fallback_completed,
        "first_ctx_median": med([r["first"] for r in runs]),
        "peak_ctx_median": med([r["peak"] for r in runs]),
        "turns_median": med([r["turns"] for r in runs]),
        "end_ctx_median": med([r["end"] for r in runs]),
        "run_integral_median": med([r["integral"] for r in runs]),
        "phase_partition_ok": sum(1 for r in runs if r["partition_ok"]),
        "ctx_at_execute_start_median": med([r["per"]["execute"]["first"] for r in runs if "execute" in r["per"]]),
        "ctx_at_review_start_median": med([r["per"]["review"]["first"] for r in runs if "review" in r["per"]]),
    }
    total_turns = sum(r["turns"] for r in runs)
    before, before_rows = {}, []
    for p in PHASES:
        vals = [r["first_idx"].get(p, 0) for r in runs]
        n_never = sum(1 for r in runs if p not in r["first_idx"])
        before[p] = {"never": n_never, "median": med(vals), "mean": statistics.fmean(vals) if vals else None,
                     "total": sum(vals)}
        before_rows.append([p, n_never, fmt(before[p]["median"], 1), fmt(before[p]["mean"], 1),
                            fmt(before[p]["total"])])
    rev_idx = [r["first_idx"]["review"] for r in runs if "review" in r["first_idx"]]
    before_review = {"total_turns_before_review": sum(rev_idx), "runs_entering_review": len(rev_idx),
                     "total_turns": total_turns}

    restart, restart_rows = {}, []
    for name, seam_phases in (("plan", ["plan"]), ("execute", ["execute"]), ("review", ["review"]),
                              ("execute+review", ["execute", "review"])):
        sav = [restart_saving(r["ctxs"], args.reorient, [r["first_idx"][ph] for ph in seam_phases])
               for r in runs if all(ph in r["first_idx"] for ph in seam_phases)]
        restart[name] = {"n": len(sav), "median_saving": med(sav), "total_saving": sum(sav),
                         "saving_pct_of_integral": share(sum(sav), total_int)}
        restart_rows.append([name, len(sav), fmt(med(sav)), fmt(sum(sav)),
                             f"{restart[name]['saving_pct_of_integral']:.1f}"])
    summary["reorient"] = args.reorient
    obj = {"meta": meta, "summary": summary, "phases": agg, "turns_before_first_entry": before,
           "before_review": before_review, "restart_model": restart}
    text = hdr(meta) + "\n\n" + table(
        ["phase", "n", "turns_med", "turns_p90", "delta_med", "delta_p90", "integ_med", "integ_p90",
         "share%", "first_ctx_med"], rows)
    text += (f"\n\nworker-leads: {len(wl)} total, {len(runs)} completed (reached phase pr); "
             f"phase fallback used by {n_fallback} total ({n_fallback_completed} completed)\n"
             f"median first-turn ctx: {fmt(summary['first_ctx_median'])}   "
             f"median peak ctx: {fmt(summary['peak_ctx_median'])}   "
             f"median turns: {fmt(summary['turns_median'])}   "
             f"median end ctx: {fmt(summary['end_ctx_median'])}\n"
             f"median run integral: {fmt(summary['run_integral_median'])}\n"
             f"median ctx at start of execute: {fmt(summary['ctx_at_execute_start_median'])}   "
             f"at start of review: {fmt(summary['ctx_at_review_start_median'])}\n"
             f"phase partition check: {summary['phase_partition_ok']}/{len(runs)} runs")
    text += ("\n\nturns before first entering each phase (completed runs, 0 if never entered):\n"
             + table(["phase", "never", "median", "mean", "total"], before_rows)
             + f"\ntotal turns before first entering review: {fmt(before_review['total_turns_before_review'])} "
               f"(over {before_review['runs_entering_review']} runs entering review) of {fmt(total_turns)} total turns "
               f"in {len(runs)} completed runs")
    text += (f"\n\nrestart model (fresh session at the seam, re-orient R={args.reorient:,} tokens; "
             f"total completed-run integral {fmt(total_int)}):\n"
             + table(["seam", "n", "median_saving", "total_saving", "saving_%integral"], restart_rows))
    emit(args, obj, text)


# ------------------------------------------------------------------- tools

BASH_CLASSES = [
    ("bash:bats", re.compile(r"\bbats\b")),
    ("bash:flake-check", re.compile(r"nix flake check")),
    ("bash:nix-other", re.compile(r"nix (build|run|develop)\b")),
    ("bash:go-test", re.compile(r"go test|go vet|golangci")),
    ("bash:shellcheck", re.compile(r"shellcheck")),
    ("bash:git", re.compile(r"git (diff|show|log)\b")),
    ("bash:gh", re.compile(r"\bgh\s")),
    ("bash:crew", re.compile(r"\b(crew|dispatch)\s")),
    ("bash:search", re.compile(r"\b(rg|grep)\s")),
    ("bash:cat", re.compile(r"sed -n|\bcat\s|\bhead\b|\btail\b")),
]
GATE_CLASSES = ["bash:bats", "bash:flake-check", "bash:go-test", "bash:shellcheck", "bash:nix-other"]
CAP_CHARS = 2000


def tool_class(name, arg):
    if name == "Read":
        if arg.endswith("adapters/core/crew.sh"):
            return "read:crew.sh"
        if arg.endswith("adapters/core/dispatch.sh"):
            return "read:dispatch.sh"
        if arg.endswith(".bats"):
            return "read:bats"
        if arg.endswith(".md"):
            return "read:protocol-md" if "/protocols/" in arg else "read:md"
        return "read:other"
    if name == "Bash":
        for cls, rx in BASH_CLASSES:
            if rx.search(arg):
                return cls
        return "bash:other"
    if name in ("Agent", "Task"):
        return "agent"
    if name in ("Grep", "Glob"):
        return "search-tool"
    if name == "Skill":
        return "skill"
    if name in ("Edit", "Write"):
        return "edit"
    if name.startswith("mcp__"):
        return "other:mcp"
    return f"other:{name}" if name else "other:unknown"


def cmd_tools(args):
    sessions, meta = load(args)
    wl = [s for s in sessions if s["class"] == "worker-lead"]
    wl_integral = sum(sum(s["turns"]) for s in wl)
    r = args.ratio
    by = {}
    side = {}       # tool_use inputs ("in:<class>") and assistant text
    thinking = {"count": 0, "chars": 0, "carry": 0.0}
    floor = 0
    images = 0
    for s in wl:
        n = len(s["turns"])
        floor += s["turns"][0] * n
        for t, name, arg, chars in s["tin"]:
            e = side.setdefault("in:" + tool_class(name, arg), {"count": 0, "chars": 0, "carry": 0.0, "tok": []})
            e["count"] += 1
            e["chars"] += chars
            e["tok"].append(chars / r)
            e["carry"] += chars / r * (n - 1 - t)
        for t, kind, chars in s["asst"]:
            if kind == "thinking":
                thinking["count"] += 1
                thinking["chars"] += chars
                thinking["carry"] += chars / r * (n - 1 - t)
                continue
            e = side.setdefault("assistant:text", {"count": 0, "chars": 0, "carry": 0.0, "tok": []})
            e["count"] += 1
            e["chars"] += chars
            e["tok"].append(chars / r)
            e["carry"] += chars / r * (n - 1 - t)
        use = {tid: (name, arg) for us in s["uses"] for tid, name, arg in us}
        for tc, tid, chars, imgs in s["results"]:
            name, arg = use.get(tid, ("", ""))
            cls = tool_class(name, arg)
            after = n - tc
            e = by.setdefault(cls, {"count": 0, "chars": 0, "carry": 0.0, "capped_carry": 0.0, "tok": []})
            e["count"] += 1
            e["chars"] += chars
            e["tok"].append(chars / r)
            e["carry"] += chars / r * after
            e["capped_carry"] += min(chars, CAP_CHARS) / r * after
            images += imgs
    total_carry = sum(e["carry"] for e in by.values())
    out, rows = {}, []
    for cls, e in sorted(by.items(), key=lambda kv: -kv[1]["carry"]):
        ent = {"count": e["count"], "chars": e["chars"], "tokens": e["chars"] / r,
               "carry_cost": e["carry"], "carry_share_pct": share(e["carry"], total_carry),
               "result_tokens_median": med(e["tok"]), "result_tokens_p90": pct(e["tok"], 0.9)}
        out[cls] = ent
        rows.append([cls, fmt(ent["count"]), fmt(ent["chars"]), fmt(ent["tokens"]), fmt(ent["carry_cost"]),
                     f"{ent['carry_share_pct']:.1f}", fmt(ent["result_tokens_median"]),
                     fmt(ent["result_tokens_p90"])])
    tot = {"count": sum(e["count"] for e in by.values()), "chars": sum(e["chars"] for e in by.values()),
           "tokens": sum(e["chars"] for e in by.values()) / r, "carry_cost": total_carry}
    rows.append(["TOTAL", fmt(tot["count"]), fmt(tot["chars"]), fmt(tot["tokens"]), fmt(total_carry),
                 "100.0", "-", "-"])
    gate_rows, gate = [], {}
    for cls in GATE_CLASSES:
        e = by.get(cls)
        carry = e["carry"] if e else 0.0
        capped = e["capped_carry"] if e else 0.0
        gate[cls] = {"carry_cost": carry, "capped_carry_cost": capped, "saving": carry - capped,
                     "saving_pct_of_integral": share(carry - capped, wl_integral)}
        gate_rows.append([cls, fmt(carry), fmt(capped), fmt(carry - capped),
                          f"{gate[cls]['saving_pct_of_integral']:.2f}"])
    gsum = {k: sum(g[k] for g in gate.values()) for k in ("carry_cost", "capped_carry_cost", "saving")}
    gsum["saving_pct_of_integral"] = share(gsum["saving"], wl_integral)
    gate_rows.append(["GATE TOTAL", fmt(gsum["carry_cost"]), fmt(gsum["capped_carry_cost"]),
                      fmt(gsum["saving"]), f"{gsum['saving_pct_of_integral']:.2f}"])
    all_share = share(total_carry, wl_integral)
    side_total = sum(e["carry"] for e in side.values())
    side_out, side_rows = {}, []
    for cls, e in sorted(side.items(), key=lambda kv: -kv[1]["carry"]):
        side_out[cls] = {"count": e["count"], "chars": e["chars"], "tokens": e["chars"] / r,
                         "carry_cost": e["carry"], "carry_share_pct": share(e["carry"], side_total),
                         "tokens_median": med(e["tok"]), "tokens_p90": pct(e["tok"], 0.9)}
        o = side_out[cls]
        side_rows.append([cls, fmt(o["count"]), fmt(o["chars"]), fmt(o["tokens"]), fmt(o["carry_cost"]),
                          f"{o['carry_share_pct']:.1f}", fmt(o["tokens_median"]), fmt(o["tokens_p90"])])
    in_carry = sum(e["carry"] for k, e in side.items() if k.startswith("in:"))
    text_carry = side.get("assistant:text", {"carry": 0.0})["carry"]
    comp = {"tool_results_pct": share(total_carry, wl_integral), "tool_inputs_pct": share(in_carry, wl_integral),
            "assistant_text_pct": share(text_carry, wl_integral),
            "thinking_pct_uncertain": share(thinking["carry"], wl_integral),
            "floor_pct": share(floor, wl_integral)}
    obj = {"meta": meta, "ratio": r, "worker_leads": len(wl), "worker_lead_integral": wl_integral,
           "classes": out, "total": tot, "gate_hygiene": gate, "gate_total": gsum,
           "all_results_carry_share_of_integral_pct": all_share, "image_results": images,
           "assistant_side": side_out,
           "thinking": {"count": thinking["count"], "chars": thinking["chars"],
                        "tokens": thinking["chars"] / r, "carry_cost": thinking["carry"],
                        "note": "whether prior thinking stays in context is uncertain"},
           "growth_composition_pct_of_integral": comp}
    text = (hdr(meta) + f"\nratio={r} chars/token   worker-leads={len(wl)}   "
            f"worker-lead integral={fmt(wl_integral)}\n\n"
            + table(["class", "count", "chars", "tokens", "carry_cost", "share%", "tok_med", "tok_p90"], rows)
            + f"\n\ngate hygiene (cap each result at {CAP_CHARS} chars):\n"
            + table(["class", "carry", "capped_carry", "saving", "saving_%integral"], gate_rows)
            + f"\n\nall tool-result carry cost = {all_share:.1f}% of worker-lead context integral "
              f"(image results: {images})"
            + "\n\nassistant-side content that stays in context (tool_use inputs, assistant text):\n"
            + table(["class", "count", "chars", "tokens", "carry_cost", "share%", "tok_med", "tok_p90"], side_rows)
            + f"\n\nthinking blocks (reported separately; whether prior thinking stays in context is UNCERTAIN): "
              f"count={fmt(thinking['count'])} chars={fmt(thinking['chars'])} "
              f"tokens={fmt(thinking['chars'] / r)} carry_cost={fmt(thinking['carry'])}"
            + f"\n\ngrowth composition (carry cost as % of worker-lead integral): "
              f"tool results {comp['tool_results_pct']:.1f}% / tool inputs {comp['tool_inputs_pct']:.1f}% / "
              f"assistant text {comp['assistant_text_pct']:.1f}% / thinking {comp['thinking_pct_uncertain']:.1f}% "
              f"(uncertain) / floor (first-turn ctx x turns) {comp['floor_pct']:.1f}%")
    emit(args, obj, text)


# ---------------------------------------------------------------- sections

HEADING = re.compile(rb"^(#{1,3}) (.*?)\s*$")
FENCE = re.compile(rb"^(`{3,}|~{3,})(.*)$")


def cmd_sections(args):
    data = Path(args.file).read_bytes()
    size = len(data)
    rows = [["preamble", 0, 0]]
    fence = None
    for line in data.splitlines(keepends=True):
        f = FENCE.match(line.lstrip())
        if fence:
            if f and f.group(1)[:1] == fence[0] and len(f.group(1)) >= fence[1] and not f.group(2).strip():
                fence = None
            m = None
        elif f and not (f.group(1)[:1] == b"`" and b"`" in f.group(2)):
            fence = (f.group(1)[:1], len(f.group(1)))
            m = None
        else:
            m = HEADING.match(line.rstrip(b"\r\n"))
        if m:
            rows.append([m.group(2).decode("utf-8", "replace"), len(m.group(1)), 0])
        rows[-1][2] += len(line)
    total = sum(r[2] for r in rows)
    assert total == size, f"sections sum {total} != file size {size}"
    r = args.ratio
    obj = {"file": args.file, "ratio": r, "bytes": size, "tokens": size / r,
           "sections": [{"heading": h, "level": lv, "bytes": b, "tokens": b / r} for h, lv, b in rows]}
    trs = [[("  " * max(lv - 1, 0)) + h if lv else h, lv or "-", fmt(b), fmt(b / r)] for h, lv, b in rows]
    trs.append(["TOTAL", "", fmt(total), fmt(total / r)])
    emit(args, obj, f"{args.file}  ({size} bytes, ratio {r})\n\n" + table(["section", "lvl", "bytes", "tokens"], trs))


# -------------------------------------------------------------- dispatcher

def wake_segments(s):
    """Wakes with the turn range [a, b) they trigger; consecutive wakes collapse."""
    n = len(s["turns"])
    marks = s["marks"]
    wakes = []
    for i, (tc, kind, sub) in enumerate(marks):
        if kind != "W" or tc >= n:
            continue
        if wakes and wakes[-1]["a"] == tc:
            wakes[-1]["mark"] = i
            wakes[-1]["kinds"].append(sub)
            continue
        wakes.append({"a": tc, "mark": i, "kinds": [sub]})
    for w in wakes:
        end = n
        for tc, _, _ in marks[w["mark"] + 1:]:
            if tc > w["a"]:
                end = tc
                break
        w["b"] = end
    return [w for w in wakes if w["b"] > w["a"]]


def is_idle(s, a, b):
    for us in s["uses"][a:b]:
        for _, name, arg in us:
            if name in IDLE_TOOLS or (name == "Bash" and IDLE_BASH.search(arg)):
                return False
    return True


def cmd_dispatcher(args):
    sessions, meta = load(args)
    ds = [s for s in sessions if s["class"] == "dispatcher"]
    total = sum(sum(s["turns"]) for s in ds)
    n_turns = sum(len(s["turns"]) for s in ds)
    wakes = idle = 0
    idle_int = wake_int = 0
    idle_ctxs = []
    kinds = {}
    for s in ds:
        for w in wake_segments(s):
            wakes += 1
            for k in w["kinds"]:
                kinds[k] = kinds.get(k, 0) + 1
            integ = sum(s["turns"][w["a"]:w["b"]])
            wake_int += integ
            if is_idle(s, w["a"], w["b"]):
                idle += 1
                idle_int += integ
                idle_ctxs.extend(s["turns"][w["a"]:w["b"]])
    pts, rows = {}, []
    for label, pos in (("turn 1", 1), ("turn 50", 50), ("turn 100", 100), ("turn 200", 200), ("end", None)):
        vals = [(s["turns"][pos - 1] if pos else s["turns"][-1]) for s in ds if pos is None or len(s["turns"]) >= pos]
        pts[label] = {"n": len(vals), "median_ctx": med(vals)}
        rows.append([label, len(vals), fmt(med(vals))])
    restart = {}
    for n in (int(x) for x in args.restart_every.split(",") if x.strip()):
        saving = sum(restart_saving(s["turns"], args.reorient, range(n, len(s["turns"]), n)) for s in ds)
        restart[n] = {"total_saving": saving, "saving_pct_of_integral": share(saving, total)}
    summary = {"sessions": len(ds), "turns": n_turns, "integral": total, "wakes": wakes, "idle_wakes": idle,
               "wake_kinds": kinds, "idle_wake_integral": idle_int,
               "idle_wake_share_pct": share(idle_int, total), "wake_integral": wake_int,
               "wake_share_pct": share(wake_int, total),
               "idle_wake_turns": len(idle_ctxs), "idle_wake_turn_ctx_median": med(idle_ctxs), "session_turns_median": med([len(s["turns"]) for s in ds])}
    obj = {"meta": meta, "summary": summary, "ctx_at": pts, "reorient": args.reorient,
           "restart_model": {str(k): v for k, v in restart.items()}}
    text = (hdr(meta) + "\n\n"
            f"dispatcher sessions: {len(ds)}   turns: {fmt(n_turns)}   integral: {fmt(total)}\n"
            f"wakes: {wakes} (by kind: {kinds})   idle wakes: {idle}\n"
            f"idle-wake integral: {fmt(idle_int)} ({summary['idle_wake_share_pct']:.1f}% of dispatcher integral)\n"
            f"idle-wake turns: {fmt(len(idle_ctxs))}   median ctx of idle-wake turns: {fmt(med(idle_ctxs))}\n"
            f"all-wake integral: {fmt(wake_int)} ({summary['wake_share_pct']:.1f}%)\n"
            f"median session turns: {fmt(summary['session_turns_median'])}\n\n"
            + table(["point", "n", "median_ctx"], rows)
            + f"\n\nrestart model (fresh session every N turns, re-orient R={args.reorient:,} tokens):\n"
            + table(["every N turns", "total_saving", "saving_%integral"],
                    [[k, fmt(v["total_saving"]), f"{v['saving_pct_of_integral']:.1f}"] for k, v in restart.items()]))
    emit(args, obj, text)


# -------------------------------------------------------------------- main

def main():
    def add_globals(p, default):
        p.add_argument("--since", type=float, default=default(7), metavar="DAYS",
                       help="transcript mtime window (default 7)")
        p.add_argument("--json", action="store_true", default=default(False), help="machine-readable output")
        # 2.7 is calibrated on this repo's protocol markdown via first-turn usage deltas.
        p.add_argument("--ratio", type=float, default=default(2.7), help="chars per token (default 2.7)")
        p.add_argument("--reorient", type=int, default=default(10000), metavar="R",
                       help="tokens to re-orient after a simulated restart (default 10000)")
        p.add_argument("--restart-every", default=default("50,100"), metavar="N,N",
                       help="dispatcher restart-model intervals in turns (default 50,100)")
        p.add_argument("--root", default=default("~/.claude/projects"),
                       help="transcript root (default ~/.claude/projects)")

    ap = argparse.ArgumentParser(prog="context-budget", description="Read-only Claude context-budget report.")
    add_globals(ap, lambda v: v)
    common = argparse.ArgumentParser(add_help=False)
    add_globals(common, lambda v: argparse.SUPPRESS)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name, fn in (("fleet", cmd_fleet), ("growth", cmd_growth), ("tools", cmd_tools),
                     ("dispatcher", cmd_dispatcher)):
        sub.add_parser(name, parents=[common]).set_defaults(fn=fn)
    sp = sub.add_parser("sections", parents=[common])
    sp.add_argument("file")
    sp.set_defaults(fn=cmd_sections)
    args = ap.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
