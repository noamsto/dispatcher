package harness

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const rwTmuxStub = `#!/usr/bin/env bash
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
`

const rwSpawnTmuxStub = `#!/usr/bin/env bash
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
`

const (
	rwFramePermission = `
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
`
	rwFrameSelect = `  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`
	rwFrameQuota = `What do you want to do?
❯ 1. Stop and wait for limit to reset
  2. Upgrade your plan
  3. Upgrade to Team plan
Enter to select · Esc to cancel
`
	rwFrameIdle = `✻ Churned for 36s · done 11:20 AM · 1 shell still running
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
`
	rwFrameLive = `  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)
✶ Hatching… (6m 1s · ↓ 73.2k tokens)
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
`
	rwFrameUnknown = `● Some transcript line
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
`
)

const rwCrewStub = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
`

const rwLogStub = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
`

var (
	reSendGo      = regexp.MustCompile(`^(paste Assignment: go|send-keys -t %6 -l Assignment: go)$`)
	reSendTwo     = regexp.MustCompile(`^(paste Assignment: two|send-keys -t %6 -l Assignment: two)$`)
	reCapture     = regexp.MustCompile(`^capture-pane`)
	reKeysOrPaste = regexp.MustCompile(`^(send-keys|load-buffer|paste-buffer)`)
)

func roleWatchCase(kind string) func(*caseTest) {
	return func(t *caseTest) {
		rw := newRoleWatch(t)
		switch kind {
		case "dialog-clear":
			rw.dialogClear()
		case "defer-frames":
			rw.deferFrames()
		case "idle":
			rw.idle()
		case "queue":
			rw.queue()
		case "late-dialog":
			rw.lateDialog()
		default:
			t.Fatalf("unknown role-watch kind %s", kind)
		}
	}
}

type roleWatch struct {
	*repoFixture
	wt     string
	common string
	cmd    *exec.Cmd
	stderr bytes.Buffer
}

func newRoleWatch(t *caseTest) *roleWatch {
	t.Helper()
	f := newRepoFixture(t)
	rw := &roleWatch{repoFixture: f}
	t.Cleanup(func() { rw.stop() })
	for _, name := range []string{"gh", "wt", "direnv"} {
		writeExecutable(t.T, filepath.Join(f.stubDir, name), rwLogStub)
	}
	writeExecutable(t.T, filepath.Join(f.stubDir, "crew"), rwCrewStub)

	protocolDir := filepath.Join(f.dir, "protocols")
	skillsDir := filepath.Join(f.dir, "harness-skills", "spec-plan-critic")
	if err := os.MkdirAll(protocolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"WORKER_PROTOCOL.md", "EVIDENCE_REVIEW.md", "GRID_PROTOCOL.md", "REVIEW_TASK.md"} {
		if err := os.WriteFile(filepath.Join(protocolDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skill := "---\nname: spec-plan-critic\ndescription: seeded\n---\n"
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}

	f.env = f.withEnv(map[string]string{
		"HOME":                    f.dir,
		"STUB_PANE_PID":           strconv.Itoa(os.Getpid()),
		"TMUX_PANE":               "%5",
		"DISPATCHER_PROTOCOL_DIR": protocolDir,
		"DISPATCHER_SKILLS_DIR":   filepath.Join(f.dir, "harness-skills"),
		"CROSS_REPO_HINT_LIB":     filepath.Join(repoRoot, "adapters/core/cross-repo-hint.sh"),
		"CREW_REAL":               filepath.Join(repoRoot, "adapters/core/crew.sh"),
	}, "DISPATCH_REPO_TRACKERS", "DISPATCH_ORG_TRACKERS")

	commit := runCommand(f.dir, f.env, "git", "commit", "-q", "--allow-empty", "-m", "init")
	if commit.status != 0 {
		t.Fatalf("git commit: %s", commit.stderr)
	}
	rw.wt = filepath.Join(f.dir, ".dispatch-wt", "feat-9-x")
	add := runCommand(f.dir, f.env, "git", "worktree", "add", "-q", "-b", "feat/9-x", rw.wt)
	if add.status != 0 {
		t.Fatalf("git worktree add: %s", add.stderr)
	}
	task := "agent_name: iris\neffort: high\nworker_id: worker:feat/9-x#s1-1\ncrew_id: c1\n"
	if err := os.WriteFile(filepath.Join(rw.wt, "WORKER_TASK.md"), []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
	common := runCommand(rw.wt, f.env, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common.status != 0 {
		t.Fatalf("git common dir: %s", common.stderr)
	}
	rw.common = common.stdout
	rolesDir := filepath.Join(rw.common, "crew", "artifacts", "feat", "9-x")
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	roles := "{\"reviewer\":{\"agent\":\"pi\",\"model\":\"openrouter/deepseek/deepseek-v4-flash\"}}\n"
	if err := os.WriteFile(filepath.Join(rolesDir, "roles.json"), []byte(roles), 0o644); err != nil {
		t.Fatal(err)
	}
	recordDir := filepath.Join(rw.common, "crew", "protocol-dirs", "feat")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wtReal, err := filepath.EvalSymlinks(rw.wt)
	if err != nil {
		t.Fatal(err)
	}
	record := protocolDir + "\n" + filepath.Join(f.dir, "harness-skills") + "\n\n\n" + wtReal + "\n"
	if err := os.WriteFile(filepath.Join(recordDir, "9-x"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	f.env = f.withEnv(map[string]string{
		"STUB_CREW_DIR":    filepath.Join(rw.common, "crew"),
		"STUB_CREW_BRANCH": "feat/9-x",
	})
	writeExecutable(t.T, filepath.Join(f.stubDir, "tmux"), rwSpawnTmuxStub)
	return rw
}

func (r *roleWatch) framePath() string { return filepath.Join(r.stubDir, "frame") }

func (r *roleWatch) installRWStub(frame string) {
	r.t.Helper()
	if err := os.WriteFile(r.framePath(), []byte(frame), 0o644); err != nil {
		r.t.Fatal(err)
	}
	writeExecutable(r.t.T, filepath.Join(r.stubDir, "tmux"), rwTmuxStub)
}

func (r *roleWatch) start(engine, body string) {
	r.t.Helper()
	r.stderr.Reset()
	dispatch := filepath.Join(repoRoot, "adapters/core/dispatch.sh")
	cmd := exec.Command("bash", dispatch, "--role-watch", "reviewer", "--pane", "%6", "--engine", engine, "--branch", "feat/9-x", "--interval", "0.2")
	cmd.Dir = r.wt
	cmd.Env = r.env
	cmd.Stdout = io.Discard
	cmd.Stderr = &r.stderr
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.cmd = cmd
	time.Sleep(600 * time.Millisecond)
	r.post(body)
}

func (r *roleWatch) post(body string) {
	r.t.Helper()
	dir := filepath.Join(r.common, "crew")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("jq", "-nc", "--arg", "body", body, `{ts: (now*1000|floor), crew_id: "c1", kind: "msg", from: "worker:feat/9-x#s1-1", to: "role:feat/9-x:reviewer", body: $body}`)
	cmd.Dir = r.wt
	cmd.Env = r.env
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("jq post: %v", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(out); err != nil {
		r.t.Fatal(err)
	}
}

func (r *roleWatch) stop() {
	if r.cmd == nil {
		return
	}
	cmd := r.cmd
	r.cmd = nil
	_ = os.WriteFile(filepath.Join(r.stubDir, "stop"), nil, 0o644)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		r.t.Errorf("role-watch did not exit after stop\n%s", r.diagnostics())
	}
}

func (r *roleWatch) lines() []string {
	r.t.Helper()
	data, err := os.ReadFile(r.stubLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		r.t.Fatal(err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (r *roleWatch) diagnostics() string {
	data, _ := os.ReadFile(r.stubLog)
	return fmt.Sprintf("stderr:\n%s\nlog:\n%s", r.stderr.String(), data)
}

func countMatch(lines []string, re *regexp.Regexp) int {
	n := 0
	for _, line := range lines {
		if re.MatchString(line) {
			n++
		}
	}
	return n
}

func lineNo(lines []string, re *regexp.Regexp) int {
	for i, line := range lines {
		if re.MatchString(line) {
			return i + 1
		}
	}
	return 0
}

func hasExact(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func hasPrefix(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func (r *roleWatch) sends() int    { return countMatch(r.lines(), reSendGo) }
func (r *roleWatch) captures() int { return countMatch(r.lines(), reCapture) }

func (r *roleWatch) waitFor(attempts int, ok func() bool) bool {
	for range attempts {
		if ok() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (r *roleWatch) waitSends(n int) {
	r.t.Helper()
	if !r.waitFor(40, func() bool { return r.sends() >= n }) {
		r.t.Fatalf("timed out waiting for %d assignment sends\n%s", n, r.diagnostics())
	}
}

func (r *roleWatch) waitCaptures(n int) {
	r.t.Helper()
	if !r.waitFor(60, func() bool { return r.captures() >= n }) {
		r.t.Fatalf("timed out waiting for %d captures\n%s", n, r.diagnostics())
	}
}

func (r *roleWatch) dialogClear() {
	r.t.Helper()
	r.installRWStub(rwFramePermission)
	r.start("claude", "go")
	r.waitCaptures(3)
	r.t.check("rw-dialog-clear-no-sends", r.sends() == 0, "sends=%d\n%s", r.sends(), r.diagnostics())
	if hasPrefix(r.lines(), "send-keys") {
		r.t.Fatalf("send-keys while the permission dialog was up\n%s", r.diagnostics())
	}
	if err := os.WriteFile(r.framePath(), []byte(rwFrameIdle), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.waitSends(1)
	time.Sleep(800 * time.Millisecond)
	r.stop()
	r.t.check("rw-dialog-clear-one-send", r.sends() == 1, "sends=%d\n%s", r.sends(), r.diagnostics())
	if !hasExact(r.lines(), "send-keys -t %6 Enter") {
		r.t.Fatalf("missing Enter after the dialog cleared\n%s", r.diagnostics())
	}
}

func (r *roleWatch) deferFrames() {
	r.t.Helper()
	frames := []struct{ name, body string }{
		{"rw_frame_select", rwFrameSelect},
		{"rw_frame_quota", rwFrameQuota},
		{"rw_frame_live", rwFrameLive},
		{"rw_frame_unknown", rwFrameUnknown},
	}
	for _, frame := range frames {
		if err := os.WriteFile(r.stubLog, nil, 0o644); err != nil {
			r.t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(r.stubDir, "stop"))
		r.installRWStub(frame.body)
		r.start("claude", "go")
		if !r.waitFor(60, func() bool { return r.captures() >= 2 }) {
			r.t.Fatalf("%s: never captured\n%s", frame.name, r.diagnostics())
		}
		r.stop()
		ok := r.captures() >= 2
		r.t.check("rw-defer-frames-captured", ok, "%s captures=%d\n%s", frame.name, r.captures(), r.diagnostics())
		if !ok {
			return
		}
		if countMatch(r.lines(), reKeysOrPaste) != 0 {
			r.t.Fatalf("%s: send-keys, load-buffer, or paste-buffer ran\n%s", frame.name, r.diagnostics())
		}
		_ = os.Remove(filepath.Join(r.common, "crew", "events.jsonl"))
	}
}

func (r *roleWatch) idle() {
	r.t.Helper()
	r.installRWStub(rwFrameIdle)
	r.start("claude", "go")
	r.waitSends(1)
	time.Sleep(800 * time.Millisecond)
	r.stop()
	r.t.check("rw-idle-deliver-one-send", r.sends() == 1, "sends=%d\n%s", r.sends(), r.diagnostics())
}

func (r *roleWatch) queue() {
	r.t.Helper()
	r.installRWStub(rwFrameIdle)
	r.start("claude", "go")
	r.post("two")
	r.waitSends(1)
	r.waitFor(150, func() bool { return countMatch(r.lines(), reSendTwo) >= 1 })
	time.Sleep(1200 * time.Millisecond)
	r.stop()
	r.t.check("rw-queue-order-first-send", r.sends() == 1, "sends=%d\n%s", r.sends(), r.diagnostics())
	two := countMatch(r.lines(), reSendTwo)
	r.t.check("rw-queue-order-second-once", two == 1, "two=%d\n%s", two, r.diagnostics())
	goLine, twoLine := lineNo(r.lines(), reSendGo), lineNo(r.lines(), reSendTwo)
	r.t.check("rw-queue-order-ordering", goLine > 0 && twoLine > 0 && goLine < twoLine, "goLine=%d twoLine=%d\n%s", goLine, twoLine, r.diagnostics())
}

func (r *roleWatch) lateDialog() {
	r.t.Helper()
	r.installRWStub(rwFrameIdle)
	if err := os.WriteFile(filepath.Join(r.stubDir, "frame_after"), []byte(rwFramePermission), 0o644); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stubDir, "flip"), nil, 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.start("claude", "go")
	r.waitSends(1)
	time.Sleep(800 * time.Millisecond)
	if hasExact(r.lines(), "send-keys -t %6 Enter") {
		r.t.Fatalf("Enter confirmed the dialog\n%s", r.diagnostics())
	}
	if err := os.WriteFile(r.framePath(), []byte(rwFrameIdle), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor(40, func() bool { return hasExact(r.lines(), "send-keys -t %6 Enter") })
	r.stop()
	if !hasExact(r.lines(), "send-keys -t %6 Enter") {
		r.t.Fatalf("missing Enter after the dialog cleared\n%s", r.diagnostics())
	}
	if !hasExact(r.lines(), "send-keys -t %6 C-u") {
		r.t.Fatalf("missing C-u\n%s", r.diagnostics())
	}
	r.t.check("rw-late-dialog-sends-two", r.sends() == 2, "sends=%d\n%s", r.sends(), r.diagnostics())
	enters := 0
	for _, line := range r.lines() {
		if line == "send-keys -t %6 Enter" {
			enters++
		}
	}
	r.t.check("rw-late-dialog-single-enter", enters == 1, "enters=%d\n%s", enters, r.diagnostics())
}
