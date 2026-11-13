package rosterrender

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
)

// The diagram, from the bus to the file: the bats fixtures' `_rr_seed` and the
// `roster-render:` cases that read what its `--once` publishes.

// utc pins the process zone, which names the diagram's file: the `<when>` stamp is
// the crew id's epoch in the renderer's local time. The bats fixtures exported
// TZ=UTC.
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

// TestOnceDrawsTheFixtureBus is the picture end to end: the bus the fixture
// plants, one `--once`, and what reaches the published file — a card per drawn
// worker headed codename and issue, a pulse on the blocked one, the stacked
// marker that survives a re-dispatch naming no base, the role pane as a chip,
// and a column per state with its count, the hold queued.
func TestOnceDrawsTheFixtureBus(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	// A re-dispatch without --base, older than feat/3-c's last status: the
	// stacked marker still comes from the earlier dispatch that named the base.
	l.appendBus(t,
		dispatchEvent("feat/3-c", 1791362700000, "nova", "magenta", "colour127", "Charlie task", "deep", "claude", "opus"))

	code, stderr := l.once(Request{})
	if code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	out := l.published()
	for _, label := range []string{"sage · #1", "atlas · #2", "nova · #3", "ember · #4"} {
		cardWith(t, out, label)
	}
	if !pulses(t, out, "nova · #3") {
		t.Error("the blocked worker does not pulse")
	}
	if _, nova := cardWith(t, out, "nova · #3"); !strings.Contains(nova.text(), "on #1") {
		t.Errorf("no stacked marker from nova to sage: %s", nova.text())
	}
	if n := cardCount(out); n != 5 {
		t.Errorf("%d cards drawn, want the 4 workers and the hold", n)
	}
	// One role pane is this crew's (the lead and the other crew's are not): an
	// idle chip.
	_, sage := cardWith(t, out, "sage · #1")
	if sage.hasClass("aeye-glow") || !strings.Contains(sage.text(), "spec-critic") {
		t.Errorf("sage's role chip is wrong: %s", sage.text())
	}
	if strings.Contains(out, "other-role") || strings.Contains(sage.text(), "lead") {
		t.Error("a pane that is not a role of this crew was drawn as a chip")
	}
	for _, want := range []string{"QUEUED 1", "WORKING 2", "BLOCKED 1", "DONE 1"} {
		if !strings.Contains(out, ">"+want+"<") {
			t.Errorf("no %q column", want)
		}
	}
	if strings.Contains(out, "PR OPEN") {
		t.Error("an empty column was drawn")
	}
}

// TestThePublishedFileIsTheSVGAndTheRecordsAreWritten: the file aeye reads is
// the board svg itself, an opaque canvas, so a changed picture is a changed
// file, and the change record and the slots sit beside the crew's other
// records, never in the carousel directory.
func TestThePublishedFileIsTheSVGAndTheRecordsAreWritten(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	out := l.published()
	if out != l.drawn() {
		t.Errorf("the published svg is not the recorded one")
	}
	if root := decodeSVG(t, out); root.Nodes[0].XMLName.Local != "rect" || root.Nodes[0].attr("fill") != colBG {
		t.Errorf("the canvas is not opaque: %+v", root.Nodes[0])
	}
	entries, err := os.ReadDir(filepath.Dir(l.diagramPath()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{filepath.Base(l.diagramPath())}; !slices.Equal(names, want) {
		t.Errorf("the carousel directory holds %q, want the svg alone: %q", names, want)
	}
	var s slotState
	data, err := os.ReadFile(filepath.Join(l.cdir(), fileSlots))
	if err != nil {
		t.Fatalf("no slots file: %v", err)
	}
	if err := json.Unmarshal(data, &s); err != nil || len(s.Order) != 4 {
		t.Errorf("slots = %s (%v), want four workers", data, err)
	}
}

// TestHostileTextCannotBreakOutOfTheSVG is the injection row: a codename, a
// host, a hold id, a role name, an engine and a palette color all arrive from
// the bus or from tmux, and none of them may leave its text element, add an
// element, or put a control byte in the file.
func TestHostileTextCannotBreakOutOfTheSVG(t *testing.T) {
	l := newLoop(t)
	name := "a\"b\\c$d |md ...@import x } {\n# e<b>"
	remote := busDoc(map[string]any{
		"ts": 1791360000000, "crew_id": "c1", "kind": "dispatch", "branch": "feat/3-r", "session": "s1",
		"worker_id": "worker:feat/3-r#s1", "engine": "claude", "model": "sonnet", "tier": "standard",
		"title": "Remote", "name": "rem", "color": "green", "tmux": "colour28",
		"host": "mbp\"];evil [label=\"x", "remote": true,
	})
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, name, "red; fill: blue", "colour32", "T\"itle", "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "sage", "green", "colour28", "Plain", "standard", "claude", "sonnet"),
		remote,
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
	l.windows = "feat/1-a\nfeat/2-b\nfeat/3-r"
	l.panes = strings.Join([]string{
		l.paths.Dir + "\tc1\tfeat/1-a\tr\";x\tidle\t",
		l.paths.Dir + "\tc1\tfeat/1-a\tspec-critic\tidle\t",
		l.paths.Dir + "\tc1\tfeat/2-b\tcritic\tidle\t",
	}, "\n") + "\n"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	out := l.published()
	if n := cardCount(out); n != 4 {
		t.Fatalf("%d cards drawn, want the 3 workers and the hold:\n%s", n, out)
	}
	// The fold swaps a codename the pool grammar rejects for a pool one; clean
	// guards what survives (planets_test).
	_, card := cardWith(t, out, " · #1")
	if !regexp.MustCompile(`(?:\A|\| )[a-z][a-z0-9-]* · #1(?: \||\z)`).MatchString(card.text()) {
		t.Errorf("the hostile codename was drawn as is: %s", card.text())
	}
	if _, rem := cardWith(t, out, "rem · #3"); !strings.Contains(rem.text(), "mbpevillabelx") {
		t.Errorf("the remote host is not reduced to its safe characters: %s", rem.text())
	}
	if !strings.Contains(out, "<polygon") {
		t.Error("the remote worker has no hexagon chip")
	}
	for _, want := range []string{"T&quot;itle", "go ${x}end"} {
		if !strings.Contains(out, want) {
			t.Errorf("the card lacks the sanitized text %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"red;", "fill: blue", ";x", "<b>", "h\"1", "h&quot;1", `\y`, "T\"itle"} {
		if strings.Contains(out, banned) {
			t.Errorf("%q reached the diagram:\n%s", banned, out)
		}
	}
	// The file is well-formed and every id is a generated one.
	id := regexp.MustCompile(`\Acard-\d+-\d+-\d+\z`)
	decodeSVG(t, out).walk(func(n xnode) {
		if v := n.attr("id"); v != "" && !id.MatchString(v) {
			t.Errorf("id %q is not a generated one", v)
		}
	})
	if controlByte.MatchString(out) {
		t.Errorf("a control byte reached the file: %q", out)
	}
}

// controlByte is every control character except the newline between elements.
// A tab off the pane listing, or an ESC off the bus, would reach a terminal
// otherwise.
var controlByte = regexp.MustCompile(`[\x00-\x09\x0b-\x1f]`)

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
	out := l.published()
	for _, want := range []string{"sage · #1", "iris · #5", ">WORKING 2<"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if n := cardCount(out); n != 2 {
		t.Errorf("%d cards drawn, want 2:\n%s", n, out)
	}
	if strings.Contains(out, "BLOCKED") || strings.Contains(out, "DONE") {
		t.Errorf("a column for a row the cut dropped:\n%s", out)
	}
	for _, banned := range []string{"atlas", "nova", "ember"} {
		if strings.Contains(out, banned) {
			t.Errorf("%s was drawn:\n%s", banned, out)
		}
	}
}

// TestAFinishedBranchIsDrawnUntilItsWorktreeIsReaped is the same rule with the
// evidence moving: `crew reap` takes the worktree, the row was the pointer to it,
// and with it gone the row is history — so the next pass stops drawing it.
func TestAFinishedBranchIsDrawnUntilItsWorktreeIsReaped(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", l.wallNow()*1000-1200000, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		// Minutes before the pinned wall clock: the row is younger than staleAfter,
		// so the worktree is its only evidence of life, and younger than the fold
		// that moves a finished planet into the legend.
		statusEvent("worker:feat/1-a#s1", l.wallNow()*1000-120000, "done", "", "", ""),
	)
	// No window and no age to spare: the worktree is the only thing keeping this
	// row drawn.
	l.windows = ""
	l.worktrees(t, "feat/1-a")

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if out := l.published(); !strings.Contains(out, "sage · #1") {
		t.Errorf("a finished branch with its worktree was not drawn:\n%s", out)
	}

	l.worktrees(t)
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if out := l.published(); strings.Contains(out, "sage") {
		t.Errorf("the row outlived its worktree:\n%s", out)
	}
}

// TestAFinishedRowPastStaleAfterIsNotDrawn is the other half of the rule: a
// finished row whose window is long gone and whose last word is past staleAfter
// stops being drawn even though its worktree still waits for `crew reap`, and the
// legend counts only what was drawn.
func TestAFinishedRowPastStaleAfterIsNotDrawn(t *testing.T) {
	l := newLoop(t)
	utc(t)
	old := l.wallNow()*1000 - staleAfterMs - 1000
	l.seed(t,
		dispatchEvent("feat/1-a", old, "sage", "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", old, "done", "", "", ""),
	)
	// No window; the worktree alone still stands, and it is not enough.
	l.windows = ""
	l.worktrees(t, "feat/1-a")

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	out := l.published()
	if strings.Contains(out, "sage") {
		t.Errorf("a finished row past staleAfter outlived its window:\n%s", out)
	}
	if strings.Contains(out, "DONE") {
		t.Errorf("a done column for a row the cut dropped:\n%s", out)
	}
}

// staleAfterMs is the roster rule's bound in the bus's millisecond stamps.
const staleAfterMs = 48 * 60 * 60 * 1000

// TestUnchangedBusWritesNothing is the gate end to end: a pass whose svg equals
// the recorded one rewrites nothing, even when the bus grew by a line the
// picture does not show; a state change redraws the svg, the record and the
// slots.
func TestUnchangedBusWritesNothing(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	files := []string{l.diagramPath(), filepath.Join(l.cdir(), fileDrawn), filepath.Join(l.cdir(), fileSlots)}
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	before := l.published()
	stamps := make([]string, len(files))
	for i, f := range files {
		stamps[i] = rewind(t, f)
	}

	// A repeated status is not on the picture: the bus changed, the svg did not.
	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361500000, "working", "plan-critic r2", "", ""))
	for range 2 {
		if code, stderr := l.once(Request{}); code != exitOK {
			t.Fatalf("code = %d, stderr %q", code, stderr)
		}
	}
	for i, f := range files {
		if after := stampOf(t, f); after != stamps[i] {
			t.Errorf("an unchanged picture rewrote %s: %s then %s", filepath.Base(f), stamps[i], after)
		}
	}

	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361600000, "blocked", "", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	for i, f := range files {
		if after := stampOf(t, f); after == stamps[i] {
			t.Errorf("a changed picture left %s alone", filepath.Base(f))
		}
	}
	after := l.published()
	if after == before {
		t.Fatal("the published picture did not change")
	}
	if !pulses(t, after, "sage · #1") {
		t.Error("the newly blocked worker does not pulse")
	}
	if l.drawn() != after {
		t.Errorf("the change record is not what was drawn")
	}
}

// TestAWorkerKeepsItsSlotAcrossPasses is the persistence end to end: the slots
// file written by the first pass is what the second reads, so a worker whose
// neighbour flips state keeps its card.
func TestAWorkerKeepsItsSlotAcrossPasses(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "sage", "green", "colour28", "Alpha", "standard", "claude", "sonnet"),
		dispatchEvent("feat/2-b", 1791360060000, "atlas", "blue", "colour32", "Bravo", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "", "", ""),
		statusEvent("worker:feat/2-b#s1", 1791361200000, "working", "", "", ""),
	)
	l.windows = "feat/1-a\nfeat/2-b"
	slots := func() slotState {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(l.cdir(), fileSlots))
		if err != nil {
			t.Fatalf("no slots file: %v", err)
		}
		var s slotState
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	first := slots()
	repo := l.paths.Common
	if want := []slotEntry{{Branch: "feat/1-a", Class: classWorking, Repo: repo}, {Branch: "feat/2-b", Class: classWorking, Repo: repo}}; !slices.Equal(first.Order, want) {
		t.Fatalf("slots after the first pass = %+v, want %+v", first.Order, want)
	}
	if id, _ := cardWith(t, l.published(), "atlas · #2"); id != "card-0-0-1" {
		t.Errorf("atlas is %s, want the second working card", id)
	}

	// atlas flips to blocked and moves ahead of sage in the sequence and to the
	// blocked column on the board; sage keeps the first working card.
	l.appendBus(t, statusEvent("worker:feat/2-b#s1", 1791361500000, "blocked", "", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	second := slots()
	if want := []slotEntry{{Branch: "feat/2-b", Class: classBlocked, Repo: repo}, {Branch: "feat/1-a", Class: classWorking, Repo: repo}}; !slices.Equal(second.Order, want) {
		t.Errorf("slots after the flip = %+v, want %+v", second.Order, want)
	}
	for label, want := range map[string]string{"sage · #1": "card-0-0-0", "atlas · #2": "card-0-1-0"} {
		if id, _ := cardWith(t, l.published(), label); id != want {
			t.Errorf("%s is %s, want %s", label, id, want)
		}
	}
}

// TestARefusedWriteKeepsTheOldRecordsAndTheLiveCount: the diagram's path has
// become a symlink, so put refuses it and says so; the pass writes no record or
// slots and still returns the model's live count, so the daemon's quiet window
// keeps working.
func TestARefusedWriteKeepsTheOldRecordsAndTheLiveCount(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	files := []string{filepath.Join(l.cdir(), fileDrawn), filepath.Join(l.cdir(), fileSlots)}
	want := map[string]string{}
	stamps := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		want[f], stamps[f] = string(data), rewind(t, f)
	}
	if err := os.Remove(l.diagramPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.svg"), l.diagramPath()); err != nil {
		t.Fatal(err)
	}

	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361600000, "blocked", "", "", ""))
	n, err := pass(t.Context(), l.opts, l.paths, l.cdir(), l.crew, true, defaultAspect, nil)
	if err != nil {
		t.Fatalf("a refused write is not a pass failure: %v", err)
	}
	// feat/1-a is now blocked, feat/4-d dispatched, feat/3-c blocked, plus the hold.
	if n != 4 {
		t.Errorf("live = %d, want the model's 4", n)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || string(data) != want[f] || stampOf(t, f) != stamps[f] {
			t.Errorf("%s was touched by a refused render (%v)", filepath.Base(f), err)
		}
	}
	if errOut := l.opts.Stderr.(*bytes.Buffer).String(); !strings.Contains(errOut, "is not a regular file — not writing it") {
		t.Errorf("stderr does not name the refusal: %q", errOut)
	}

	// The symlink goes: the same bus draws.
	if err := os.Remove(l.diagramPath()); err != nil {
		t.Fatal(err)
	}
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if !pulses(t, l.published(), "sage · #1") {
		t.Error("the redraw after the refusal is stale")
	}
}

// TestAFirstRenderThatIsRefusedWritesNoRecords: with nothing drawn yet there is
// no picture to keep, and no slots or record may be written for one that never
// was.
func TestAFirstRenderThatIsRefusedWritesNoRecords(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	if err := os.MkdirAll(filepath.Dir(l.diagramPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.svg"), l.diagramPath()); err != nil {
		t.Fatal(err)
	}
	if code, stderr := l.once(Request{}); code != exitOK || !strings.Contains(stderr, "not writing it") {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	for _, f := range []string{filepath.Join(l.cdir(), fileDrawn), filepath.Join(l.cdir(), fileSlots)} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after a refused first render (%v)", filepath.Base(f), err)
		}
	}
}

// TestDiagramFileIsNamedForItsRepoAndCrew is the carousel directory's contract, read
// off the files a renderer actually wrote: `roster-<repo>-<when>-<hash>.svg`, where
// `<when>` is the crew id's epoch prefix in the renderer's zone and the hash keeps
// two crews of one repo, or two repos sharing a directory name, in separate files.
func TestDiagramFileIsNamedForItsRepoAndCrew(t *testing.T) {
	t.Run("two crews of one repo", func(t *testing.T) {
		l := newLoop(t)
		// A crew started in the same minute as another is a different file, and both
		// hold the same crew: the bus is the repo's, shared by its crews.
		want := regexp.MustCompile(`^roster-` + regexp.QuoteMeta(repoToken(l.paths.Common)) + `-10-07-0800-[0-9a-f]{4}\.svg\z`)
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
			if got := l.published(); !strings.Contains(got, "sage · #1") {
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
		for _, tc := range []struct{ common, name string }{
			{filepath.Join(t.TempDir(), "x", "repo", ".git"), "orgx"},
			{filepath.Join(t.TempDir(), "y", "repo", ".git"), "orgy"},
		} {
			l.moveToRepo(t, tc.common)
			l.seed(t,
				dispatchEvent("feat/1-a", 1791360000000, tc.name, "green", "colour28", "Alpha task", "standard", "claude", "sonnet"),
				statusEvent("worker:feat/1-a#s1", 1791361200000, "working", "", "", ""),
			)
			if code, stderr := l.once(Request{}); code != exitOK {
				t.Fatalf("%s: code = %d, stderr %q", tc.common, code, stderr)
			}
			files = append(files, l.diagramPath())
			if !strings.Contains(l.published(), tc.name) {
				t.Errorf("%s was not drawn in %s:\n%s", tc.name, tc.common, l.published())
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
		if !strings.Contains(string(first), "orgx") || strings.Contains(string(first), "orgy") {
			t.Errorf("the first repo drew the second's crew:\n%s", first)
		}
	})
}

// repoToken is the `<repo>` of the file name: the sanitized directory that owns
// the bus.
func repoToken(common string) string {
	return sanitize(filepath.Base(filepath.Dir(common)))
}

// hashSuffix is the name's `-<4hex>.svg`, the only part two repos' names differ by.
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

	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361500000, "blocked", "", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	text := l.published()
	if !pulses(t, text, "sage · #1") {
		t.Errorf("the renderer stopped drawing once its worktree was gone:\n%s", text)
	}
	if strings.Contains(text, "nova") {
		t.Errorf("a finished row with no worktree came back:\n%s", text)
	}
	if probed == 0 {
		t.Error("the worktree probe never ran, so the assertions above prove nothing")
	}
}
