import json
import re
import shutil
import subprocess
import time
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
    # Only the tools the script needs, so a real claude/pi/codex/curl on the
    # host never leaks into a run; engine stubs are added per case.
    tools = h.tmp_path / "tools"
    tools.mkdir()
    for tool in "bash jq grep sed head date mkdir mv cat find env chmod tr".split():
        (tools / tool).symlink_to(shutil.which(tool))
    h.env["PATH"] = f"{h.stubs}:{tools}"
    for name in ("claude", "codex", "pi"):
        (h.stubs / name).unlink()


PI_STUB = """[[ -n "${SHIM_PI_FAIL:-}" ]] && exit 1
printf 'provider    model    context  max-out\\n'
printf 'openrouter  deepseek/deepseek-v4.1-flash  1M  64K\\n'
printf 'anthropic   claude-haiku-4-5  200K  64K\\n'
[[ -n "${SHIM_PI_EXTRA:-}" ]] && printf '%b\\n' "$SHIM_PI_EXTRA"
if [[ -n "${SHIM_PI_MANY:-}" ]]; then
  for ((i = 0; i < 2100; i++)); do
    printf 'p  vendor/a-fairly-long-model-name-for-size-padding-padding-padding-%d  1M  64K\\n' "$i"
  done
fi
exit 0
"""

CURL_STUB = """printf '%s\\n' "$*" >>"$STUB_LOG"
filler=$(head -c 200000 /dev/zero | tr '\\0' 'a')
echo '{"data":[{"id":"deepseek/deepseek-v4.1-flash","created":1789000000,"pricing":{"prompt":"0.0000001","completion":"0.0000004"}},{"id":"filler","description":"'"$filler"'"}]}'
"""


def refresh_catalog(h, env, before=None):
    h.stub("claude", 'echo "Usage: claude [options]"\n')
    h.stub("codex", "exit 0\n")
    h.stub("pi", PI_STUB)
    h.stub("curl", CURL_STUB)
    codex = Path(h.env["HOME"]) / ".codex"
    codex.mkdir()
    (codex / "models_cache.json").write_text('{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.5"}]}\n')
    data = Path(h.env["XDG_DATA_HOME"]) / "crew"
    if before:
        data.mkdir(parents=True)
        (data / "model-catalog.json").write_text(before)
    result = h.run("adapters/core/refresh-models.sh", env=env)
    return result, data / "cursor-models-cache.json", json.loads((data / "model-catalog.json").read_text())


def refresh_models(h, case_id):
    prepare_cursor(h)
    cache = Path(h.env["XDG_DATA_HOME"]) / "crew/cursor-models-cache.json"
    if "stubbed-cursor-agent-failure" in case_id or "cursor-agent-absent" in case_id:
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

    if "catalog-holds-all-four-engines" in case_id:
        result, _, cat = refresh_catalog(h, {})
        pi = {m["id"]: m for m in cat["pi"]["models"]}
        flash = pi["openrouter/deepseek/deepseek-v4.1-flash"]
        expect("rm-catalog-1", result.returncode == 0)
        expect("rm-catalog-2", cat["claude"]["source"] == "aliases")
        expect("rm-catalog-3", ",".join(m["id"] for m in cat["claude"]["models"]) == "fable,haiku,opus,sonnet")
        expect("rm-catalog-4", cat["codex"]["source"] == "codex models_cache.json")
        expect("rm-catalog-5", cat["codex"]["models"][0]["id"] == "gpt-5.5")
        expect("rm-catalog-6", cat["cursor"]["source"] == "cursor-agent --list-models")
        expect("rm-catalog-7", "cursor-grok-4.6-high" in [m["id"] for m in cat["cursor"]["models"]])
        expect("rm-catalog-8", cat["pi"]["source"] == "pi --list-models; openrouter /api/v1/models")
        expect("rm-catalog-9", (flash["created"], flash["price"]["in"] == 0.0000001, flash["price"]["out"] == 0.0000004) == (1789000000, True, True))
        expect("rm-catalog-11", pi["anthropic/claude-haiku-4-5"] == {"id": "anthropic/claude-haiku-4-5"})
        expect("rm-catalog-12", all(isinstance(engine["fetched_epoch"], (int, float)) for engine in cat.values()))
        return

    if "failing-pi-probe" in case_id:
        old = '{"pi":{"fetched_epoch":1,"source":"old","models":[{"id":"old/pi-model"}]}}\n'
        result, _, cat = refresh_catalog(h, {"SHIM_PI_FAIL": "1"}, old)
        expect("rm-pi-fail-1", result.returncode == 0)
        expect("rm-pi-fail-2", "pi --list-models failed" in output(result))
        expect("rm-pi-fail-3", cat["pi"] == {"fetched_epoch": 1, "source": "old", "models": [{"id": "old/pi-model"}]})
        expect("rm-pi-fail-4", cat["cursor"]["source"] == "cursor-agent --list-models")
        expect("rm-pi-fail-5", cat["claude"]["source"] == "aliases")
        return

    if "ids-with-control-bytes" in case_id:
        extra = r"bad  evil\x1b[31mid  1M\nbad  has$dollar  1M\nbad  spa<ce  1M\nok  good.id:1@x  1M"
        result, _, cat = refresh_catalog(h, {"SHIM_PI_EXTRA": extra})
        ids = "\n".join(m["id"] for m in cat["pi"]["models"])
        expect("rm-ids-drop-1", result.returncode == 0)
        expect("rm-ids-drop-2", "ok/good.id:1@x" in ids)
        expect("rm-ids-drop-3", "evil" not in ids)
        expect("rm-ids-drop-4", "dollar" not in ids)
        expect("rm-ids-drop-5", "spa<ce" not in ids)
        expect("rm-ids-drop-6", not [m for e in cat.values() for m in e["models"] if re.search(r"[\x00-\x1f\x7f]", m["id"])])
        return

    if "engine-list-past-128-kib" in case_id:
        result, _, cat = refresh_catalog(h, {"SHIM_PI_MANY": "1"})
        expect("rm-cap-1", result.returncode == 0)
        expect("rm-cap-2", "pi: dropped" in output(result))
        expect("rm-cap-3", len(cat["pi"]["models"]) == 2000)
        expect("rm-cap-4", cat["claude"]["source"] == "aliases")
        return

    if "cursor-cache-is-unchanged" in case_id:
        result, cache, _ = refresh_catalog(h, {})
        data = json.loads(cache.read_text())
        slugs = ["gpt-5.3-codex-low", "cursor-grok-4.6-high", "cursor-grok-4.6-medium-fast", "cursor-grok-4.6-low-fast", "claude-opus-5-high"]
        expect("rm-cache-compat-1", result.returncode == 0)
        expect("rm-cache-compat-2", data["models"] == [{"slug": slug} for slug in slugs] and set(data) == {"fetched_at", "fetched_epoch", "models"})
        assert "openrouter.ai/api/v1/models" in (h.stubs / "calls.log").read_text(), "openrouter fetch did not reach the curl stub"
        return

    if "cursor-failing-while-another-engine" in case_id:
        result, _, cat = refresh_catalog(h, {"SHIM_CURSOR_FAIL": "1"})
        expect("rm-cursor-fail-1", result.returncode == 0)
        expect("rm-cursor-fail-2", "cursor-agent --list-models failed" in output(result))
        expect("rm-cursor-fail-3", "cursor" not in cat)
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


def set_view(h, head="aaa11", state="OPEN", conclusion="SUCCESS", pending=False,
             base="baseaaa", mergeable="MERGEABLE"):
    checks = ([{"name": "a", "status": "COMPLETED", "conclusion": "SUCCESS"},
               {"name": "b", "status": "IN_PROGRESS", "conclusion": ""}]
              if pending else [{"name": "check", "status": "COMPLETED", "conclusion": conclusion}])
    Path(h.env["GH_VIEW"]).write_text(json.dumps({
        "headRefOid": head, "state": state, "reviewDecision": "",
        "latestReviews": [], "statusCheckRollup": checks, "comments": [],
        "baseRefOid": base, "baseRefName": "main", "mergeable": mergeable,
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
    if "missing-base-sha" in case_id:
        cursor(h).parent.mkdir(parents=True, exist_ok=True)
        cursor(h).write_text(json.dumps({
            "head_sha": "aaa11", "state": "OPEN", "review_decision": "", "review_at": "",
            "checks": "SUCCESS", "thread_at": "", "thread_n": 0,
        }) + "\n")
        quiet = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
        expect("pw-oldcursor-quiet-status", quiet.returncode == 0)
        expect("pw-oldcursor-quiet-empty-stdout", not quiet.stdout)
        expect("pw-oldcursor-base-backfilled", json.loads(cursor(h).read_text())["base_sha"] == "baseaaa")
        set_view(h, base="basebbb")
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
        event = json.loads(result.stdout)
        expect("pw-oldcursor-event-status", result.returncode == 0)
        expect("pw-oldcursor-event", event["changed"] == ["base_sha"] and event["was"]["base_sha"] == "baseaaa")
        return
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
    if "default-clock" in case_id:
        h.env.pop("PR_WATCH_CLOCK", None)
        started = time.monotonic()
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
        elapsed = time.monotonic() - started
        expect("pw-default-clock-status", result.returncode == 0)
        expect("pw-default-clock-timeout-stderr", "park ended after 1s" in result.stderr)
        expect("pw-default-clock-elapsed", elapsed >= 1.0)
        return

    if "exits-with-the-child-s-status" in case_id:
        h.stub("pr-watch", "printf '%s\\n' '{\"pr\":42,\"changed\":[\"head_sha\"]}'\nprintf '%s\\n' 'gh: something went wrong' >&2\nexit 3\n")
        result = h.crew("pr-watch", "42", env={"CREW_ID": "c1"})
        expect("pw-crew-child-status", result.returncode == 3)
        expect("pw-crew-child-stderr", "gh: something went wrong" in result.stderr)
        expect("pw-crew-child-no-partial-stdout", '"changed"' not in result.stdout)
        expect("pw-crew-child-no-bus-row", not (h.repo / ".git/crew/events.jsonl").exists())
        return

    prefixes = {
        "first-park": "pw",
        "base-tip": "pw-base", "mergeable-flips": "pw-conflict",
        "unknown-mergeable": "pw-unknown-mergeable",
        "head-sha": "pw-head", "review-thread": "pw-thread", "in-flight": "pw-pending",
        "rollup": "pw-checks", "merges": "pw-merged", "unchanged": "pw-unchanged",
        "re-delivery": "pw-restart", "crew-id-unset": "pw-no-crew",
        "posts-the-event": "pw-crew-event", "posts-nothing": "pw-crew-timeout",
    }
    prefix = next(value for key, value in prefixes.items() if key in case_id)
    if prefix == "pw-pending":
        set_view(h, pending=True)
    seed(h, prefix)
    if prefix == "pw-base":
        set_view(h, base="basebbb")
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
        event = json.loads(result.stdout)
        expect("pw-base-status", result.returncode == 0)
        expect("pw-base-event", event["changed"] == ["base_sha"])
        expect("pw-base-state-transition", event["state"]["base_sha"] == "basebbb" and event["was"]["base_sha"] == "baseaaa")
        return
    if prefix == "pw-conflict":
        set_view(h, mergeable="CONFLICTING")
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
        event = json.loads(result.stdout)
        expect("pw-conflict-status", result.returncode == 0)
        expect("pw-conflict-event", event["changed"] == ["mergeable"])
        expect("pw-conflict-state-transition", event["state"]["mergeable"] == "CONFLICTING" and event["was"]["mergeable"] == "MERGEABLE")
        return
    if prefix == "pw-unknown-mergeable":
        set_view(h, mergeable="UNKNOWN")
        result = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
        expect("pw-unknown-mergeable-status", result.returncode == 0)
        expect("pw-unknown-mergeable-empty-stdout", not result.stdout)
        expect("pw-unknown-mergeable-cursor-kept", json.loads(cursor(h).read_text())["mergeable"] == "MERGEABLE")
        set_view(h)
        again = h.pr_watch("42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
        expect("pw-unknown-mergeable-recover-status", again.returncode == 0)
        expect("pw-unknown-mergeable-recover-empty-stdout", not again.stdout)
        return
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
    if case_id.startswith("refresh-models-"):
        refresh_models(harness, case_id)
    else:
        pr_watch(harness, case_id)
    assert set(seen_assertions[before:]) == assertion_ids(case_id)
