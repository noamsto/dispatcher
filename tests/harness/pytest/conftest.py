import os
import shutil
import subprocess
import tempfile
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[3]


@pytest.fixture(scope="session", autouse=True)
def crew_go_bin():
    """Build crew-go so raw-source runs of crew.sh reach the ported arms.

    crew.sh names the binary through CREW_GO_BIN, whose fallback is still the
    @crewGoBin@ placeholder in a checkout. tests/setup_suite.bash does this for
    bats; this is the same for a direct pytest run. An adapter exporting
    CREW_GO_BIN once for a whole bench run makes this a no-op, keeping the build
    out of the measured per-case window.
    """
    if os.environ.get("CREW_GO_BIN"):
        # A generator fixture has to yield on every path; pytest 9 fails the
        # test outright otherwise.
        yield
        return
    build_dir = Path(tempfile.mkdtemp(prefix="crew-go-harness."))
    try:
        subprocess.run(
            ["go", "build", "-o", str(build_dir / "crew-go"), "."],
            cwd=ROOT / "crew",
            env=os.environ | {"GOTOOLCHAIN": "local", "GOFLAGS": "-buildvcs=false"},
            check=True,
        )
        os.environ["CREW_GO_BIN"] = str(build_dir / "crew-go")
        yield
    finally:
        shutil.rmtree(build_dir, ignore_errors=True)


@pytest.fixture
def harness(tmp_path, monkeypatch):
    data = tmp_path / "data"
    config = tmp_path / "config"
    tmux = tmp_path / "tmux"
    repo = tmp_path / "repo"
    stubs = tmp_path / "stubs"
    for path in (data, config, tmux, repo, stubs):
        path.mkdir()

    for name in (
        "DISPATCH_ENGINES", "TMUX", "TMUX_PANE", "CREW_ID", "CREW_WORKER_ID",
        "CREW_ROLE_ID", "DISPATCHER_CRITICS_DIR", "DISPATCHER_REVIEWERS_DIR",
        "DISPATCHER_SKILLS_DIR", "DISPATCH_PROFILE", "DISPATCH_SKIP_MODEL_CHECK",
        "DISPATCH_IGNORE_RUNG", "DISPATCH_GRANT_ROOTS", "DISPATCH_SPEC",
        "DISPATCH_SHAPE", "DISPATCH_DRAFT_PR", "DISPATCH_CLAUDE_CONNECTORS",
        "DISPATCH_LOCKED_SETTINGS",
    ):
        monkeypatch.delenv(name, raising=False)

    env = os.environ.copy()
    env.update({
        "XDG_DATA_HOME": str(data),
        "PR_WATCH_CLOCK": str(tmp_path / "clock"),
        "XDG_CONFIG_HOME": str(config),
        "TMUX_TMPDIR": str(tmux),
        "CREW_RATE_AUTOSWEEP": "0",
        "DISPATCH_CONFIG_BIN": str(ROOT / "adapters/core/dispatch-config.sh"),
        "GRANT_CHECK_LIB": str(ROOT / "adapters/core/grant-check.sh"),
        "WORKTREE_GIT_LIB": str(ROOT / "adapters/core/worktree-git.sh"),
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "STUB_DIR": str(stubs),
        "STUB_LOG": str(stubs / "calls.log"),
        "PATH": f"{stubs}:{env['PATH']}",
    })
    subprocess.run(["git", "init", "-q", "-b", "main", "."], cwd=repo, env=env, check=True)
    subprocess.run(["git", "config", "user.email", "test@example.com"], cwd=repo, env=env, check=True)
    subprocess.run(["git", "config", "user.name", "test"], cwd=repo, env=env, check=True)
    for name in ("claude", "codex", "cursor-agent", "pi"):
        write_stub(stubs / name, 'printf \'%s\\n\' "$*" >>"$STUB_LOG"\n')
    return Harness(tmp_path, repo, stubs, env)


def write_stub(path, body):
    path.write_text(f"#!/usr/bin/env bash\n{body}", encoding="utf-8")
    path.chmod(0o755)


class Harness:
    def __init__(self, tmp_path, repo, stubs, env):
        self.tmp_path = tmp_path
        self.repo = repo
        self.stubs = stubs
        self.env = env

    def run(self, script, *args, cwd=None, env=None):
        run_env = self.env | (env or {})
        return subprocess.run(
            ["bash", "-euo", "pipefail", str(ROOT / script), *map(str, args)],
            cwd=cwd or self.repo,
            env=run_env,
            text=True,
            capture_output=True,
        )

    def crew(self, *args, cwd=None, env=None):
        return self.run("adapters/core/crew.sh", *args, cwd=cwd, env=env)

    def pr_watch(self, *args, cwd=None, env=None):
        return self.run("adapters/core/pr-watch.sh", *args, cwd=cwd, env=env)

    def stub(self, name, body):
        write_stub(self.stubs / name, body)


def pytest_addoption(parser):
    parser.addoption("--case-id", action="store")


def pytest_collection_modifyitems(config, items):
    case_id = config.getoption("--case-id")
    if not case_id:
        return
    keep = [item for item in items if item.callspec.id == case_id]
    if len(keep) != 1:
        raise pytest.UsageError(f"expected exactly one pytest case for {case_id}, found {len(keep)}")
    config.hook.pytest_deselected(items=[item for item in items if item not in keep])
    items[:] = keep
