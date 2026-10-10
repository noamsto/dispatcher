package frame

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const crewScript = "../../../adapters/core/crew.sh"

var engines = []string{"claude", "codex", "pi", "cursor"}

// driver runs after the extracted functions are sourced. Usage:
// driver <engine> <mode> <frame file>...; a frame's colored capture is the
// sibling <name>.color.txt, else the plain text itself. It prints
// "@@ <file name>" and then one "key<TAB>value" line per predicate result,
// in the order results() produces them. Only _is_permission_prompt and
// _is_prompt read $engine, so mode "engine" stops after those two and the
// other engines reuse the "full" run's remaining results.
const driver = `
enc() {
  local v=$1
  v=${v//\\/\\\\}
  v=${v//$'\n'/\\n}
  v=${v//$'\033'/\\e}
  printf '%s' "$v"
}
emit() { printf '%s\t%s\n' "$1" "$(enc "$2")"; }
flag() {
  local k=$1
  shift
  if "$@" >/dev/null; then emit "$k" 1; else emit "$k" 0; fi
}
box() { # key text first-row-regex
  local out rc
  out=$(_box_rows "$2" "$3")
  rc=$?
  emit "$1.rc" "$rc"
  emit "$1.out" "$out"
}
idle() { # key text colored skipBg
  local out rc
  out=$(_pane_idle_reason "$2" "$3" "$4")
  rc=$?
  emit "$1" "rc=$rc reason=$out"
}

engine=$1
mode=$2
shift 2
_frame_classifier
. "$ISBGWAIT"
for f in "$@"; do
  echo "@@ ${f##*/}"
  text=$(cat "$f")
  cf=${f%.txt}.color.txt
  if [ -e "$cf" ]; then col=$(cat "$cf"); else col=$text; fi
  flag codex_hook_review _is_codex_hook_review_prompt "$text"
  flag permission_prompt _is_permission_prompt "$text"
  flag prompt _is_prompt "$text"
  [ "$mode" = full ] || continue
  flag quota_prompt _is_quota_prompt "$text"
  flag quota_session_limit _is_quota_session_limit "$text"
  flag quota_cursor_limit _is_quota_cursor_limit "$text"
  flag bg_wait _is_bg_wait "$text"
  emit meter "$(_meter_line "$text")"
  flag subrow _has_subrow "$text"
  flag pi_live_turn _pi_live_turn "$text"
  flag pi_working_row _pi_working_row "$text"
  flag pi_working_label _pi_working_label "$text"
  flag pi_idle_box _pi_idle_box "$text"
  box box_claude "$text" '^[[:space:]]*❯'
  box box_any "$text" '.*'
  flag claude_idle_box.own0.plain _claude_idle_box "$text" "" 0
  flag claude_idle_box.own0.color _claude_idle_box "$text" "$col" 0
  flag claude_idle_box.own1.plain _claude_idle_box "$text" "" 1
  flag claude_idle_box.own1.color _claude_idle_box "$text" "$col" 1
  idle idle_reason.skip0.plain "$text" "" 0
  idle idle_reason.skip0.color "$text" "$col" 0
  idle idle_reason.skip1.plain "$text" "" 1
  idle idle_reason.skip1.color "$text" "$col" 1
done
`

type kv struct{ key, val string }

var escaper = strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\x1b", "\\e")

// results mirrors the driver, key for key. colored is the "color" variant's
// colored capture.
func results(engine, text, colored string) []kv {
	var r []kv
	add := func(k, v string) { r = append(r, kv{k, escaper.Replace(v)}) }
	flag := func(k string, b bool) {
		if b {
			add(k, "1")
		} else {
			add(k, "0")
		}
	}
	box := func(k string, first string) {
		re := FirstRowClaude
		if first == "any" {
			re = FirstRowAny
		}
		row, above, ok := BoxRows(text, re)
		rc, out := 1, ""
		if ok {
			rc, out = 0, strings.Join(append([]string{row}, above...), "\n")
		}
		add(k+".rc", strconv.Itoa(rc))
		add(k+".out", out)
	}
	idle := func(k, colored string, skipBg bool) {
		reason, ok := PaneIdleReason(text, colored, skipBg)
		rc := 1
		if ok {
			rc = 0
		}
		add(k, fmt.Sprintf("rc=%d reason=%s", rc, reason))
	}
	flag("codex_hook_review", IsCodexHookReviewPrompt(text))
	flag("permission_prompt", IsPermissionPrompt(engine, text))
	flag("prompt", IsPrompt(engine, text))
	flag("quota_prompt", IsQuotaPrompt(text))
	flag("quota_session_limit", IsQuotaSessionLimit(text))
	flag("quota_cursor_limit", IsQuotaCursorLimit(text))
	flag("bg_wait", IsBgWait(text))
	add("meter", MeterLine(text))
	flag("subrow", HasSubrow(text))
	flag("pi_live_turn", PiLiveTurn(text))
	flag("pi_working_row", PiWorkingRow(text))
	flag("pi_working_label", PiWorkingLabel(text))
	flag("pi_idle_box", PiIdleBox(text))
	box("box_claude", "claude")
	box("box_any", "any")
	flag("claude_idle_box.own0.plain", ClaudeIdleBox(text, "", false))
	flag("claude_idle_box.own0.color", ClaudeIdleBox(text, colored, false))
	flag("claude_idle_box.own1.plain", ClaudeIdleBox(text, "", true))
	flag("claude_idle_box.own1.color", ClaudeIdleBox(text, colored, true))
	idle("idle_reason.skip0.plain", "", false)
	idle("idle_reason.skip0.color", colored, false)
	idle("idle_reason.skip1.plain", "", true)
	idle("idle_reason.skip1.color", colored, true)
	return r
}

// readFrame reads a fixture the way bash's $(cat file) does.
func readFrame(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

// corpus lists the plain frames under testdata/frames; a <name>.color.txt twin
// is the frame's colored capture.
func corpus(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("testdata/frames/*.txt")
	if err != nil {
		t.Fatal(err)
	}
	var plain []string
	for _, p := range all {
		if !strings.HasSuffix(p, ".color.txt") {
			plain = append(plain, p)
		}
	}
	if len(plain) == 0 {
		t.Fatal("no frames under testdata/frames")
	}
	return plain
}

func coloredFor(t *testing.T, path, text string) string {
	t.Helper()
	cf := strings.TrimSuffix(path, ".txt") + ".color.txt"
	if _, err := os.Stat(cf); err != nil {
		return text
	}
	return readFrame(t, cf)
}

// extractFunc returns the column-0 `name() {` ... `}` block of src.
func extractFunc(src, name string) (string, bool) {
	var out []string
	in := false
	for _, l := range strings.Split(src, "\n") {
		if !in && l != name+"() {" {
			continue
		}
		in = true
		out = append(out, l)
		if l == "}" {
			return strings.Join(out, "\n") + "\n", true
		}
	}
	return "", false
}

// bashScript writes the bash program that defines the classifier from
// crew.sh and runs the driver.
func bashScript(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	src, err := os.ReadFile(crewScript)
	if os.IsNotExist(err) {
		t.Skip("adapters/core/crew.sh not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, fn := range []string{"_frame_classifier", "_pane_idle_reason"} {
		body, ok := extractFunc(string(src), fn)
		if !ok {
			t.Fatalf("%s not found in %s", fn, crewScript)
		}
		sb.WriteString(body)
	}
	sb.WriteString(driver)
	path := filepath.Join(t.TempDir(), "drift.bash")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// engineKeys are the results that depend on the engine.
var engineKeys = map[string]bool{"permission_prompt": true, "prompt": true}

// bashResults runs the driver script over files in chunks, concurrently, and
// returns each frame's results keyed by file name, then result key.
func bashResults(script, engine, mode string, files []string) (map[string]map[string]string, error) {
	isBgWait, err := filepath.Abs("testdata/is_bg_wait.bash")
	if err != nil {
		return nil, err
	}
	chunk := max(1, (len(files)+runtime.NumCPU()-1)/runtime.NumCPU())
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  = map[string]map[string]string{}
		rerr error
	)
	for lo := 0; lo < len(files); lo += chunk {
		part := files[lo:min(lo+chunk, len(files))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("bash", append([]string{script, engine, mode}, part...)...)
			cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "ISBGWAIT="+isBgWait)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rerr = fmt.Errorf("bash driver (%s %s): %w\n%s", engine, mode, err, stderr.String())
				return
			}
			cur := ""
			for _, l := range strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n") {
				if name, ok := strings.CutPrefix(l, "@@ "); ok {
					cur = name
					out[cur] = map[string]string{}
					continue
				}
				k, v, _ := strings.Cut(l, "\t")
				out[cur][k] = v
			}
		}()
	}
	wg.Wait()
	return out, rerr
}

// bashFor runs the full driver under claude and the engine-dependent part
// under the other engines.
func bashFor(t *testing.T, files []string) map[string]map[string]map[string]string {
	t.Helper()
	script := bashScript(t)
	bash := map[string]map[string]map[string]string{}
	for _, engine := range engines {
		mode := "engine"
		if engine == "claude" {
			mode = "full"
		}
		res, err := bashResults(script, engine, mode, files)
		if err != nil {
			t.Fatal(err)
		}
		bash[engine] = res
	}
	return bash
}

// want is bash's result for key under engine.
func want(bash map[string]map[string]map[string]string, engine, frame, key string) (string, bool) {
	e := "claude"
	if engineKeys[key] {
		e = engine
	}
	v, ok := bash[e][frame][key]
	return v, ok
}

func TestDriftAgainstBash(t *testing.T) {
	frames := corpus(t)
	bash := bashFor(t, frames)

	const maxReported = 40
	mismatches, compared := 0, 0
	for _, engine := range engines {
		for _, path := range frames {
			name := filepath.Base(path)
			text := readFrame(t, path)
			for _, g := range results(engine, text, coloredFor(t, path, text)) {
				w, ok := want(bash, engine, name, g.key)
				if !ok {
					t.Fatalf("%s / %s / %s: bash printed no result", engine, name, g.key)
				}
				compared++
				if g.val == w {
					continue
				}
				if mismatches++; mismatches <= maxReported {
					t.Errorf("%s / %s / %s: Go %q, bash %q", engine, name, g.key, g.val, w)
				}
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("%d of %d predicate results differ from bash across %d frames x %d engines (first %d shown)",
			mismatches, compared, len(frames), len(engines), min(mismatches, maxReported))
	}
}

// row is one focused expectation: key's result for text under engine. The
// colored capture defaults to text, as in the corpus. TestRowsMatchBash proves
// every want against the bash original.
type row struct {
	name, engine, key, text, colored, want string
}

func lines(l ...string) string { return strings.Join(l, "\n") }

func filler(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s %d", prefix, i+1)
	}
	return out
}

const (
	rule       = "────────────────────"
	hookReview = "Hooks need review\n  1 hook is new or changed.\n  Hooks can run outside the sandbox after you trust them.\n› 1. Review hooks  2. Trust all and continue  3. Continue without trusting"
	permAsk    = "Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel · Tab to amend"
	trust      = "> 1. Yes, I trust this folder\n2. No, exit\nEnter to confirm"
	meterRow   = "✳ Perusing… (1m 2s · ↓ 3.1k tokens · thinking)"
	subRow     = "  ◯ general-purpose  Revise spec per critic                              3m 29s · ↓ 71.5k tokens"
	doneRow    = "✻ Churned for 36s · done 11:20 AM"
	modeRow    = "  -- INSERT -- ⏵⏵ auto mode on · ← for agents"
)

func rows() []row {
	var r []row
	add := func(name, engine, key, text, colored, want string) {
		r = append(r, row{name, engine, key, text, colored, want})
	}
	const claude = "claude"

	add("codex hook review verbatim", "codex", "codex_hook_review", hookReview, "", "1")
	add("codex hook review across blank lines", "codex", "codex_hook_review",
		lines("scrollback", "", "Hooks need review", "", "  1 hook is new or changed.", "  Hooks can run outside the sandbox after you trust them.", "", "› 1. Review hooks  2. Trust all and continue  3. Continue without trusting"), "", "1")
	add("codex hook review with a trailing line", "codex", "codex_hook_review", hookReview+"\n  press enter", "", "0")
	add("codex hook review reordered", "codex", "codex_hook_review",
		lines("Hooks need review", "  Hooks can run outside the sandbox after you trust them.", "  1 hook is new or changed.", "› 1. Review hooks  2. Trust all and continue  3. Continue without trusting"), "", "0")

	add("prompt: confirm footer last", claude, "prompt", trust, "", "1")
	add("prompt: select footer", claude, "prompt", lines("  1. Yes", "Enter to select · Esc to cancel"), "", "1")
	add("prompt: footer not last", claude, "prompt", trust+"\n"+modeRow, "", "0")
	add("prompt: option 6 lines above the footer", claude, "prompt",
		lines(append(append([]string{"  1. Yes"}, filler("line", 5)...), "Enter to confirm")...), "", "1")
	add("prompt: option 7 lines above the footer", claude, "prompt",
		lines(append(append([]string{"  1. Yes"}, filler("line", 6)...), "Enter to confirm")...), "", "0")
	add("prompt: codex defers to the hook-review shape", "codex", "prompt", trust, "", "0")
	add("prompt: codex hook review", "codex", "prompt", hookReview, "", "1")
	add("prompt: pi uses the claude shape", "pi", "prompt", trust, "", "1")

	add("permission: claude dialog", claude, "permission_prompt", permAsk, "", "1")
	add("permission: other engines never match", "pi", "permission_prompt", permAsk, "", "0")
	add("permission: footer scrolled up", claude, "permission_prompt", permAsk+"\n"+modeRow, "", "0")
	add("permission: question 9 rows above the footer", claude, "permission_prompt",
		lines(append(append([]string{"Do you want to proceed?"}, filler("line", 7)...), " ❯ 1. Yes", " Esc to cancel · Tab to amend")...), "", "1")
	add("permission: question 10 rows above the footer", claude, "permission_prompt",
		lines(append(append([]string{"Do you want to proceed?"}, filler("line", 8)...), " ❯ 1. Yes", " Esc to cancel · Tab to amend")...), "", "0")
	add("permission: no option row", claude, "permission_prompt", lines("Do you want to proceed?", " Esc to cancel · Tab to amend"), "", "0")

	quota := "What do you want to do?\n❯ 1. Stop and wait for limit to reset\n  2. Upgrade your plan\nEnter to select"
	add("quota prompt phrase in the last 12 rows", claude, "quota_prompt", quota, "", "1")
	add("quota prompt phrase 11 rows above the end", claude, "quota_prompt",
		lines(append([]string{"Stop and wait for limit to reset"}, filler("line", 11)...)...), "", "1")
	add("quota prompt phrase 12 rows above the end", claude, "quota_prompt",
		lines(append([]string{"Stop and wait for limit to reset"}, filler("line", 12)...)...), "", "0")

	session := func(gap int) string {
		return lines(append(append([]string{"  ⎿  You've hit your session limit · resets 7pm", "     /upgrade to increase your usage limit.", "  ⚠ /low-priority · uses your weekly limit"}, filler("line", gap)...), "end")...)
	}
	add("session limit anchors within the tail", claude, "quota_session_limit", session(4), "", "1")
	add("session limit weekly hint 7 rows from the end", claude, "quota_session_limit", session(5), "", "0")
	add("session limit one anchor missing", claude, "quota_session_limit", lines("  ⎿  You've hit your session limit", "end"), "", "0")

	add("cursor limit both anchors", "cursor", "quota_cursor_limit",
		lines("  Error: You've reached your monthly usage limit", "  fallbackModel:", "  spendLimitHit: true", "  chatMessage:"), "", "1")
	add("cursor limit error line alone", "cursor", "quota_cursor_limit", "  Error: You've reached your monthly usage limit", "", "0")
	add("cursor limit anchors 12 rows deep", "cursor", "quota_cursor_limit",
		lines(append([]string{"  Error: You've reached your monthly usage limit", "  spendLimitHit: true"}, filler("line", 11)...)...), "", "0")

	add("bg wait: done marker with a running shell", claude, "bg_wait", "✻ Crunched for 1m 12s · done 11:16 AM · 1 shell still running", "", "1")
	add("bg wait: single-digit hour", claude, "bg_wait", "✻ Crunched for 1m · done 9:05 AM · 2 shells still running", "", "1")
	add("bg wait: shell count in the mode line", claude, "bg_wait", lines(doneRow, "  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents"), "", "1")
	add("bg wait: zero shells", claude, "bg_wait", "✻ Crunched for 1m · done 11:16 AM · 0 shells still running", "", "0")
	add("bg wait: leading-zero count", claude, "bg_wait", "✻ Crunched for 1m · done 11:16 AM · 01 shell still running", "", "0")
	add("bg wait: ten shells", claude, "bg_wait", "✻ Crunched for 1m · done 11:16 AM · 10 shells still running", "", "1")
	add("bg wait: live spinner under a stale done line", claude, "bg_wait", lines("✻ Crunched for 1m · done 11:16 AM · 1 shell still running", "✻ Pondering… (3s)"), "", "0")
	add("bg wait: live meter under a stale done line", claude, "bg_wait", lines("✻ Crunched for 1m · done 11:16 AM · 1 shell still running", meterRow), "", "0")
	add("bg wait: done marker 9 rows up", claude, "bg_wait",
		lines(append([]string{"✻ Crunched for 1m · done 11:16 AM · 1 shell still running"}, filler("line", 8)...)...), "", "0")

	add("meter: plain", claude, "meter", meterRow, "", escaper.Replace(meterRow))
	add("meter: the last of two", claude, "meter", lines("Reading… (3s · ↓ 1.0k tokens)", meterRow), "", escaper.Replace(meterRow))
	add("meter: hours form", claude, "meter", "Considering… (1h 20m · ↓ 28.0k tokens)", "", "Considering… (1h 20m · ↓ 28.0k tokens)")
	add("meter: accented letter before the ellipsis", claude, "meter", "✳ Pérusing… (1m 2s · ↓ 3.1k tokens)", "", "")
	add("meter: non-ASCII spinner row", claude, "meter", "é… (3s · ↓ 1.2k tokens)", "", "")
	add("meter: ascii-fied transcription", claude, "meter", "Considering... 1h 20m - 28.0k tokens", "", "")
	add("meter: circled digit is not alnum", claude, "meter", "① Reading… (3s · ↓ 1.2k tokens", "", "① Reading… (3s · ↓ 1.2k tokens")

	add("subrow: live subagent row", claude, "subrow", subRow, "", "1")
	add("subrow: finished-subagent history line", claude, "subrow", "  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)", "", "0")

	add("pi live turn: spinner on the top rule", "pi", "pi_live_turn", lines("── ⠼ Working ──────────", "", rule), "", "1")
	add("pi live turn: pure rules", "pi", "pi_live_turn", lines(rule, "", rule), "", "0")
	add("pi live turn: vim INSERT on the lower rule", "pi", "pi_live_turn", lines(rule, "", rule+" INSERT"), "", "0")
	add("pi live turn: vim pending command on the lower rule", "pi", "pi_live_turn", lines(rule, "", rule+" NORMAL 3dw_"), "", "0")
	add("pi live turn: scroll indicator", "pi", "pi_live_turn", lines("── ↑ 3 more "+rule, "x", "── ↓ 2 more "+rule), "", "0")
	add("pi live turn: other rule text", "pi", "pi_live_turn", lines(rule, "", rule+" thinking"), "", "1")
	add("pi live turn: rule 31 rows up is out of the window", "pi", "pi_live_turn",
		lines(append([]string{"── ⠼ Working ──────────"}, filler("line", 30)...)...), "", "0")

	add("pi working row: braille row", "pi", "pi_working_row", " ⠸ Working", "", "1")
	add("pi working row: no leading space", "pi", "pi_working_row", "⠸ Working", "", "1")
	add("pi working row: no glyph", "pi", "pi_working_row", " Working", "", "0")
	add("pi working row: glyph not first", "pi", "pi_working_row", "x ⠸ Working", "", "0")
	add("pi working row: non-braille symbol", "pi", "pi_working_row", " ✻ Working", "", "0")

	add("pi working label: rule-borne", "pi", "pi_working_label", lines("── ⠼ Working ──────────", "", rule, "~/git/x", "stats"), "", "1")
	add("pi working label: row above the box", "pi", "pi_working_label", lines(" ⠸ Working", "", rule, "", rule, "~/git/x", "stats"), "", "1")
	add("pi working label: row with no box", "pi", "pi_working_label", " ⠸ Working", "", "0")
	add("pi working label: stale row far above the box", "pi", "pi_working_label",
		lines(append(append([]string{" ⠸ Working"}, filler("line", 8)...), rule, "", rule, "~/git/x", "stats")...), "", "0")

	add("pi idle box: empty editor", "pi", "pi_idle_box", lines(" pi v0.87.1", rule, "", rule, "~/git/x", "stats"), "", "1")
	add("pi idle box: vim lower rule", "pi", "pi_idle_box", lines(" pi v1.0.0", rule, "hello", rule+" INSERT", "~/git/x", "stats"), "", "1")
	add("pi idle box: vim working row above", "pi", "pi_idle_box", lines(" ⠸ Working", rule, "", rule+" INSERT", "~/git/x", "stats"), "", "0")
	add("pi idle box: working spinner on the rule", "pi", "pi_idle_box", lines("── ⠼ Working ──────────", "", rule, "~/git/x", "stats"), "", "0")
	add("pi idle box: bare shell", "pi", "pi_idle_box", lines("noams@g6 ~/git/x (main)", "$"), "", "0")
	add("pi idle box: numbered option in the editor", "pi", "pi_idle_box", lines(rule, "  1. Yes", rule, "cwd", "stats"), "", "0")

	box := lines(doneRow, rule, "❯ hi", rule, modeRow)
	add("box rows: claude row and the rows above", claude, "box_claude.out", box, "", escaper.Replace(lines("❯ hi", doneRow)))
	add("box rows: claude row regex rejects a plain editor", claude, "box_claude.rc", lines(rule, "text", rule, "status"), "", "1")
	add("box rows: any row regex accepts a plain editor", claude, "box_any.rc", lines(rule, "text", rule, "status"), "", "0")
	add("box rows: empty box with the any regex", claude, "box_any.out", lines("above", rule, rule, "status"), "", "\\n"+"above")
	add("box rows: nothing after the lower rule", claude, "box_claude.rc", lines(rule, "❯", rule), "", "1")
	add("box rows: six rows after the lower rule", claude, "box_claude.rc", lines(rule, "❯", rule, "1", "2", "3", "4", "5", "6"), "", "1")
	add("box rows: five rows after the lower rule", claude, "box_claude.rc", lines(rule, "❯", rule, "1", "2", "3", "4", "5"), "", "0")
	add("box rows: nbsp after the prompt glyph", claude, "box_claude.rc", lines(rule, "❯ text", rule, "status"), "", "0")
	add("box rows: only seven rows above are reported", claude, "box_claude.out",
		lines(append(filler("up", 9), rule, "❯", rule, "status")...), "",
		escaper.Replace(lines("❯", "up 9", "up 8", "up 7", "up 6", "up 5", "up 4", "up 3")))

	ghost := "\x1b[39m❯ \x1b[2mTry \"a test\"\x1b[0m"
	draft := "\x1b[39m❯ hello draft"
	ghostPlain := lines(doneRow, rule, "❯ Try \"a test\"", rule, modeRow)
	add("claude idle: ghost suggestion, dim SGR", claude, "claude_idle_box.own0.color", ghostPlain, lines(doneRow, rule, ghost, rule, modeRow), "1")
	add("claude idle: real draft, no dim SGR", claude, "claude_idle_box.own0.color", ghostPlain, lines(doneRow, rule, draft, rule, modeRow), "0")
	add("claude idle: draft without a colored capture fails closed", claude, "claude_idle_box.own0.plain", ghostPlain, "", "0")
	add("claude idle: own text counts as idle", claude, "claude_idle_box.own1.plain", ghostPlain, "", "1")
	add("claude idle: empty box", claude, "claude_idle_box.own0.plain", lines(doneRow, rule, "❯", rule, modeRow), "", "1")
	add("claude idle: live meter vetoes even own text", claude, "claude_idle_box.own1.plain", lines(meterRow, rule, "❯", rule, modeRow), "", "0")
	add("claude idle: esc to interrupt vetoes", claude, "claude_idle_box.own0.plain", lines("esc to interrupt", rule, "❯", rule, modeRow), "", "0")
	add("claude idle: spinner row above the box vetoes", claude, "claude_idle_box.own0.plain", lines("✻ Hatching…", rule, "❯", rule, modeRow), "", "0")
	add("claude idle: accented spinner row does not veto", claude, "claude_idle_box.own0.plain", lines("é… (3s)", rule, "❯", rule, modeRow), "", "1")
	add("claude idle: numbered option row", claude, "claude_idle_box.own1.plain", lines(rule, "❯ 1. Yes", rule, modeRow), "", "0")
	add("claude idle: pi-shaped box without the glyph", claude, "claude_idle_box.own1.plain", lines(rule, "text", rule, modeRow), "", "0")

	idleFrame := lines("  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)", doneRow, rule, "❯", rule, modeRow)
	add("idle reason: finished idle turn", claude, "idle_reason.skip0.plain", idleFrame, "", "rc=0 reason=")
	add("idle reason: prompt on screen", claude, "idle_reason.skip0.plain", trust, "", "rc=1 reason=prompt on screen")
	add("idle reason: permission dialog", claude, "idle_reason.skip0.plain", permAsk, "", "rc=1 reason=prompt on screen")
	add("idle reason: background shell", claude, "idle_reason.skip0.plain",
		lines(doneRow+" · 1 shell still running", rule, "❯", rule, modeRow), "", "rc=1 reason=background shell or monitor still running")
	add("idle reason: background shell skipped", claude, "idle_reason.skip1.plain",
		lines(doneRow+" · 1 shell still running", rule, "❯", rule, modeRow), "", "rc=0 reason=")
	add("idle reason: background monitor", claude, "idle_reason.skip0.plain",
		lines("  2 monitors · working", rule, "❯", rule, modeRow), "", "rc=1 reason=background shell or monitor still running")
	add("idle reason: live turn", claude, "idle_reason.skip0.plain",
		lines(meterRow, rule, "❯", rule, modeRow), "", "rc=1 reason=live turn, unsent input, or no idle input box")
	add("idle reason: no finished-turn marker", claude, "idle_reason.skip0.plain",
		lines("✻ Welcome to Claude Code!", rule, "❯", rule, "  ? for shortcuts"), "", "rc=1 reason=no finished-turn marker above the input box")
	add("idle reason: engine is forced to claude", "codex", "idle_reason.skip0.plain", trust, "", "rc=1 reason=prompt on screen")
	return r
}

func TestRows(t *testing.T) {
	for _, c := range rows() {
		t.Run(c.name, func(t *testing.T) {
			colored := c.colored
			if colored == "" {
				colored = c.text
			}
			if got, ok := lookup(results(c.engine, c.text, colored), c.key); !ok || got != c.want {
				t.Errorf("%s: got %q, want %q", c.key, got, c.want)
			}
		})
	}
}

// TestRowsMatchBash keeps every want above honest: the bash original must
// produce it.
func TestRowsMatchBash(t *testing.T) {
	script := bashScript(t)
	dir := t.TempDir()
	byEngine := map[string][]string{}
	for i, c := range rows() {
		p := filepath.Join(dir, fmt.Sprintf("%03d.txt", i))
		if err := os.WriteFile(p, []byte(c.text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if c.colored != "" {
			if err := os.WriteFile(strings.TrimSuffix(p, ".txt")+".color.txt", []byte(c.colored+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		byEngine[c.engine] = append(byEngine[c.engine], p)
	}
	bash := map[string]map[string]map[string]string{}
	for engine, files := range byEngine {
		res, err := bashResults(script, engine, "full", files)
		if err != nil {
			t.Fatal(err)
		}
		bash[engine] = res
	}
	for i, c := range rows() {
		got, ok := bash[c.engine][fmt.Sprintf("%03d.txt", i)][c.key]
		if !ok {
			t.Errorf("%s: bash printed no %s", c.name, c.key)
			continue
		}
		if got != c.want {
			t.Errorf("%s: bash gives %q for %s, want %q", c.name, got, c.key, c.want)
		}
	}
}

func lookup(r []kv, key string) (string, bool) {
	for _, e := range r {
		if e.key == key {
			return e.val, true
		}
	}
	return "", false
}
