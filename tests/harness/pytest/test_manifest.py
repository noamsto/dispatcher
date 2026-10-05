import json
import subprocess
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[3]
MANIFEST = ROOT / "tests/harness/manifest.tsv"
ASSERTIONS = ROOT / "tests/harness/assertions.tsv"
seen_assertions = []


def manifest_ids():
    return [line.split("\t", 1)[0] for line in MANIFEST.read_text().splitlines()[1:]]


def assertion_ids(case_id):
    return {
        columns[1]
        for line in ASSERTIONS.read_text().splitlines()[1:]
        if (columns := line.split("\t"))[0] == case_id
    }


def expect(assert_id, condition):
    seen_assertions.append(assert_id)
    assert condition, assert_id


def output(result):
    return result.stdout + result.stderr


def crew_id(h, case_id):
    task = h.repo / "WORKER_TASK.md"
    if "no-worker-task-md" in case_id:
        result = h.crew("id", env={"CREW_ID": "c-from-env"})
        expect("cid-env-status", result.returncode == 0)
        expect("cid-env-stdout", result.stdout.strip() == "c-from-env")
    elif "has-no-crew-id-line" in case_id:
        task.write_text("title: some task\n")
        result = h.crew("id", env={"CREW_ID": "c-from-env"})
        expect("cid-taskdoc-missing-status", result.returncode == 0)
        expect("cid-taskdoc-missing-stdout", result.stdout.strip() == "c-from-env")
    else:
        task.write_text("crew_id: c-from-taskdoc\n")
        cwd = h.repo
        env = {"CREW_ID": "c-from-env"} if "wins-over" in case_id else {}
        if "subdirectory" in case_id:
            cwd = h.repo / "a/b/c"
            cwd.mkdir(parents=True)
        result = h.crew("id", cwd=cwd, env=env)
        prefix = "cid-taskdoc-wins" if "wins-over" in case_id else "cid-subdir" if "subdirectory" in case_id else "cid-taskdoc-unset"
        expect(f"{prefix}-status", result.returncode == 0)
        expect(f"{prefix}-stdout", result.stdout.strip() == "c-from-taskdoc")


MODELS = """Available models

auto - Auto (default)
gpt-5.3-codex-low - Codex 5.3 Low
cursor-grok-4.6-high - Cursor Grok 4.6
cursor-grok-4.6-medium-fast - Cursor Grok 4.6 Medium Fast
cursor-grok-4.6-low-fast - Cursor Grok 4.6 Low Fast
claude-opus-5-high - Claude Opus 5 1M

Tip: use --model <id>
"""


def prepare_cursor(h):
    h.stub("cursor-agent", 'printf \'%s\\n\' "$*" >>"$STUB_LOG"\n[[ -z "${SHIM_CURSOR_FAIL:-}" ]] || exit 1\ncat <<\'EOF\'\n' + MODELS + "EOF\n")
    h.env["HOME"] = str(h.tmp_path / "home")
    Path(h.env["HOME"]).mkdir()


def refresh_models(h, case_id):
    prepare_cursor(h)
    cache = Path(h.env["XDG_DATA_HOME"]) / "crew/cursor-models-cache.json"
    if "failure" in case_id or "absent" in case_id:
        cache.parent.mkdir(parents=True)
        stale = '{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}\n'
        cache.write_text(stale)
        env = {"SHIM_CURSOR_FAIL": "1"}
        if "absent" in case_id:
            (h.stubs / "cursor-agent").unlink()
            env = {"PATH": ":".join(p for p in h.env["PATH"].split(":") if not (Path(p) / "cursor-agent").exists())}
        result = h.run("adapters/core/refresh-models.sh", env=env)
        prefix = "rm-absent" if "absent" in case_id else "rm-stub-failure"
        expect(f"{prefix}-status", result.returncode != 0)
        expect(f"{prefix}-cache-unchanged", cache.read_text() == stale)
        return

    result = h.run("adapters/core/refresh-models.sh")
    if "expected-cache-shape" in case_id:
        expect("rm-shape-script-status", result.returncode == 0)
        data = json.loads(cache.read_text())
        slugs = [model["slug"] for model in data["models"]]
        expect("rm-shape-high-slug", "cursor-grok-4.6-high" in slugs)
        expect("rm-shape-medium-fast-slug", "cursor-grok-4.6-medium-fast" in slugs)
        expect("rm-shape-low-fast-slug", "cursor-grok-4.6-low-fast" in slugs)
        expect("rm-shape-excludes-auto", "auto" not in slugs)
        expect("rm-shape-epoch-type", isinstance(data["fetched_epoch"], (int, float)))
    else:
        expect("rm-atomic-script-status", result.returncode == 0)
        expect("rm-atomic-cache-exists", cache.is_file())
        expect("rm-atomic-no-temp-file", not list(cache.parent.glob("*.tmp.*")))
        expect("rm-atomic-model-count", len(json.loads(cache.read_text())["models"]) == 5)


def set_view(h, head="aaa11", state="OPEN", conclusion="SUCCESS", pending=False):
    checks = ([{"name": "a", "status": "COMPLETED", "conclusion": "SUCCESS"},
               {"name": "b", "status": "IN_PROGRESS", "conclusion": ""}]
              if pending else [{"name": "check", "status": "COMPLETED", "conclusion": conclusion}])
    Path(h.env["GH_VIEW"]).write_text(json.dumps({
        "headRefOid": head, "state": state, "reviewDecision": "",
        "latestReviews": [], "statusCheckRollup": checks, "comments": [],
    }))


def prepare_pr_watch(h):
    view = h.tmp_path / "view.json"
    threads = h.tmp_path / "threads.txt"
    h.env.update({"GH_VIEW": str(view), "GH_THREADS": str(threads)})
    threads.write_text("")
    h.stub("gh", 'printf \'%s\\n\' "$*" >>"$STUB_LOG"\ncase "$1" in\npr) cat "$GH_VIEW" ;;\napi) cat "$GH_THREADS" ;;\nesac\n')
    set_view(h)


def seed(h, prefix):
    result = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
    expect(f"{prefix}-seed-status", result.returncode == 0)
    expect(f"{prefix}-seed-empty-stdout", not result.stdout)
    expect(f"{prefix}-seed-timeout-stderr", "park ended after 1s" in result.stderr)


def cursor(h, repo="o/r"):
    return Path(h.env["XDG_DATA_HOME"]) / "crew/pr-watch" / repo / "42.json"


def pr_watch(h, case_id):
    prepare_pr_watch(h)
    if "aborts-without" in case_id:
        result = h.pr_watch("--repo", "o/r")
        expect("pw-missing-pr-status", result.returncode == 1)
        expect("pw-missing-pr-usage", "usage: pr-watch" in output(result))
        return
    if "unbounded" in case_id:
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "0")
        expect("pw-unbounded-status", result.returncode == 1)
        expect("pw-unbounded-message", "must be > 0" in output(result))
        return
    if "unknown-flag" in case_id:
        result = h.pr_watch("42", "--bogus")
        expect("pw-unknown-status", result.returncode == 1)
        expect("pw-unknown-message", "unknown arg" in output(result))
        return
    if "cannot-read" in case_id:
        h.stub("gh", "exit 1\n")
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
        expect("pw-gh-failure-status", result.returncode == 1)
        expect("pw-gh-failure-message", "could not read PR 42" in output(result))
        return
    if "no-git-repo" in case_id:
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1", cwd="/")
        expect("pw-explicit-repo-status", result.returncode == 0)
        expect("pw-explicit-repo-cursor", cursor(h).is_file())
        return
    if "derives-the-repo" in case_id:
        subprocess.run(["git", "remote", "add", "origin", "git@github.com:o/derived.git"], cwd=h.repo, env=h.env, check=True)
        result = h.pr_watch("42", "--timeout", "1", "--interval", "1")
        expect("pw-derived-repo-status", result.returncode == 0)
        expect("pw-derived-repo-cursor", cursor(h, "o/derived").is_file())
        return

    prefixes = {
        "first-park": "pw",
        "head-sha": "pw-head", "review-thread": "pw-thread", "in-flight": "pw-pending",
        "rollup": "pw-checks", "merges": "pw-merged", "unchanged": "pw-unchanged",
        "re-delivery": "pw-restart", "crew-id-unset": "pw-no-crew",
        "posts-the-event": "pw-crew-event", "posts-nothing": "pw-crew-timeout",
    }
    prefix = next(value for key, value in prefixes.items() if key in case_id)
    if prefix == "pw-pending":
        set_view(h, pending=True)
    seed(h, prefix)
    if "first-park" in case_id:
        expect("pw-first-cursor-exists", cursor(h).is_file())
        return
    if prefix == "pw-pending":
        expect("pw-pending-state", json.loads(cursor(h).read_text())["checks"] == "PENDING")
        return
    if prefix == "pw-crew-timeout":
        install_pr_watch_stub(h)
        result = h.crew("pr-watch", "42", "--repo", "o/r", "--timeout", "1", "--interval", "1", env={"CREW_ID": "c1"})
        expect("pw-crew-timeout-status", result.returncode == 0)
        expect("pw-crew-timeout-empty-stdout", not result.stdout)
        expect("pw-crew-timeout-no-bus-row", not (h.repo / ".git/crew/events.jsonl").exists())
        return

    if prefix == "pw-thread":
        Path(h.env["GH_THREADS"]).write_text("2026-08-04T10:00:00Z\n")
    elif prefix == "pw-checks":
        set_view(h, conclusion="FAILURE")
    elif prefix == "pw-merged":
        set_view(h, state="MERGED")
    elif prefix not in ("pw-unchanged",):
        set_view(h, head="bbb22")

    if prefix == "pw-crew-event":
        install_pr_watch_stub(h)
        result = h.crew("pr-watch", "42", "--repo", "o/r", "--timeout", "30", "--interval", "1", env={"CREW_ID": "c1"})
        expect("pw-crew-event-status", result.returncode == 0)
        expect("pw-crew-event-stdout", bool(result.stdout))
        rows = [json.loads(line) for line in (h.repo / ".git/crew/events.jsonl").read_text().splitlines()]
        row = next(row for row in rows if row["kind"] == "msg")
        body = json.loads(row["body"])
        expect("pw-crew-event-bus-row", f'{row["from"]}|{row["to"]}|{body["changed"][0]}' == "pr-watch:42|dispatcher:c1|head_sha")
        return

    result = h.pr_watch("42", "--repo", "o/r", "--timeout", "1" if prefix == "pw-unchanged" else "30", "--interval", "1")
    if prefix == "pw-head":
        event = json.loads(result.stdout)
        expect("pw-head-status", result.returncode == 0)
        expect("pw-head-event-shape", event["changed"] == ["head_sha"] and event["pr"] == 42 and event["repo"] == "o/r")
        expect("pw-head-state-transition", event["state"]["head_sha"] == "bbb22" and event["was"]["head_sha"] == "aaa11")
    elif prefix == "pw-thread":
        event = json.loads(result.stdout)
        expect("pw-thread-status", result.returncode == 0)
        expect("pw-thread-change-set", sorted(event["changed"]) == ["thread_at", "thread_n"])
        expect("pw-thread-count", event["state"]["thread_n"] == 1)
    elif prefix == "pw-checks":
        event = json.loads(result.stdout)
        expect("pw-checks-status", result.returncode == 0)
        expect("pw-checks-event", event["changed"] == ["checks"] and event["state"]["checks"] == "FAILURE")
    elif prefix == "pw-merged":
        event = json.loads(result.stdout)
        expect("pw-merged-status", result.returncode == 0)
        expect("pw-merged-event", event["changed"] == ["state"] and event["state"]["state"] == "MERGED")
    elif prefix == "pw-unchanged":
        expect("pw-unchanged-status", result.returncode == 0)
        expect("pw-unchanged-empty-stdout", not result.stdout)
    elif prefix == "pw-restart":
        expect("pw-restart-event-status", result.returncode == 0)
        expect("pw-restart-event-stdout", bool(result.stdout))
        repeat = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
        expect("pw-restart-repeat-status", repeat.returncode == 0)
        expect("pw-restart-repeat-empty-stdout", not repeat.stdout)
    elif prefix == "pw-no-crew":
        expect("pw-no-crew-status", result.returncode == 0)
        expect("pw-no-crew-event", bool(result.stdout))
        expect("pw-no-crew-no-bus", not (h.repo / ".git/crew").exists())


def install_pr_watch_stub(h):
    target = ROOT / "adapters/core/pr-watch.sh"
    h.stub("pr-watch", f'exec bash -euo pipefail "{target}" "$@"\n')


@pytest.mark.parametrize("case_id", manifest_ids(), ids=manifest_ids())
def test_manifest_case(harness, case_id):
    before = len(seen_assertions)
    if case_id.startswith("crew-id-"):
        crew_id(harness, case_id)
    elif case_id.startswith("refresh-models-"):
        refresh_models(harness, case_id)
    else:
        pr_watch(harness, case_id)
    assert set(seen_assertions[before:]) == assertion_ids(case_id)
