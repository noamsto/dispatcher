import json
import os
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
    elif case_id.startswith("role-watch-"):
        role_watch(harness, case_id)
    else:
        pr_watch(harness, case_id)
    assert set(seen_assertions[before:]) == assertion_ids(case_id)

SEND_GO = re.compile(r"^(paste Assignment: go|send-keys -t %6 -l Assignment: go)$")
SEND_TWO = re.compile(r"^(paste Assignment: two|send-keys -t %6 -l Assignment: two)$")
CAPTURE = re.compile(r"^capture-pane")
KEYS_OR_PASTE = re.compile(r"^(send-keys|load-buffer|paste-buffer)")

RW_TMUX_STUB = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
esc=$'\033'
case "$1" in
display-message)
  case "$*" in
  *'#{@crew_exited}'*) printf '%s\n' 0 ;;
  *'#{@crew_role}|#{window_id}'*) printf '%s\n' 'reviewer|@1' ;;
  *)
    [ -e "$STUB_DIR/stop" ] && exit 1
    printf '%s\n' '%6'
    ;;
  esac
  ;;
show-options)
  case "${*: -1}" in
  @crew_branch) printf '%s\n' feat/9-x ;;
  @crew_id) [ -e "$STUB_DIR/no_crew_id" ] || printf '%s\n' c1 ;;
  esac
  ;;
capture-pane)
  case " $* " in
  *' -e '*) cat "$STUB_DIR/frame" ;;
  *) sed -E "s/${esc}\\[[0-9;]*m//g" "$STUB_DIR/frame" ;;
  esac
  ;;
send-keys)
  # dispatch.sh no longer types the assignment via send-keys -l (delivery is
  # paste-buffer below), but the flip trigger stays here too so a fixture that
  # still exercises literal typing (e.g. the bare Enter/C-u keystrokes) has a
  # frame-swap path to hook into.
  if [ "$2 $3 $4" = "-t %6 -l" ] && [ -e "$STUB_DIR/flip" ]; then
    rm -f "$STUB_DIR/flip"
    cp "$STUB_DIR/frame_after" "$STUB_DIR/frame"
  fi
  [ -x "$STUB_DIR/hook" ] && "$STUB_DIR/hook" "$@"
  ;;
load-buffer)
  [ -e "$STUB_DIR/load_buffer_fail" ] && exit 1
  cat >"$STUB_DIR/paste_payload"
  ;;
paste-buffer)
  [ -e "$STUB_DIR/paste_buffer_fail" ] && exit 1
  printf 'paste %s\n' "$(cat "$STUB_DIR/paste_payload" 2>/dev/null)" >>"$STUB_LOG"
  if [ -e "$STUB_DIR/flip" ]; then
    rm -f "$STUB_DIR/flip"
    cp "$STUB_DIR/frame_after" "$STUB_DIR/frame"
  fi
  [ -x "$STUB_DIR/hook" ] && "$STUB_DIR/hook" "$@"
  ;;
esac
exit 0
"""
RW_SPAWN_TMUX_STUB = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message)
  case "${*: -1}" in
  '#{pane_pid}') printf '%s\n' "$STUB_PANE_PID" ;;
  *) printf '%s\n' '@1' ;;
  esac
  ;;
show-options)
  case "${*: -1}" in
  @crew_dir) printf '%s\n' "$STUB_CREW_DIR" ;;
  @crew_branch) printf '%s\n' "$STUB_CREW_BRANCH" ;;
  esac
  ;;
list-panes) ;;
split-window) printf '%s\n' '%6' ;;
esac
exit 0
"""
RW_FRAME_PERMISSION = r"""
● Waiting on the shell reviewer and test-runner.

✻ Waiting for 1 background agent to finish

› Message from @a6fd725715048d707 (ctrl+o to expand)

● The test-runner confirmed every acceptance criterion passes on the branch, and
  the allowlist tests fail on main. Only the shell reviewer is still out.

✻ Waiting for 2 background agents to finish

● Agent "Review: targeted test-runner" finished · 16m 44s

● Waiting on the shell reviewer.

✻ Waiting for 1 background agent to finish

──────────────────────────────────────────────────────────────────────────────────
 Bash command · from the shell-reviewer agent

   bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30
   Run bats tests matching grant filter in dispatch-resume.bats

 │ Auto mode classifier requires confirmation for this command.
 │ 3 consecutive actions were blocked. Please review the transcript before
 │ continuing.
 │
 │ Latest blocked action: [Irreversible Local Destruction]

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No

 Esc to cancel · Tab to amend
"""
RW_FRAME_SELECT = r"""  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
"""
RW_FRAME_QUOTA = r"""What do you want to do?
❯ 1. Stop and wait for limit to reset
  2. Upgrade your plan
  3. Upgrade to Team plan
Enter to select · Esc to cancel
"""
RW_FRAME_IDLE = r"""✻ Churned for 36s · done 11:20 AM · 1 shell still running
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
"""
RW_FRAME_LIVE = r"""  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)
✶ Hatching… (6m 1s · ↓ 73.2k tokens)
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
"""
RW_FRAME_UNKNOWN = r"""● Some transcript line
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
"""


class RoleWatch:
    def __init__(self, h):
        self.h = h
        self.proc = None
        self.stderr = ""
        self._prepare()

    def _prepare(self):
        h = self.h
        for name in ("gh", "wt", "direnv"):
            h.stub(name, "printf '%s\\n' \"$*\" >>\"$STUB_LOG\"\nexit 0\n")
        h.stub(
            "crew",
            "printf '%s\\n' \"$*\" >>\"$STUB_LOG\"\n"
            'case "$1" in\n'
            'pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;\n'
            "esac\nexit 0\n",
        )
        protocol = h.repo / "protocols"
        skills = h.repo / "harness-skills"
        protocol.mkdir()
        (skills / "spec-plan-critic").mkdir(parents=True)
        for name in ("WORKER_PROTOCOL.md", "EVIDENCE_REVIEW.md", "GRID_PROTOCOL.md", "REVIEW_TASK.md"):
            (protocol / name).write_text("")
        (skills / "spec-plan-critic" / "SKILL.md").write_text("---\nname: spec-plan-critic\ndescription: seeded\n---\n")
        h.env.update({
            "HOME": str(h.repo),
            "STUB_PANE_PID": str(os.getpid()),
            "TMUX_PANE": "%5",
            "DISPATCHER_PROTOCOL_DIR": str(protocol),
            "DISPATCHER_SKILLS_DIR": str(skills),
            "CROSS_REPO_HINT_LIB": str(ROOT / "adapters/core/cross-repo-hint.sh"),
            "CREW_REAL": os.environ["CREW_GO_BIN"],
        })
        for name in ("DISPATCH_REPO_TRACKERS", "DISPATCH_ORG_TRACKERS"):
            h.env.pop(name, None)
        subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "init"], cwd=h.repo, env=h.env, check=True)
        self.wt = h.repo / ".dispatch-wt" / "feat-9-x"
        subprocess.run(["git", "worktree", "add", "-q", "-b", "feat/9-x", str(self.wt)], cwd=h.repo, env=h.env, check=True)
        (self.wt / "WORKER_TASK.md").write_text("agent_name: iris\neffort: high\nworker_id: worker:feat/9-x#s1-1\ncrew_id: c1\n")
        common = subprocess.run(
            ["git", "rev-parse", "--path-format=absolute", "--git-common-dir"],
            cwd=self.wt, env=h.env, check=True, text=True, capture_output=True,
        ).stdout.strip()
        self.common = Path(common)
        roles = self.common / "crew/artifacts/feat/9-x"
        roles.mkdir(parents=True)
        (roles / "roles.json").write_text('{"reviewer":{"agent":"pi","model":"openrouter/deepseek/deepseek-v4-flash"}}\n')
        record_dir = self.common / "crew/protocol-dirs/feat"
        record_dir.mkdir(parents=True)
        (record_dir / "9-x").write_text(f"{protocol}\n{skills}\n\n\n{self.wt.resolve()}\n")
        h.env.update({"STUB_CREW_DIR": str(self.common / "crew"), "STUB_CREW_BRANCH": "feat/9-x"})
        self._write_tmux(RW_SPAWN_TMUX_STUB)

    def _write_tmux(self, body):
        path = self.h.stubs / "tmux"
        path.write_text(body, encoding="utf-8")
        path.chmod(0o755)

    def install_stub(self, frame):
        (self.h.stubs / "frame").write_text(frame, encoding="utf-8")
        self._write_tmux(RW_TMUX_STUB)

    def start(self, engine="claude", body="go"):
        dispatch = ROOT / "adapters/core/dispatch.sh"
        self.proc = subprocess.Popen(
            ["bash", str(dispatch), "--role-watch", "reviewer", "--pane", "%6", "--engine", engine, "--branch", "feat/9-x", "--interval", "0.2"],
            cwd=self.wt,
            env=self.h.env,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        time.sleep(0.6)
        self.post(body)

    def post(self, body):
        crew = self.common / "crew"
        crew.mkdir(parents=True, exist_ok=True)
        out = subprocess.run(
            ["jq", "-nc", "--arg", "body", body, '{ts: (now*1000|floor), crew_id: "c1", kind: "msg", from: "worker:feat/9-x#s1-1", to: "role:feat/9-x:reviewer", body: $body}'],
            cwd=self.wt, env=self.h.env, check=True, text=True, capture_output=True,
        ).stdout
        with (crew / "events.jsonl").open("a", encoding="utf-8") as handle:
            handle.write(out)

    def stop(self):
        proc = self.proc
        if proc is None:
            return
        self.proc = None
        (self.h.stubs / "stop").write_text("")
        try:
            _, err = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            _, err = proc.communicate(timeout=5)
            self.stderr = err or ""
            raise AssertionError(f"role-watch did not exit after stop\n{self.diagnostics()}")
        self.stderr = err or ""

    def lines(self):
        path = Path(self.h.env["STUB_LOG"])
        if not path.exists():
            return []
        text = path.read_text(encoding="utf-8").rstrip("\n")
        return [] if text == "" else text.split("\n")

    def diagnostics(self):
        log = ""
        path = Path(self.h.env["STUB_LOG"])
        if path.exists():
            log = path.read_text(encoding="utf-8")
        return f"stderr:\n{self.stderr}\nlog:\n{log}"

    def sends(self):
        return sum(1 for line in self.lines() if SEND_GO.search(line))

    def captures(self):
        return sum(1 for line in self.lines() if CAPTURE.search(line))

    def wait_for(self, attempts, predicate):
        for _ in range(attempts):
            if predicate():
                return True
            time.sleep(0.1)
        return False

    def wait_sends(self, n):
        assert self.wait_for(40, lambda: self.sends() >= n), f"timed out waiting for {n} sends\n{self.diagnostics()}"

    def wait_captures(self, n):
        assert self.wait_for(60, lambda: self.captures() >= n), f"timed out waiting for {n} captures\n{self.diagnostics()}"


def _exact(lines, want):
    return any(line == want for line in lines)


def _tracked(assert_id, condition, rw):
    if not condition:
        raise AssertionError(f"{assert_id}\n{rw.diagnostics()}")
    expect(assert_id, True)


def role_watch(h, case_id):
    rw = RoleWatch(h)
    try:
        if case_id.endswith("lands-once"):
            _dialog_clear(rw)
        elif "option-select" in case_id:
            _defer_frames(rw)
        elif "idle-claude" in case_id:
            _idle(rw)
        elif "queued-assignments" in case_id:
            _queue(rw)
        elif "never-confirmed" in case_id:
            _late_dialog(rw)
        else:
            raise AssertionError(f"unhandled role-watch case {case_id}")
    finally:
        rw.stop()


def _dialog_clear(rw):
    rw.install_stub(RW_FRAME_PERMISSION)
    rw.start()
    rw.wait_captures(3)
    _tracked("rw-dialog-clear-no-sends", rw.sends() == 0, rw)
    assert not any(line.startswith("send-keys") for line in rw.lines()), rw.diagnostics()
    (rw.h.stubs / "frame").write_text(RW_FRAME_IDLE, encoding="utf-8")
    rw.wait_sends(1)
    time.sleep(0.8)
    rw.stop()
    _tracked("rw-dialog-clear-one-send", rw.sends() == 1, rw)
    assert _exact(rw.lines(), "send-keys -t %6 Enter"), rw.diagnostics()


def _defer_frames(rw):
    for name, frame in (
        ("rw_frame_select", RW_FRAME_SELECT),
        ("rw_frame_quota", RW_FRAME_QUOTA),
        ("rw_frame_live", RW_FRAME_LIVE),
        ("rw_frame_unknown", RW_FRAME_UNKNOWN),
    ):
        Path(rw.h.env["STUB_LOG"]).write_text("")
        (rw.h.stubs / "stop").unlink(missing_ok=True)
        rw.install_stub(frame)
        rw.start()
        if not rw.wait_for(60, lambda: rw.captures() >= 2):
            raise AssertionError(f"{name}: never captured\n{rw.diagnostics()}")
        rw.stop()
        _tracked("rw-defer-frames-captured", rw.captures() >= 2, rw)
        assert not any(KEYS_OR_PASTE.search(line) for line in rw.lines()), f"{name}\n{rw.diagnostics()}"
        (rw.common / "crew/events.jsonl").unlink(missing_ok=True)


def _idle(rw):
    rw.install_stub(RW_FRAME_IDLE)
    rw.start()
    rw.wait_sends(1)
    time.sleep(0.8)
    rw.stop()
    _tracked("rw-idle-deliver-one-send", rw.sends() == 1, rw)


def _queue(rw):
    rw.install_stub(RW_FRAME_IDLE)
    rw.start()
    rw.post("two")
    rw.wait_sends(1)
    rw.wait_for(150, lambda: any(SEND_TWO.search(line) for line in rw.lines()))
    time.sleep(1.2)
    rw.stop()
    _tracked("rw-queue-order-first-send", rw.sends() == 1, rw)
    two = sum(1 for line in rw.lines() if SEND_TWO.search(line))
    _tracked("rw-queue-order-second-once", two == 1, rw)
    go_line = next((i for i, line in enumerate(rw.lines(), 1) if SEND_GO.search(line)), 0)
    two_line = next((i for i, line in enumerate(rw.lines(), 1) if SEND_TWO.search(line)), 0)
    _tracked("rw-queue-order-ordering", go_line > 0 and two_line > 0 and go_line < two_line, rw)


def _late_dialog(rw):
    rw.install_stub(RW_FRAME_IDLE)
    (rw.h.stubs / "frame_after").write_text(RW_FRAME_PERMISSION, encoding="utf-8")
    (rw.h.stubs / "flip").write_text("")
    rw.start()
    rw.wait_sends(1)
    time.sleep(0.8)
    assert not _exact(rw.lines(), "send-keys -t %6 Enter"), rw.diagnostics()
    (rw.h.stubs / "frame").write_text(RW_FRAME_IDLE, encoding="utf-8")
    rw.wait_for(40, lambda: _exact(rw.lines(), "send-keys -t %6 Enter"))
    rw.stop()
    assert _exact(rw.lines(), "send-keys -t %6 Enter"), rw.diagnostics()
    assert _exact(rw.lines(), "send-keys -t %6 C-u"), rw.diagnostics()
    _tracked("rw-late-dialog-sends-two", rw.sends() == 2, rw)
    enters = sum(1 for line in rw.lines() if line == "send-keys -t %6 Enter")
    _tracked("rw-late-dialog-single-enter", enters == 1, rw)
