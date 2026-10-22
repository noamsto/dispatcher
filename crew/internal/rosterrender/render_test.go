package rosterrender

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/identity"
)

// The diagram, from the bus to the file: the bats fixtures' `_rr_seed` and the
// `roster-render:` cases that read the D2 text its `--once` writes.

// utc pins the process zone, which is what the diagram's `strflocaltime` reads —
// the "since HH:MM" and "until MM-DD HH:MM" stamps. The bats fixtures exported
// TZ=UTC; gojq reads time.Local per call, so the pin lands for the render.
func utc(t *testing.T) {
	t.Helper()
	got := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = got })
}

// busDoc is one bus line, in the bus's own field names.
func busDoc(fields map[string]any) string {
	text, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(text)
}

// dispatchEvent is `_rr_dispatch`: the line a worker's launch leaves, carrying the
// identity and the task it was given.
func dispatchEvent(branch string, ts int64, name, color, tmuxColor, title, tier, engine, model string) string {
	return busDoc(map[string]any{
		"ts": ts, "crew_id": "c1", "kind": "dispatch", "branch": branch, "session": "s1",
		"worker_id": "worker:" + branch + "#s1", "engine": engine, "model": model,
		"tier": tier, "title": title, "name": name, "color": color, "tmux": tmuxColor,
	})
}

// dispatchBase is `_rr_dispatch` with the `--base` that made it a stacked layer.
func dispatchBase(branch string, ts int64, name, color, tmuxColor, title, tier, engine, model, base string) string {
	fields := map[string]any{
		"ts": ts, "crew_id": "c1", "kind": "dispatch", "branch": branch, "session": "s1",
		"worker_id": "worker:" + branch + "#s1", "engine": engine, "model": model,
		"tier": tier, "title": title, "name": name, "color": color, "tmux": tmuxColor,
		"base": base,
	}
	return busDoc(fields)
}

// statusEvent is `_rr_status`: a state and the three parts a status may leave off
// — the detail, the source that raised it, the PR url — which the fixture's jq
// dropped from the body rather than writing empty.
func statusEvent(from string, ts int64, state, detail, source, pr string) string {
	body := map[string]any{"state": state}
	if detail != "" {
		body["detail"] = detail
	}
	if source != "" {
		body["source"] = source
	}
	if pr != "" {
		body["pr_url"] = pr
	}
	return busDoc(map[string]any{
		"ts": ts, "crew_id": "c1", "from": from, "to": "dispatcher:c1", "kind": "status", "body": body,
	})
}

// holdEvent is `seed_hold`: a hold row shaped like `crew hold add`'s own — the
// hold itself is a JSON *string* in the msg body — waiting on an engine until
// `resetsAt`.
func holdEvent(id string, resetsAt int64, branch, ref, waitEngine, taskEngine, title string) string {
	hold, err := json.Marshal(map[string]any{
		"id":   id,
		"wait": map[string]any{"engine": waitEngine, "window": "5h", "resets_at": resetsAt},
		"task": map[string]any{
			"ref": ref, "branch": branch, "tier": "standard", "engine": taskEngine,
			"model": "sonnet", "effort": "medium", "title": title,
		},
	})
	if err != nil {
		panic(err)
	}
	return busDoc(map[string]any{
		"ts": resetsAt * 1000, "crew_id": "c1", "kind": "msg",
		"from": "dispatcher:c1", "to": "hold:c1", "body": string(hold),
	})
}

// seedBus is the bats fixture's `_rr_seed`: four workers — feat/3-c stacked on
// feat/1-a with two sessions and a watchdog block, feat/4-d dispatched but silent
// apart from a role row — one hold, and one finished branch whose worktree still
// waits for `crew reap`. Every branch the bus names is stamped on a window, which
// is what keeps a row live however old its last word is.
func (l *loop) seedBus(t *testing.T) {
	t.Helper()
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "atlas", "blue", "colour32", "Bravo task", "standard", "codex", "gpt-5"),
		dispatchBase("feat/3-c", 1791360120000, "nova", "magenta", "colour127", "Charlie task", "deep", "claude", "opus", "feat/1-a"),
		dispatchEvent("feat/4-d", 1791360180000, "ember", "orange", "colour130", "Delta task", "quick", "cursor", "composer"),
		statusEvent("role:feat/4-d:reviewer", 1791360240000, "working", "", "", ""),
		statusEvent("worker:feat/3-c#s1", 1791360300000, "working", "", "", ""),
		statusEvent("worker:feat/1-a#s1", 1791360600000, "working", "execute: tests", "", ""),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "plan-critic r2", "", ""),
		statusEvent("worker:feat/2-b#s1", 1791361800000, "pr_open", "", "", "https://github.com/o/r/pull/124"),
		statusEvent("worker:feat/2-b#s1", 1791362400000, "done", "", "", ""),
		statusEvent("worker:feat/3-c#s2", 1791363000000, "blocked", "quiet: %204", "watchdog", ""),
		holdEvent("h-1", 1791374400, "feat/5-e", "r-5", "claude", "codex", "Echo task"),
	)
	l.windows = "feat/1-a\nfeat/2-b\nfeat/3-c\nfeat/4-d"
	l.worktrees(t, "feat/2-b")
	// The role listing is keyed on the crew dir the pane was stamped with, which is
	// `bus.Paths.Dir` — and a `lead` row is the worker itself, never a role.
	l.panes = strings.Join([]string{
		l.paths.Dir + "\t" + l.crew + "\tfeat/1-a\tspec-critic\tidle\t",
		l.paths.Dir + "\t" + l.crew + "\tfeat/1-a\tlead\tworking\t",
		l.paths.Dir + "\tc2\tfeat/1-a\tother-role\tidle\t",
	}, "\n") + "\n"
	l.rolesJSON(t, "feat/1-a", `{"spec-critic":{"agent":"claude"}}`)
}

// crewed is `_rr_recrew`: the same bus, spoken of as another crew's. The fixtures
// write c1 because that is the id every other case uses.
func (l *loop) crewed(events []string) []string {
	if l.crew == "" || l.crew == "c1" {
		return events
	}
	out := make([]string, len(events))
	for i, e := range events {
		e = strings.ReplaceAll(e, `"crew_id":"c1"`, `"crew_id":"`+l.crew+`"`)
		out[i] = strings.ReplaceAll(e, `"dispatcher:c1"`, `"dispatcher:`+l.crew+`"`)
	}
	return out
}

// appendBus adds to the bus the way a live crew grows one.
func (l *loop) appendBus(t *testing.T, events ...string) {
	t.Helper()
	f, err := os.OpenFile(l.paths.Log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, err := f.WriteString(e + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// worktrees is the `git worktree list --porcelain` a pass reads: one worktree per
// branch, the evidence a finished row needs to stay drawn. None at all is the
// `newLoop` default.
func (l *loop) worktrees(t *testing.T, branches ...string) {
	t.Helper()
	out := ""
	for _, b := range branches {
		out += "worktree /wt/" + strings.ReplaceAll(b, "/", "-") + "\nHEAD 0000000000000000000000000000000000000000\nbranch refs/heads/" + b + "\n\n"
	}
	l.opts.Git = &gittest.Fake{Func: func(string, []string) (string, error) { return out, nil }}
}

// rolesJSON writes the file a role pane's engine is read from, beside the branch's
// artifacts dir.
func (l *loop) rolesJSON(t *testing.T, branch, text string) {
	t.Helper()
	dir := filepath.Join(l.paths.Dir, "artifacts", branch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// aeyePublishHelp is the `aeye --help` that lists publish-diagram, the only answer
// the renderer publishes on.
const aeyePublishHelp = "Usage: aeye [command]\n\nCommands:\n  render <file>\n  publish-diagram <file> [flags]\n  open\n"

// seedDiagram is the fixture bus drawn: the exact D2 text the bats case diffed its
// file against.
const seedDiagram = `title: "Crew roster" {near: top-center; shape: text}
legend: "2 active · 1 blocked · 1 done" {near: bottom-center; shape: text}
dispatcher: "dispatcher" {style.bold: true}
w1: "sage\nAlpha task\nstandard·claude·sonnet\nworking · plan-critic r2 (loop) · since 08:20" {
  grid-rows: 1
  style: {fill: transparent; stroke: "#76a76b"; stroke-width: 3; font-size: 16}
  r1: "spec-critic\nclaude · idle"
}
dispatcher -> w1
w2: "atlas\nBravo task\nstandard·codex·gpt-5\ndone · since 08:40" {
  style: {fill: transparent; stroke: "#6b8fd6"; stroke-width: 3; font-size: 16}
}
dispatcher -> w2
w2_pr: "PR #124" {shape: page}
w2 -> w2_pr: "#124"
w3: "nova\nCharlie task\ndeep·claude·opus\nblocked (watchdog) · quiet: %204 · since 08:50 · 2 sessions" {
  style: {fill: transparent; stroke: "#c46bb5"; stroke-width: 3; font-size: 16; stroke-dash: 3}
}
dispatcher -> w3
w4: "ember\nDelta task\nquick·cursor·composer\ndispatched · since 08:03" {
  style: {fill: transparent; stroke: "#d49a62"; stroke-width: 3; font-size: 16}
}
dispatcher -> w4
w3 -> w1: "stacked on"
h1: "hold r-5\nwaiting on claude 5h\nuntil 10-07 12:00" {shape: hexagon}
dispatcher -> h1: {style.stroke-dash: 3}
`

// TestOnceRendersTheFixtureBus is the diagram end to end: the bus the fixture
// plants, one `--once`, and every byte of the file it writes — the palette stroke,
// the legend's counts, the stacked edge that survives a re-dispatch naming no
// base, the PR node, the role pane inside its worker, and the hold.
func TestOnceRendersTheFixtureBus(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	// A re-dispatch without --base, older than feat/3-c's last status: the
	// stacked edge still comes from the earlier dispatch that named the base.
	l.appendBus(t,
		dispatchEvent("feat/3-c", 1791362700000, "nova", "magenta", "colour127", "Charlie task", "deep", "claude", "opus"))

	code, stderr := l.once(Request{})
	if code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if got := l.diagram(); got != seedDiagram {
		t.Errorf("diagram =\n%s\nwant\n%s", got, seedDiagram)
	}
}

// TestPRNodeIsLabelledByNumberAndCarriesNoURL is the carousel's one link out: the
// node says `PR #124` and the edge says `#124`, while the url itself never reaches
// the file a screenshot is drawn from. A pr_url with no /pull/N is still a PR,
// just one with no number to show.
func TestPRNodeIsLabelledByNumberAndCarriesNoURL(t *testing.T) {
	t.Run("numbered", func(t *testing.T) {
		l := newLoop(t)
		l.seedBus(t)
		if code, stderr := l.once(Request{}); code != exitOK {
			t.Fatalf("code = %d, stderr %q", code, stderr)
		}
		text := l.diagram()
		lines := diagramLines(text)
		for _, want := range []string{"w2_pr: \"PR #124\" {shape: page}", "w2 -> w2_pr: \"#124\""} {
			if !slices.Contains(lines, want) {
				t.Errorf("no line %q in\n%s", want, text)
			}
		}
		if strings.Contains(text, "github.com") {
			t.Errorf("the url reached the diagram:\n%s", text)
		}
	})

	t.Run("unnumbered", func(t *testing.T) {
		l := newLoop(t)
		utc(t)
		l.seed(t,
			dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
			statusEvent("worker:feat/1-a#s1", 1791361200000, "pr_open", "", "", "https://example.com/x"),
		)
		l.windows = "feat/1-a"

		if code, stderr := l.once(Request{}); code != exitOK {
			t.Fatalf("code = %d, stderr %q", code, stderr)
		}
		text := l.diagram()
		lines := diagramLines(text)
		for _, want := range []string{"w1_pr: \"PR\" {shape: page}", "w1 -> w1_pr"} {
			if !slices.Contains(lines, want) {
				t.Errorf("no line %q in\n%s", want, text)
			}
		}
	})
}

// TestLongTitleIsCutAtAWordBoundary is the worker box's own width: a title of
// words is cut between them, dropping the partial word, and one unbroken word is
// cut mid-letter because there is no boundary to land on.
func TestLongTitleIsCutAtAWordBoundary(t *testing.T) {
	long := "word alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho"
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", long, "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "atlas", "blue", "colour32", strings.Repeat("x", 100), "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "", "", ""),
		statusEvent("worker:feat/2-b#s1", 1791361800000, "working", "", "", ""),
	)
	l.windows = "feat/1-a\nfeat/2-b"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.diagram()
	// Cut lands mid-"omicron": the partial word is dropped, never half a word.
	if !strings.Contains(text, `\nword alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi…\n`) {
		t.Errorf("the word-boundary cut is missing:\n%s", text)
	}
	if !strings.Contains(text, `\n`+strings.Repeat("x", 79)+`…\n`) {
		t.Errorf("the hard cut of one unbroken word is missing:\n%s", text)
	}
}

// TestHostileTextStaysInsideQuotedLabels is the injection row: a title, a detail,
// a hold id, a role name and a palette color all arrive from the bus or from tmux,
// and none of them may leave a quoted label, add a style line, or put a control
// byte in the file.
func TestHostileTextStaysInsideQuotedLabels(t *testing.T) {
	l := newLoop(t)
	utc(t)
	// The bats wrote this title with $'…': one real newline, one backslash, and
	// the `$d` of an expansion that never happens.
	title := "a\"b\\c$d |md ...@import x } {\n# e"
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "atlas", "red; fill: blue", "colour32", title, "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "sage", "green", "colour28", "Plain", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "go ${x}\x01end", "", ""),
		statusEvent("worker:feat/2-b#s1", 1791361800000, "working", "", "", ""),
		holdEvent("h-1", 1791374400, "feat/5-e", `h"1$`, "claude", "codex", "Echo task"),
	)
	// An engine that is not one, and a roles.json that is a symlink to a file the
	// worker can read: both answer "?", and neither leaves its own text behind.
	l.rolesJSON(t, "feat/1-a", `{"spec-critic":{"agent":"evil\""}}`)
	elsewhere := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(elsewhere, []byte(`{"critic":{"agent":"claude"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.paths.Dir, "artifacts", "feat/2-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(l.paths.Dir, "artifacts", "feat/2-b", "roles.json")); err != nil {
		t.Fatal(err)
	}
	l.windows = "feat/1-a\nfeat/2-b"
	l.panes = strings.Join([]string{
		l.paths.Dir + "\tc1\tfeat/1-a\tr\";x\tidle\t",
		l.paths.Dir + "\tc1\tfeat/1-a\tspec-critic\tidle\t",
		l.paths.Dir + "\tc1\tfeat/2-b\tcritic\tidle\t",
	}, "\n") + "\n"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.diagram()
	lines := diagramLines(text)
	for _, want := range []string{
		`w1: "atlas\na\"b\\c\$d |md ...@import x } {\n# e\nstandard·claude·sonnet\nworking · go \${x}end · since 08:20" {`,
		`  style: {fill: transparent; stroke-width: 3; font-size: 16}`,
		`  r1: "spec-critic\n? · idle"`,
		`w2: "sage\nPlain\nstandard·claude·sonnet\nworking · since 08:30" {`,
		`  style: {fill: transparent; stroke: "#76a76b"; stroke-width: 3; font-size: 16}`,
		`  r1: "critic\n? · idle"`,
		"h1: \"hold h\\\"1\\$\\nwaiting on claude 5h\\nuntil 10-07 12:00\" {shape: hexagon}",
	} {
		// Whole lines, the way `grep -qFx` read them: a fragment of a longer line
		// is not the label the renderer was supposed to write.
		if !slices.Contains(lines, want) {
			t.Errorf("no line %q in\n%s", want, text)
		}
	}

	// The hostile role pane and the non-palette color leave no trace.
	for _, banned := range []string{"red", "fill: blue", ";x", "evil"} {
		if strings.Contains(text, banned) {
			t.Errorf("%q reached the diagram:\n%s", banned, text)
		}
	}
	// Only generated keys start a line, and the only control byte in the file is the
	// newline between them: a label's own breaks are the two-character escape.
	for _, line := range lines {
		if !generatedLine.MatchString(line) {
			t.Errorf("a line the renderer did not write: %q", line)
		}
	}
	if controlByte.MatchString(text) {
		t.Errorf("a control byte reached the file: %q", text)
	}
}

// controlByte is the bats `grep -qP '[\x00-\x09\x0b-\x1f]'`: every control character
// except the newline the model itself writes between label segments. A tab off the
// pane listing, or an ESC off the bus, would reach a terminal otherwise.
var controlByte = regexp.MustCompile(`[\x00-\x09\x0b-\x1f]`)

// diagramLines is the file as `grep -x` saw it: whole lines, no trailing blank.
func diagramLines(text string) []string {
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// generatedLine is one line the renderer writes: a key, an edge, or the `}` that
// closes a container. Anything else in the file came off the bus.
var generatedLine = regexp.MustCompile(
	`^(\}$|(title|legend|dispatcher|w[0-9]+(_pr)?|h[0-9]+|  (style|grid-rows|r[0-9]+)|dispatcher -> (w|h)[0-9]+|w[0-9]+ -> w[0-9]+(_pr)?)[:" {-])`)

// TestWorkerBlockLinesStayWithin80Chars is the carousel's own width limit: every
// `\n`-separated segment of a worker box, and of the role panes drawn inside it,
// fits 80 characters however long the detail is.
func TestWorkerBlockLinesStayWithin80Chars(t *testing.T) {
	l := newLoop(t)
	utc(t)
	detail := strings.Repeat("lengthy detail words ", 30) + "ending in r3"
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", detail, "watchdog", ""),
		statusEvent("worker:feat/1-a#s2", 1791361300000, "working", detail, "watchdog", ""),
	)
	l.windows = "feat/1-a"
	l.rolesJSON(t, "feat/1-a", `{"spec-critic":{"agent":"claude"}}`)
	l.panes = l.paths.Dir + "\tc1\tfeat/1-a\tspec-critic\tidle\t\n"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	// name, title, engine line, status line, then the role's two lines.
	if n := checkSegments(t, l.diagram()); n != 6 {
		t.Errorf("%d label segments, want 6", n)
	}
}

// TestLongStateWatchdogAndSessionsFitTheStatusLine is the same limit from the
// other side: a watchdog state, its detail and a dozen sessions crowded onto one
// status line.
func TestLongStateWatchdogAndSessionsFitTheStatusLine(t *testing.T) {
	l := newLoop(t)
	utc(t)
	events := []string{
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
	}
	for i := 1; i <= 12; i++ {
		events = append(events, statusEvent(fmt.Sprintf("worker:feat/1-a#s%d", i),
			int64(1791361200000+i), "abcdefghijklmnopqrstuvwxyz",
			"plan-critic revision 3 of the lengthy fix cycle r3", "watchdog", ""))
	}
	l.seed(t, events...)
	l.windows = "feat/1-a"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.diagram()
	if !strings.Contains(text, "12 sessions") {
		t.Errorf("the sessions were not counted:\n%s", text)
	}
	checkSegments(t, text)
}

// labelLine is a worker's or a role's own label line — the PR node and the hold
// are not worker blocks, and the bats check did not read them.
var labelLine = regexp.MustCompile(`^(w[0-9]+|  r[0-9]+): "`)

// checkSegments is `_rr_segments_le80`: every `\n`-separated segment of every
// worker label, counted, and no segment over 80 characters — characters, the way
// `${#seg}` and jq's `length` count them, not bytes.
func checkSegments(t *testing.T, text string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if !labelLine.MatchString(line) {
			continue
		}
		_, rest, ok := strings.Cut(line, `: "`)
		if !ok {
			continue
		}
		end := strings.LastIndex(rest, `"`)
		if end < 0 {
			continue
		}
		for _, seg := range strings.Split(rest[:end], `\n`) {
			if utf8.RuneCountInString(seg) > 80 {
				t.Errorf("segment over 80: %s", seg)
			}
			n++
		}
	}
	return n
}

// TestALoopDetailShowsLoopAsTextEvenWhenTruncated is the loop marker: it is
// spelled out in words, it survives the cut of a long detail, and the ↻ glyph the
// arm used to have never appears.
func TestALoopDetailShowsLoopAsTextEvenWhenTruncated(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "atlas", "blue", "colour32", "Bravo task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "plan-critic r2", "", ""),
		statusEvent("worker:feat/2-b#s1", 1791361800000, "working", strings.Repeat("lengthy detail words ", 4)+"ending in r3", "", ""),
	)
	l.windows = "feat/1-a\nfeat/2-b"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.diagram()
	if !strings.Contains(text, `working · plan-critic r2 (loop) · since 08:20" {`) {
		t.Errorf("the loop marker is missing:\n%s", text)
	}
	if !loopTruncated.MatchString(text) {
		t.Errorf("a truncated detail lost its loop marker:\n%s", text)
	}
	if strings.Contains(text, "↻") {
		t.Errorf("the glyph the arm dropped is back:\n%s", text)
	}
}

// loopTruncated is the truncated detail's own line: cut short, then (loop).
var loopTruncated = regexp.MustCompile(`lengthy detail[^"]*… \(loop\) · since 08:30" \{`)

// TestStaleFinishedAndReapedRowsAreNotDrawn is the diagram's cut on the file: of a
// bus with one windowed worker, one silent-but-dispatched one, one working row no
// window or worktree vouches for, one finished, and one reaped, only the first two
// are drawn — and the legend counts what it drew.
func TestStaleFinishedAndReapedRowsAreNotDrawn(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "atlas", "blue", "colour32", "Bravo task", "standard", "claude", "sonnet"),
		dispatchEvent("feat/3-c", 1791360120000, "nova", "magenta", "colour127", "Charlie task", "standard", "claude", "sonnet"),
		dispatchEvent("feat/4-d", 1791360180000, "ember", "orange", "colour130", "Delta task", "standard", "claude", "sonnet"),
		dispatchEvent("feat/5-e", 1791360240000, "iris", "red", "colour91", "Echo task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791360600000, "working", "", "", ""),
		statusEvent("worker:feat/2-b#s1", 1791360600000, "working", "", "", ""),
		statusEvent("worker:feat/3-c#s1", 1791360600000, "done", "", "", ""),
		statusEvent("worker:feat/4-d#s1", 1791360600000, "reaped", "", "", ""),
		// feat/5-e was dispatched and has not answered: live by definition.
	)
	// Only feat/1-a has a window, none of them has a worktree, and every stamp is
	// days old.
	l.windows = "feat/1-a"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.diagram()
	for _, want := range []string{`sage\nAlpha task`, `iris\nEcho task`, `legend: "2 active · 0 blocked · 0 done"`} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if n := len(workerLines.FindAllString(text, -1)); n != 2 {
		t.Errorf("%d worker boxes drawn, want 2:\n%s", n, text)
	}
	for _, banned := range []string{"atlas", "nova", "ember"} {
		if strings.Contains(text, banned) {
			t.Errorf("%s was drawn:\n%s", banned, text)
		}
	}
}

// workerLines is one worker box's opening line — the PR node and the hold are not
// workers.
var workerLines = regexp.MustCompile(`(?m)^w[0-9]+: `)

// TestAFinishedBranchIsDrawnUntilItsWorktreeIsReaped is the same rule with the
// evidence moving: `crew reap` takes the worktree, the row was the pointer to it,
// and with it gone the row is history — so the next pass stops drawing it.
func TestAFinishedBranchIsDrawnUntilItsWorktreeIsReaped(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791360600000, "done", "", "", ""),
	)
	// No window: the worktree is the only thing keeping this row drawn.
	l.windows = ""
	l.worktrees(t, "feat/1-a")

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if text := l.diagram(); !strings.Contains(text, `sage\nAlpha task`) {
		t.Errorf("a finished branch with its worktree was not drawn:\n%s", text)
	}

	l.worktrees(t)
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if text := l.diagram(); strings.Contains(text, "sage") {
		t.Errorf("the row outlived its worktree:\n%s", text)
	}
}

// TestUnchangedBusWritesNothingIsTheCmpGateEndToEnd: a rewrite, even of identical
// bytes, would move the mtime off the epoch the fixture left there, and a one-word
// change to a detail is exactly one line of the diagram out and one in.
func TestUnchangedBusWritesNothingIsTheCmpGateEndToEnd(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	file := l.diagramPath()
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	before := rewind(t, file)
	beforeText := l.diagram()

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if after := stampOf(t, file); after != before {
		t.Fatalf("an unchanged bus rewrote the diagram: %s then %s", before, after)
	}

	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361500000, "working", "execute: lint", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if after := stampOf(t, file); after == before {
		t.Fatalf("a changed bus left the diagram alone")
	}
	after := l.diagram()
	out, in := diffLines(beforeText, after)
	if out != 1 || in != 1 {
		t.Errorf("the detail change moved %d lines out and %d in, want one of each\n%s", out, in, after)
	}
	if !strings.Contains(after, `working · execute: lint · since 08:25" {`) {
		t.Errorf("the new detail is not what was drawn:\n%s", after)
	}
}

// diffLines is `diff | grep -c '^<'` and `'^>'`: how many lines belong to only one
// of the two renders.
func diffLines(before, after string) (int, int) {
	set := func(text string) map[string]int {
		out := map[string]int{}
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			out[line]++
		}
		return out
	}
	a, b := set(before), set(after)
	var onlyA, onlyB int
	for line, n := range a {
		if m := b[line]; m < n {
			onlyA += n - m
		}
	}
	for line, n := range b {
		if m := a[line]; m < n {
			onlyB += n - m
		}
	}
	return onlyA, onlyB
}

// TestEveryPaletteColorCompilesThroughD2 is the pool's own guarantee: the renderer
// writes each identity color as a quoted hex stroke looked up by name, so an entry
// with no hex gets no stroke at all. The colors are read from the pool, so a new
// one cannot join without this seeing it, and d2 — the real compiler — takes what
// comes out.
func TestEveryPaletteColorCompilesThroughD2(t *testing.T) {
	l := newLoop(t)
	utc(t)
	pool := identity.Palette()
	events := make([]string, 0, len(pool))
	windows := make([]string, 0, len(pool))
	for i, color := range pool {
		branch := fmt.Sprintf("feat/pal-%d", i+1)
		events = append(events, dispatchEvent(branch, int64(1791360000000+(i+1)*1000),
			fmt.Sprintf("pal%d", i+1), color, "colour28", fmt.Sprintf("Palette %d", i+1), "standard", "claude", "sonnet"))
		windows = append(windows, branch)
	}
	l.seed(t, events...)
	l.windows = strings.Join(windows, "\n")

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	// Every entry reached a stroke, so the compile below cannot pass vacuously.
	text := l.diagram()
	if n := strings.Count(text, `  style: {fill: transparent; stroke: "#`); n != len(pool) {
		t.Errorf("%d palette colors reached a stroke, want %d", n, len(pool))
	}

	// d2 is the compiler the palette has to satisfy. The devshell has it; a sandbox
	// that builds only ./crew does not.
	d2, err := exec.LookPath("d2")
	if err != nil {
		t.Skip("d2 not on PATH")
	}
	out, err := exec.Command(d2, l.diagramPath(), filepath.Join(t.TempDir(), "palette.svg")).CombinedOutput()
	if err != nil {
		t.Errorf("d2 rejected the diagram: %v\n%s", err, out)
	}
}

// TestDiagramFileIsNamedForItsRepoAndCrew is the carousel directory's contract, read
// off the files a renderer actually wrote: `roster-<repo>-<when>-<hash>.d2`, where
// `<when>` is the crew id's epoch prefix in the renderer's zone and the hash keeps
// two crews of one repo, or two repos sharing a directory name, in separate files.
func TestDiagramFileIsNamedForItsRepoAndCrew(t *testing.T) {
	t.Run("two crews of one repo", func(t *testing.T) {
		l := newLoop(t)
		// A crew started in the same minute as another is a different file, and both
		// hold the same crew: the bus is the repo's, shared by its crews.
		want := regexp.MustCompile(`^roster-` + regexp.QuoteMeta(repoToken(l.paths.Common)) + `-10-07-0800-[0-9a-f]{4}\.d2\z`)
		var names []string
		for _, crew := range []string{"1791360000-123", "1791360000-456"} {
			l.crew = crew
			l.seedBus(t)
			if code, stderr := l.once(Request{}); code != exitOK {
				t.Fatalf("%s: code = %d, stderr %q", crew, code, stderr)
			}
			name := filepath.Base(l.diagramPath())
			if !want.MatchString(name) {
				t.Errorf("%q is not a crew diagram name: want %s", name, want)
			}
			if got := l.diagram(); !strings.Contains(got, "Alpha task") {
				t.Errorf("%s holds no worker:\n%s", name, got)
			}
			names = append(names, name)
		}
		if names[0] == names[1] {
			t.Errorf("two crews of one repo share a file: %s", names[0])
		}
	})

	t.Run("two repos sharing a directory name", func(t *testing.T) {
		l := newLoop(t)
		utc(t)
		var files []string
		for _, tc := range []struct{ common, title string }{
			{filepath.Join(t.TempDir(), "x", "repo", ".git"), "from org x"},
			{filepath.Join(t.TempDir(), "y", "repo", ".git"), "from org y"},
		} {
			l.moveToRepo(t, tc.common)
			l.seed(t,
				dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", tc.title, "standard", "claude", "sonnet"),
				statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "", "", ""),
			)
			if code, stderr := l.once(Request{}); code != exitOK {
				t.Fatalf("%s: code = %d, stderr %q", tc.common, code, stderr)
			}
			files = append(files, l.diagramPath())
			if !strings.Contains(l.diagram(), tc.title) {
				t.Errorf("%s was not drawn in %s:\n%s", tc.title, tc.common, l.diagram())
			}
		}
		if files[0] == files[1] {
			t.Fatalf("both repos rendered the same file: %s", files[0])
		}
		// Same repo token, same crew: only the hash tells them apart.
		if strings.TrimSuffix(files[0], hashSuffix(files[0])) != strings.TrimSuffix(files[1], hashSuffix(files[1])) {
			t.Errorf("the two names differ by more than their hash:\n %s\n %s", files[0], files[1])
		}
		// Each file holds its own repo's crew and nothing of the other's.
		first, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(first), "from org x") || strings.Contains(string(first), "from org y") {
			t.Errorf("the first repo drew the second's crew:\n%s", first)
		}
	})
}

// repoToken is the `<repo>` of the file name: the sanitized directory that owns
// the bus.
func repoToken(common string) string {
	return sanitize(filepath.Base(filepath.Dir(common)))
}

// hashSuffix is the name's `-<4hex>.d2`, the only part two repos' names differ by.
func hashSuffix(file string) string {
	base := filepath.Base(file)
	i := strings.LastIndex(base, "-")
	return base[i:]
}

// TestTheLaunchWorktreeCanBeReapedUnderTheRenderer is why Run moves the process to
// the bus dir before anything resolves a path: a renderer started inside a worktree
// that `crew reap` takes away keeps drawing, and its worktree probe keeps naming
// the bus. A probe that resolved the repo from the cwd goes quiet with the worktree,
// and the finished row it was drawing comes back.
func TestTheLaunchWorktreeCanBeReapedUnderTheRenderer(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "execute: tests", "", ""),
		// A second branch, finished and with no worktree of its own: it is the row
		// that reads the probe.
		dispatchEvent("feat/6-gone", 1791360000000, "nova", "magenta", "colour127", "Gone task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/6-gone#s1", 1791360600000, "done", "", "", ""),
	)
	l.windows = "feat/1-a"
	var probed int
	l.opts.Git = &gittest.Fake{Func: func(_ string, args []string) (string, error) {
		probed++
		// Only a probe anchored on the bus answers. One that resolved the repo from
		// the process's cwd is exactly what the reaper takes away.
		if !slices.Contains(args, "--git-dir="+l.paths.Common) {
			t.Errorf("the worktree probe did not name the bus: %v", args)
			return "", errors.New("git: not a worktree")
		}
		return "", nil
	}}

	launch := filepath.Join(t.TempDir(), "rr-wt")
	if err := os.MkdirAll(launch, 0o755); err != nil {
		t.Fatal(err)
	}
	// The renderer is started from inside the worktree, which is then reaped. By the
	// time the test unwinds that directory is gone, so the cwd comes back by hand.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Chdir(launch); err != nil {
		t.Fatal(err)
	}
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if err := os.RemoveAll(launch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the launch worktree survived the reap")
	}

	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361500000, "working", "after-reap detail", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.diagram()
	if !strings.Contains(text, "after-reap detail") {
		t.Errorf("the renderer stopped drawing once its worktree was gone:\n%s", text)
	}
	if strings.Contains(text, "nova") {
		t.Errorf("a finished row with no worktree came back:\n%s", text)
	}
	if probed == 0 {
		t.Error("the worktree probe never ran, so the assertions above prove nothing")
	}
}
