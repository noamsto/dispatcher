package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const retroHeader = "branch\tengine\tmodel\ttier\toutcome\ttags"

// busSeed appends hand-built events to the repo's bus, with explicit ts values
// so the dispatch-boundary windowing fixtures control run ordering exactly
// (`crew status` always stamps now).
type busSeed struct {
	t   *testing.T
	log string
}

func (b *busSeed) add(ev map[string]any) {
	b.t.Helper()
	ev["crew_id"] = "c1"
	line, err := json.Marshal(ev)
	if err != nil {
		b.t.Fatal(err)
	}
	b.raw(string(line))
}

func (b *busSeed) raw(line string) {
	b.t.Helper()
	f, err := os.OpenFile(b.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		b.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.t.Fatal(err)
	}
}

// dispatch seeds a dispatch event; extra overrides or adds fields (plan,
// task_kind, resume, tier, ...).
func (b *busSeed) dispatch(branch string, ts int, extra map[string]any) {
	b.t.Helper()
	ev := map[string]any{"ts": ts, "kind": "dispatch", "branch": branch,
		"engine": "claude", "model": "sonnet", "tier": "standard", "effort": "medium", "title": "t"}
	maps.Copy(ev, extra)
	b.add(ev)
}

// resume is a `dispatch resume` bus row in the shape dispatch-resume.sh
// writes: no `from` field, unlike every other event seeded here.
func (b *busSeed) resume(branch string, ts int) {
	b.t.Helper()
	b.add(map[string]any{"ts": ts, "kind": "resume", "branch": branch,
		"worker_id": "worker:" + branch + "#s2", "prev_worker_id": "worker:" + branch + "#s1",
		"engine": "claude", "model": "sonnet", "session": "s2", "continued": true})
}

func (b *busSeed) status(from string, ts int, state string) {
	b.t.Helper()
	b.add(map[string]any{"ts": ts, "from": from, "to": "dispatcher:c1", "kind": "status",
		"body": map[string]any{"state": state}})
}

// msg's body is the JSON string form, which is what `crew msg` itself writes.
func (b *busSeed) msg(from, to string, ts int, body string) {
	b.t.Helper()
	b.add(map[string]any{"ts": ts, "from": from, "to": to, "kind": "msg", "body": body})
}

func newRetroSeed(t *testing.T) (*holdCLI, *busSeed) {
	t.Helper()
	h := newHoldCLI(t)
	if err := os.MkdirAll(h.repo+"/.git/crew", 0o755); err != nil {
		t.Fatal(err)
	}
	return h, &busSeed{t: t, log: h.log}
}

// retroOK runs `crew retro args...` and requires a clean exit with no stderr.
func retroOK(t *testing.T, h *holdCLI, args ...string) string {
	t.Helper()
	out, errOut, code := h.run(append([]string{"retro"}, args...)...)
	if code != 0 || errOut != "" {
		t.Fatalf("retro %v: code %d, stderr %q", args, code, errOut)
	}
	return out
}

func retroLines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

type retroReport struct {
	Tags    []map[string]any `json:"tags"`
	Unknown []string         `json:"unknown"`
	Rows    []map[string]any `json:"rows"`
}

func retroJSON(t *testing.T, h *holdCLI) retroReport {
	t.Helper()
	out := retroOK(t, h, "--report", "--json")
	var r retroReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("--report --json is not JSON: %v\n%s", err, out)
	}
	return r
}

func (r retroReport) tag(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, tg := range r.Tags {
		if tg["tag"] == name {
			return tg
		}
	}
	t.Fatalf("tag %q missing from %v", name, r.Tags)
	return nil
}

func (r retroReport) hasTag(name string) bool {
	for _, tg := range r.Tags {
		if tg["tag"] == name {
			return true
		}
	}
	return false
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	got := retroLines(out)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines =\n%q\nwant\n%q", got, want)
	}
}

func wantTagCounts(t *testing.T, r retroReport, name string, hits, runs, nondone float64) {
	t.Helper()
	tg := r.tag(t, name)
	if tg["hits"] != hits || tg["runs"] != runs || tg["nondone_pct"] != nondone {
		t.Errorf("%s = %v, want hits %v runs %v nondone_pct %v", name, tg, hits, runs, nondone)
	}
}

func note(tag, detail string) string {
	b, _ := json.Marshal(map[string]string{"seam": "execute", "tag": tag, "detail": detail})
	return string(b)
}

func TestRetroTwoDispatchesOnOneBranchKeepTheirOwnNotes(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/reuse", 1000, nil)
	s.msg("worker:feat/reuse#s1", "retro:c1", 1200, note("gate_thrash", "first run"))
	s.status("worker:feat/reuse#s1", 1500, "failed")
	s.dispatch("feat/reuse", 2000, nil)
	s.msg("worker:feat/reuse#s2", "retro:c1", 2200, note("command_not_found", "second run"))
	s.status("worker:feat/reuse#s2", 2500, "done")

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/reuse\tclaude\tsonnet\tstandard\tfailed\tgate_thrash",
		"feat/reuse\tclaude\tsonnet\tstandard\tdone\tcommand_not_found")

	r := retroJSON(t, h)
	wantTagCounts(t, r, "gate_thrash", 1, 1, 100)
	wantTagCounts(t, r, "command_not_found", 1, 1, 0)
}

func TestRetroSeamNotesSurviveARunWithNoTerminalStatus(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/killed", 1000, nil)
	s.status("worker:feat/killed#s1", 1100, "working")
	s.msg("worker:feat/killed#s1", "retro:c1", 1200, note("gate_thrash", "circled build"))
	s.dispatch("feat/hardkill", 3000, nil)
	s.msg("worker:feat/hardkill#s1", "retro:c1", 3200, note("rung_blocked", "top rung"))

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/hardkill\tclaude\tsonnet\tstandard\t—\trung_blocked",
		"feat/killed\tclaude\tsonnet\tstandard\tworking\tgate_thrash")

	r := retroJSON(t, h)
	wantTagCounts(t, r, "gate_thrash", 1, 1, 100)
	wantTagCounts(t, r, "rung_blocked", 1, 1, 100)
}

func TestRetroLaterMetricsSnapshotWinsAndDoesNotDuplicate(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/resumed", 1000, nil)
	s.msg("worker:feat/resumed#s1", "metrics:c1", 1200, `{"rework_count":1,"notes":[{"seam":"execute","tag":"other","detail":"early"}]}`)
	s.msg("worker:feat/resumed#s1", "metrics:c1", 1300, `{"rework_count":2,"notes":[{"seam":"execute","tag":"gate_thrash","detail":"late"}]}`)
	s.status("worker:feat/resumed#s1", 1400, "done")

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/resumed\tclaude\tsonnet\tstandard\tdone\tgate_thrash")

	r := retroJSON(t, h)
	if len(r.Tags) != 1 {
		t.Fatalf("tags = %v, want only gate_thrash", r.Tags)
	}
	tg := r.tag(t, "gate_thrash")
	if tg["hits"] != float64(1) || !slices.Equal(anyStrings(t, tg["details"]), []string{"late"}) {
		t.Errorf("gate_thrash = %v", tg)
	}
}

func TestRetroNoteInSeamMsgAndSnapshotCountsOnce(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/dedupe", 1000, nil)
	s.msg("worker:feat/dedupe#s1", "retro:c1", 1200, note("gate_thrash", "gate=build target=pkg/x"))
	s.msg("worker:feat/dedupe#s1", "metrics:c1", 1300, `{"notes":[`+note("gate_thrash", "gate=build target=pkg/x")+`]}`)
	s.status("worker:feat/dedupe#s1", 1400, "done")

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/dedupe\tclaude\tsonnet\tstandard\tdone\tgate_thrash")

	r := retroJSON(t, h)
	if len(r.Tags) != 1 || r.tag(t, "gate_thrash")["hits"] != float64(1) {
		t.Errorf("tags = %v, want one gate_thrash hit", r.Tags)
	}
}

func TestRetroRepeatedTagInOneRunIsCountedNotDeduped(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/twice", 1000, nil)
	s.msg("worker:feat/twice#s1", "retro:c1", 1200, note("command_not_found", "no just"))
	s.msg("worker:feat/twice#s1", "retro:c1", 1250, note("command_not_found", "no golangci-lint"))
	s.msg("worker:feat/twice#s1", "retro:c1", 1260, note("gate_thrash", "circled"))
	s.status("worker:feat/twice#s1", 1400, "failed")

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/twice\tclaude\tsonnet\tstandard\tfailed\tcommand_not_found x2,gate_thrash")

	tg := retroJSON(t, h).tag(t, "command_not_found")
	if tg["hits"] != float64(2) || tg["runs"] != float64(1) {
		t.Errorf("command_not_found = %v, want hits 2 runs 1", tg)
	}
}

func TestRetroUnknownTagIsReportedNeverDropped(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/unknown", 1000, nil)
	s.msg("worker:feat/unknown#s1", "retro:c1", 1200, note("gate_thrashh", "typo tag"))
	s.msg("worker:feat/unknown#s1", "retro:c1", 1250, note("oops", "bare guess"))
	s.msg("worker:feat/unknown#s1", "retro:c1", 1260, note("gate_thrash", "real one"))
	s.status("worker:feat/unknown#s1", 1400, "failed")

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/unknown\tclaude\tsonnet\tstandard\tfailed\tgate_thrashh,oops,gate_thrash")

	// Known tags first in vocabulary order, then the unknown ones alphabetically.
	lines := retroLines(retroOK(t, h, "--report"))
	if len(lines) != 5 {
		t.Fatalf("report = %q, want header, three rows and a footer", lines)
	}
	for i, prefix := range map[int]string{1: "gate_thrash ", 2: "gate_thrashh ", 3: "oops "} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	if lines[4] != "2 unrecognized tag(s): gate_thrashh, oops" {
		t.Errorf("footer = %q", lines[4])
	}

	r := retroJSON(t, h)
	if strings.Join(r.Unknown, ",") != "gate_thrashh,oops" {
		t.Errorf("unknown = %v", r.Unknown)
	}
	var unknownHits []any
	for _, tg := range r.Tags {
		if tg["known"] == false {
			unknownHits = append(unknownHits, tg["hits"])
		}
	}
	if len(unknownHits) != 2 || unknownHits[0] != float64(1) || unknownHits[1] != float64(1) {
		t.Errorf("unknown tag hits = %v, want [1 1]", unknownHits)
	}
}

func TestRetroNoUnknownTagsLeavesFooterOffAndUnknownEmpty(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/clean-vocab", 1000, nil)
	s.msg("worker:feat/clean-vocab#s1", "retro:c1", 1200, note("other", "d"))
	s.status("worker:feat/clean-vocab#s1", 1400, "done")

	out := retroOK(t, h, "--report")
	if n := len(retroLines(out)); n != 2 || strings.Contains(out, "unrecognized") {
		t.Errorf("report = %q, want header and one row, no footer", out)
	}
	if r := retroJSON(t, h); r.Unknown == nil || len(r.Unknown) != 0 {
		t.Errorf("unknown = %#v, want []", r.Unknown)
	}
}

func TestRetroCleanRunIsSilentInAllThreeModes(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/clean", 1000, nil)
	s.status("worker:feat/clean#s1", 1100, "working")
	s.status("worker:feat/clean#s1", 1400, "done")
	s.msg("worker:feat/clean#s1", "metrics:c1", 1450, `{"rework_count":0,"notes":[]}`)

	if out := retroOK(t, h); out != retroHeader+"\n" {
		t.Errorf("bare = %q", out)
	}
	if out := retroOK(t, h, "--report"); out != "tag  hits  runs  engines  nondone  sample\n" {
		t.Errorf("--report = %q", out)
	}
	r := retroJSON(t, h)
	if len(r.Tags) != 0 || r.Tags == nil || len(r.Unknown) != 0 || r.Unknown == nil {
		t.Errorf("--json = tags %#v, unknown %#v, want both []", r.Tags, r.Unknown)
	}
}

func TestRetroDispatcherNotesGetTheirOwnRowWithoutRunAttribution(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/w", 1000, nil)
	s.msg("worker:feat/w#s1", "retro:c1", 1200, note("gate_thrash", "d"))
	s.status("worker:feat/w#s1", 1400, "done")
	s.msg("dispatcher:c1", "retro:c1", 5000, `{"seam":"drained","tag":"misrouted","detail":"trivial, but the diff touched auth"}`)
	s.msg("dispatcher:c1", "retro:c1", 5100, `{"seam":"drained","tag":"session_summary","detail":"one worker, one thrash"}`)

	// Dispatcher rows sort last.
	wantLines(t, retroOK(t, h), retroHeader,
		"feat/w\tclaude\tsonnet\tstandard\tdone\tgate_thrash",
		"dispatcher:c1\t—\t—\t—\t—\tmisrouted,session_summary")

	// misrouted is a dispatcher tag: runs 0, no engines, no non-done share.
	if out := retroOK(t, h, "--report"); !strings.Contains(out, "misrouted           1     0  —              —  trivial, but the diff touched auth") {
		t.Errorf("report = %q", out)
	}

	r := retroJSON(t, h)
	m := r.tag(t, "misrouted")
	if m["runs"] != float64(0) || len(anyStrings(t, m["engines"])) != 0 || m["nondone_pct"] != nil {
		t.Errorf("misrouted = %v", m)
	}
	if r.tag(t, "session_summary")["runs"] != float64(0) {
		t.Errorf("session_summary = %v", r.tag(t, "session_summary"))
	}
}

func TestRetroUnparseableNoteBodyDoesNotAbortTheFold(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/broken", 1000, nil)
	s.msg("worker:feat/broken#s1", "retro:c1", 1200, "not json at all")
	s.status("worker:feat/broken#s1", 1400, "failed")
	s.dispatch("feat/good", 2000, nil)
	s.msg("worker:feat/good#s1", "retro:c1", 2200, note("consult_failed", "codex refused"))
	s.status("worker:feat/good#s1", 2400, "done")

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/good\tclaude\tsonnet\tstandard\tdone\tconsult_failed")
}

func TestRetroMalformedStatusBodyCostsOneRunItsOutcomeNotTheFold(t *testing.T) {
	h, s := newRetroSeed(t)
	// An object body whose state is itself an object sorts before every other
	// branch here, so a fold that died on it would take the good row down too.
	s.dispatch("feat/aobjstate", 500, nil)
	s.msg("worker:feat/aobjstate#s1", "retro:c1", 700, note("gate_thrash", "must survive"))
	s.raw(`{"ts":800,"crew_id":"c1","from":"worker:feat/aobjstate#s1","to":"dispatcher:c1","kind":"status","body":{"state":{"weird":true}}}`)
	s.dispatch("feat/badstatus", 1000, nil)
	s.msg("worker:feat/badstatus#s1", "retro:c1", 1200, note("gate_thrash", "still visible"))
	// `crew status` cannot write this, but a hand-edited or truncated bus can.
	s.raw(`{"ts":1300,"crew_id":"c1","from":"worker:feat/badstatus#s1","to":"dispatcher:c1","kind":"status","body":"oops-not-an-object"}`)
	s.dispatch("feat/other", 2000, nil)
	s.msg("worker:feat/other#s1", "retro:c1", 2200, note("consult_failed", "codex refused"))
	s.status("worker:feat/other#s1", 2400, "done")

	// Both corrupt bodies degrade to the same em dash a status-less run gets.
	wantLines(t, retroOK(t, h), retroHeader,
		"feat/aobjstate\tclaude\tsonnet\tstandard\t—\tgate_thrash",
		"feat/badstatus\tclaude\tsonnet\tstandard\t—\tgate_thrash",
		"feat/other\tclaude\tsonnet\tstandard\tdone\tconsult_failed")

	var names []string
	for _, tg := range retroJSON(t, h).Tags {
		names = append(names, tg["tag"].(string))
	}
	if strings.Join(names, ",") != "gate_thrash,consult_failed" {
		t.Errorf("tags = %v", names)
	}
}

func TestRetroControlCharactersNeverReachARenderedSurface(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/inject", 1000, nil)
	// Cursor-up + erase-line: raw, this repaints the row above it in the table.
	// NEL/DEL/RLO/LRI on the end cover the multibyte C1 range and the other
	// stripped classes in one detail.
	s.msg("worker:feat/inject#s1", "retro:c1", 1200,
		note("gate_thrash", "benign\x1b[1A\x1b[2KFAKE ROW\x1b[0m"+string(rune(0x85))+"N\x7fD"+string(rune(0x202e))+"R"+string(rune(0x2066))+"I"))
	s.msg("worker:feat/inject#s1", "retro:c1", 1250, note("drift\ntag\tx", "tag carries a newline and a tab"))
	s.status("worker:feat/inject#s1", 1400, "done")

	out := retroOK(t, h, "--report")
	if strings.Contains(out, "\x1b") {
		t.Errorf("--report leaked ESC: %q", out)
	}
	lines := retroLines(out)
	// Header + two rows + footer: the tag no longer splits its own row.
	if len(lines) != 4 {
		t.Fatalf("report = %q, want 4 lines", lines)
	}
	// Sample is the last column, so its exact suffix proves NEL/DEL/RLO/LRI are
	// all gone, not merely absent from a truncated prefix.
	if !strings.HasSuffix(lines[1], "benign[1A[2KFAKE ROW[0mNDRI") {
		t.Errorf("sample row = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "drift tag x") {
		t.Errorf("tag row = %q", lines[2])
	}
	if lines[3] != "1 unrecognized tag(s): drift tag x" {
		t.Errorf("footer = %q", lines[3])
	}

	bare := retroOK(t, h)
	if strings.Contains(bare, "\x1b") {
		t.Errorf("bare leaked ESC: %q", bare)
	}
	wantLines(t, bare, retroHeader,
		"feat/inject\tclaude\tsonnet\tstandard\tdone\tgate_thrash,drift tag x")

	// --json keeps the detail verbatim; its encoder is what renders the byte inert.
	jsonOut := retroOK(t, h, "--report", "--json")
	if strings.Contains(jsonOut, "\x1b") {
		t.Errorf("--json leaked ESC: %q", jsonOut)
	}
	var r retroReport
	if err := json.Unmarshal([]byte(jsonOut), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Unknown) != 1 || r.Unknown[0] != "drift tag x" {
		t.Errorf("unknown = %q", r.Unknown)
	}
}

func TestRetroReportRowsCarryNoTrailingWhitespace(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/pad", 1000, nil)
	s.msg("worker:feat/pad#s1", "retro:c1", 1200, note("gate_thrash", "short"))
	s.msg("worker:feat/pad#s1", "retro:c1", 1250, note("command_not_found", "a sample long enough to widen the last column"))
	s.status("worker:feat/pad#s1", 1400, "done")

	lines := retroLines(retroOK(t, h, "--report"))
	if len(lines) != 3 {
		t.Fatalf("report = %q, want 3 lines", lines)
	}
	for _, line := range lines {
		if strings.HasSuffix(line, " ") {
			t.Errorf("trailing space on %q", line)
		}
	}
}

func TestRetroOversizedGateThrashDetailIsOneParseableLineThroughCrewMsg(t *testing.T) {
	h, s := newRetroSeed(t)
	t.Setenv("CREW_ID", "c1")
	s.dispatch("feat/big", 1000, nil)
	bigNote := note("gate_thrash", strings.Repeat("ledger-row-", 600))
	if len(bigNote) <= 4096 {
		t.Fatalf("fixture note is %d bytes, want over 4096", len(bigNote))
	}
	if _, errOut, code := h.run("msg", "worker:feat/big#s1", "retro:c1", bigNote); code != 0 {
		t.Fatalf("msg: code %d, stderr %q", code, errOut)
	}

	// The note lands as ONE record inside bus.LineMax, and bus.Shrink cuts the
	// string leaf rather than the object, so the body still parses and the tag
	// survives.
	data, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d bus lines, want 2", len(lines))
	}
	for _, l := range lines {
		if len(l) > 4096 {
			t.Errorf("bus line is %d bytes, over 4096", len(l))
		}
	}
	var ev struct {
		Body string `json:"body"`
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(ev.Body), &body); err != nil || body.Tag != "gate_thrash" {
		t.Fatalf("shrunken body %q: tag %q, err %v", ev.Body, body.Tag, err)
	}

	wantLines(t, retroOK(t, h), retroHeader,
		"feat/big\tclaude\tsonnet\tstandard\t—\tgate_thrash")

	// The report truncates the sample; --json keeps the shrunken detail whole.
	if rep := retroLines(retroOK(t, h, "--report")); !strings.HasSuffix(rep[1], "…") {
		t.Errorf("report row = %q, want a truncated sample", rep[1])
	}
	details := anyStrings(t, retroJSON(t, h).Tags[0]["details"])
	if utf8.RuneCountInString(details[0]) <= 60 {
		t.Errorf("--json detail was cut to %q", details[0])
	}
}

func TestRetroSinkDoesNotWakeTheDispatcher(t *testing.T) {
	h, _ := newRetroSeed(t)
	t.Setenv("CREW_ID", "c1")
	for _, args := range [][]string{
		{"msg", "worker:feat/sink#s1", "retro:c1", note("other", "d")},
		{"msg", "dispatcher:c1", "retro:c1", `{"seam":"drained","tag":"session_summary","detail":"d"}`},
	} {
		if _, errOut, code := h.run(args...); code != 0 {
			t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
		}
	}

	out, errOut, code := h.run("inbox", "dispatcher:c1")
	if code != 0 || out != "" {
		t.Errorf("inbox: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = h.run("watch", "--since", "0", "--timeout", "1", "--interval", "1")
	if code != 0 || out != "" {
		t.Errorf("watch: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// plan_required_unaudited (#179): a standard-tier implement run dispatched
// with plan: required, whose snapshot records no plan-critic verdict but did
// reach the review gate, and has no plan seam, resume row or resume flag.
func TestRetroPlanRequiredUnaudited(t *testing.T) {
	const flagged = "plan_required_unaudited"
	nullVerdict := `{"plan_critic_first_pass":null,"review_mode":"full"}`
	tests := []struct {
		name string
		seed func(s *busSeed)
		want string // the one run's bare row, or "" for no row
	}{
		{"plan required, standard tier, null verdict flags", func(s *busSeed) {
			s.dispatch("feat/planreq", 1000, map[string]any{"plan": "required"})
			s.msg("worker:feat/planreq#s1", "metrics:c1", 1200, nullVerdict)
			s.status("worker:feat/planreq#s1", 1300, "done")
		}, "feat/planreq\tclaude\tsonnet\tstandard\tdone\t" + flagged},
		{"trivial tier does not flag", func(s *busSeed) {
			s.dispatch("feat/trivial", 1000, map[string]any{"plan": "required", "tier": "trivial"})
			s.msg("worker:feat/trivial#s1", "metrics:c1", 1200, `{"plan_critic_first_pass":null}`)
			s.status("worker:feat/trivial#s1", 1300, "done")
		}, ""},
		{"review task_kind does not flag", func(s *busSeed) {
			s.dispatch("feat/review", 1000, map[string]any{"plan": "required", "task_kind": "review"})
			s.msg("worker:feat/review#s1", "metrics:c1", 1200, `{"plan_critic_first_pass":null}`)
			s.status("worker:feat/review#s1", 1300, "done")
		}, ""},
		{"a dispatch-resume row in the window does not flag", func(s *busSeed) {
			s.dispatch("feat/resumewin", 1000, map[string]any{"plan": "required"})
			s.resume("feat/resumewin", 1100)
			s.msg("worker:feat/resumewin#s2", "metrics:c1", 1200, `{"plan_critic_first_pass":null}`)
			s.status("worker:feat/resumewin#s2", 1300, "done")
		}, ""},
		{"resume:true on the dispatch row does not flag", func(s *busSeed) {
			s.dispatch("feat/resumeflag", 1000, map[string]any{"plan": "required", "resume": true})
			s.msg("worker:feat/resumeflag#s1", "metrics:c1", 1200, `{"plan_critic_first_pass":null}`)
			s.status("worker:feat/resumeflag#s1", 1300, "done")
		}, ""},
		{"a recorded verdict does not flag", func(s *busSeed) {
			s.dispatch("feat/verdicted", 1000, map[string]any{"plan": "required"})
			s.msg("worker:feat/verdicted#s1", "metrics:c1", 1200, `{"plan_critic_first_pass":"accept"}`)
			s.status("worker:feat/verdicted#s1", 1300, "done")
		}, ""},
		{"a plan seam in the window does not flag", func(s *busSeed) {
			s.dispatch("feat/x", 1000, map[string]any{"plan": "required"})
			s.msg("worker:feat/x#s1", "review:c1", 1100, `{"seam":"plan","plan_critic_first_pass":"accept"}`)
			s.msg("worker:feat/x#s1", "metrics:c1", 1200, nullVerdict)
			s.status("worker:feat/x#s1", 1300, "done")
		}, ""},
		{"a malformed plan seam still flags", func(s *busSeed) {
			s.dispatch("feat/x", 1000, map[string]any{"plan": "required"})
			s.msg("worker:feat/x#s1", "review:c1", 1100, `{"seam":"plan","plan_critic_first_pass":"maybe"}`)
			s.msg("worker:feat/x#s1", "metrics:c1", 1200, nullVerdict)
			s.status("worker:feat/x#s1", 1300, "done")
		}, "feat/x\tclaude\tsonnet\tstandard\tdone\t" + flagged},
		{"plan provided does not flag", func(s *busSeed) {
			s.dispatch("feat/planprovided", 1000, map[string]any{"plan": "provided"})
			s.msg("worker:feat/planprovided#s1", "metrics:c1", 1200, `{"plan_critic_first_pass":null}`)
			s.status("worker:feat/planprovided#s1", 1300, "done")
		}, ""},
		{"no metrics snapshot at all does not flag", func(s *busSeed) {
			s.dispatch("feat/nometrics", 1000, map[string]any{"plan": "required"})
			s.status("worker:feat/nometrics#s1", 1300, "failed")
		}, ""},
		{"a run that stopped before the review gate does not flag", func(s *busSeed) {
			s.dispatch("feat/noreviewgate", 1000, map[string]any{"plan": "required"})
			s.msg("worker:feat/noreviewgate#s1", "metrics:c1", 1200, `{"plan_critic_first_pass":null,"review_mode":"none"}`)
			s.status("worker:feat/noreviewgate#s1", 1300, "blocked")
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, s := newRetroSeed(t)
			tt.seed(s)
			want := []string{retroHeader}
			if tt.want != "" {
				want = append(want, tt.want)
			}
			wantLines(t, retroOK(t, h), want...)
		})
	}

	t.Run("an out-of-window resume does not suppress a later independent run", func(t *testing.T) {
		h, s := newRetroSeed(t)
		s.dispatch("feat/indep", 1000, map[string]any{"plan": "required"})
		s.resume("feat/indep", 1100)
		s.msg("worker:feat/indep#s2", "metrics:c1", 1200, `{"plan_critic_first_pass":"accept"}`)
		s.status("worker:feat/indep#s2", 1300, "done")
		s.dispatch("feat/indep", 5000, map[string]any{"plan": "required"})
		s.msg("worker:feat/indep#s3", "metrics:c1", 5200, nullVerdict)
		s.status("worker:feat/indep#s3", 5300, "done")

		if !retroJSON(t, h).hasTag(flagged) {
			t.Errorf("%s not flagged for the later run", flagged)
		}
	})
}

func TestRetroJSONRowsAreOneRunRowAndOneDispatcherRow(t *testing.T) {
	h, s := newRetroSeed(t)
	s.dispatch("feat/rows", 1000, nil)
	s.msg("worker:feat/rows#s1", "retro:c1", 1200, note("gate_thrash", "circled build"))
	s.status("worker:feat/rows#s1", 1400, "done")
	s.msg("dispatcher:c1", "retro:c1", 5100, `{"seam":"drained","tag":"session_summary","detail":"one worker, one thrash"}`)
	s.msg("dispatcher:c1", "retro:c1", 5000, `{"seam":"drained","tag":"misrouted","detail":"trivial, but the diff touched auth"}`)

	r := retroJSON(t, h)
	if len(r.Rows) != 2 {
		t.Fatalf("rows = %v, want 2", r.Rows)
	}
	for i, want := range []string{
		`{"kind":"run","crew":"c1","branch":"feat/rows","engine":"claude","model":"sonnet",
		  "tier":"standard","outcome":"done","t0":1000,
		  "notes":[{"seam":"execute","tag":"gate_thrash","detail":"circled build"}]}`,
		`{"kind":"dispatcher","crew":"c1","branch":"dispatcher:c1",
		  "engine":"—","model":"—","tier":"—","outcome":"—","t0":5000,
		  "notes":[
		    {"seam":"drained","tag":"misrouted","detail":"trivial, but the diff touched auth"},
		    {"seam":"drained","tag":"session_summary","detail":"one worker, one thrash"}]}`,
	} {
		var wantRow map[string]any
		if err := json.Unmarshal([]byte(want), &wantRow); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(r.Rows[i])
		wantJSON, _ := json.Marshal(wantRow)
		if string(got) != string(wantJSON) {
			t.Errorf("rows[%d] =\n%s\nwant\n%s", i, got, wantJSON)
		}
	}

	// .tags/.unknown keep their existing meaning alongside the rows field.
	if g := r.tag(t, "gate_thrash"); g["hits"] != float64(1) || g["runs"] != float64(1) {
		t.Errorf("gate_thrash = %v", g)
	}
	if r.tag(t, "misrouted")["runs"] != float64(0) || r.tag(t, "session_summary")["runs"] != float64(0) || len(r.Unknown) != 0 {
		t.Errorf("dispatcher tags = %v, unknown %v", r.Tags, r.Unknown)
	}
}

func TestRetroJSONWithoutReportIsAnError(t *testing.T) {
	h := newHoldCLI(t)
	out, errOut, code := h.run("retro", "--json")
	if code != 1 || out != "" || errOut != "crew: retro takes --report and --json\n" {
		t.Errorf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestRetroUnknownFlagAndPositionalAreTheSameError(t *testing.T) {
	h := newHoldCLI(t)
	for _, args := range [][]string{{"retro", "--bogus"}, {"retro", "c1"}} {
		out, errOut, code := h.run(args...)
		if code != 1 || out != "" || errOut != "crew: retro takes --report and --json\n" {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestRetroMissingBusExitsZeroWithNoOutput(t *testing.T) {
	h := newHoldCLI(t)
	for _, args := range [][]string{{"retro"}, {"retro", "--report"}, {"retro", "--report", "--json"}, {"retro", "--draft", "--crew", "c1"}} {
		out, errOut, code := h.run(args...)
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

// anyStrings reads a decoded JSON array of strings, failing on any other shape.
func anyStrings(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not a JSON array", v)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		if out[i], ok = e.(string); !ok {
			t.Fatalf("element %d of %v is not a string", i, arr)
		}
	}
	return out
}

// --draft: one crew's session_summary note, printed ready to post.

// The --draft refusals, one line each like retroUsage's.
const (
	retroDraftPair   = "crew: retro --draft takes no --report/--json"
	retroDraftOnly   = "crew: retro --crew/--all need --draft"
	retroDraftNoCrew = "crew: retro --draft needs --crew ID or CREW_ID"
)

// statusBody seeds a worker status whose body carries more than the state: the
// pr_url a pr_open posts, or the source a reap or a watchdog posted a rewrite
// under.
func (b *busSeed) statusBody(from string, ts int, body map[string]any) {
	b.t.Helper()
	b.add(map[string]any{"ts": ts, "from": from, "to": "dispatcher:c1", "kind": "status", "body": body})
}

// wid is the worker id a branch's own rows are posted from.
func wid(branch, session string) string { return "worker:" + branch + "#" + session }

// draftDispatch seeds a dispatch row with the codename and the
// {tier,engine,model} tuple --draft prints for that branch.
func draftDispatch(b *busSeed, branch string, ts int, codename, tier, engine, model string) {
	b.t.Helper()
	b.dispatch(branch, ts, map[string]any{
		"name": codename, "color": "grey", "tmux": "colour244",
		"tier": tier, "engine": engine, "model": model,
	})
}

// summaryNote is a session_summary note in the shape the dispatcher posts one.
func summaryNote(detail string) string {
	b, _ := json.Marshal(map[string]string{"seam": "dispatch", "tag": "session_summary", "detail": detail})
	return string(b)
}

// draftRun runs `crew retro --draft args...` with the probes the roster fold
// needs: the codename --draft prints is roster's, so that fold runs for real
// against a faked tmux.
func draftRun(t *testing.T, h *holdCLI, args ...string) (string, string, int) {
	t.Helper()
	e := env{getwd: func() (string, error) { return h.repo, nil }, probes: noTouchProbes()}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"retro", "--draft"}, args...), &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

// draftNote runs --draft and reads the one note back: its seam, tag, and the
// segments of its detail.
func draftNote(t *testing.T, h *holdCLI, args ...string) (string, string, []string) {
	t.Helper()
	out, errOut, code := draftRun(t, h, args...)
	if code != 0 || errOut != "" {
		t.Fatalf("retro --draft %v: code %d, stderr %q", args, code, errOut)
	}
	body := strings.TrimSuffix(out, "\n")
	if strings.Contains(body, "\n") {
		t.Fatalf("retro --draft %v printed %d lines, want one:\n%s", args, strings.Count(body, "\n")+1, body)
	}
	var n struct {
		Seam   string `json:"seam"`
		Tag    string `json:"tag"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		t.Fatalf("retro --draft %v is no JSON object: %v\n%s", args, err, body)
	}
	if n.Detail == "" {
		return n.Seam, n.Tag, nil
	}
	return n.Seam, n.Tag, strings.Split(n.Detail, "; ")
}

func wantDetail(t *testing.T, parts, want []string) {
	t.Helper()
	if strings.Join(parts, "; ") != strings.Join(want, "; ") {
		t.Errorf("detail =\n%v\nwant\n%v", parts, want)
	}
}

// draftParts runs --draft and returns just the segments of its detail.
func draftParts(t *testing.T, h *holdCLI, args ...string) []string {
	t.Helper()
	_, _, parts := draftNote(t, h, args...)
	return parts
}

func TestRetroDraftPrintsOneCrewsSummary(t *testing.T) {
	h, s := newRetroSeed(t)
	draftDispatch(s, "feat/1186-scope-guard", 1000, "onyx", "standard", "pi", "lemonade/qwen")
	s.statusBody(wid("feat/1186-scope-guard", "s1"), 1100, map[string]any{
		"state": "pr_open", "pr_url": "https://github.com/noamsto/dispatcher/pull/1192"})
	s.msg(wid("feat/1186-scope-guard", "s1"), "retro:c1", 1200, note("misrouted", "guard-relaxing edit"))
	// A dispatcher note naming the branch by issue number lands on its line.
	s.msg("dispatcher:c1", "retro:c1", 1300, note("spec_too_thin", "onyx #1186 {standard,pi,lemonade/qwen} blocked"))
	// `other` is the catch-all, so it names no pattern on anyone's line.
	s.msg("dispatcher:c1", "retro:c1", 1400, note("other", "and one more thing about #1186"))

	draftDispatch(s, "feat/1190-other", 1500, "teal", "trivial", "claude", "sonnet")
	s.status(wid("feat/1190-other", "s1"), 1600, "done")

	seam, tag, parts := draftNote(t, h, "--crew", "c1")
	if seam != "dispatch" || tag != "session_summary" {
		t.Errorf("seam %q tag %q, want dispatch and session_summary", seam, tag)
	}
	wantDetail(t, parts, []string{
		"onyx {standard,pi,lemonade/qwen} pr_open PR #1192 [misrouted,spec_too_thin]",
		"teal {trivial,claude,sonnet} done",
	})
}

func TestRetroDraftStartsAfterTheCrewsNewestSessionSummary(t *testing.T) {
	h, s := newRetroSeed(t)
	draftDispatch(s, "feat/1180-old", 1000, "teal", "standard", "claude", "sonnet")
	s.status(wid("feat/1180-old", "s1"), 1050, "done")
	s.msg("dispatcher:c1", "retro:c1", 1500, summaryNote("teal #1180 {standard,claude,sonnet} done"))
	draftDispatch(s, "feat/1181-new", 2000, "birch", "standard", "claude", "opus")
	s.status(wid("feat/1181-new", "s1"), 2050, "done")
	// A summary a worker posted is not the dispatcher's: the boundary stays 1500.
	s.msg(wid("feat/1181-new", "s1"), "retro:c1", 3000, summaryNote("self-reported"))

	_, _, parts := draftNote(t, h, "--crew", "c1")
	wantDetail(t, parts, []string{"birch {standard,claude,opus} done"})

	_, _, all := draftNote(t, h, "--crew", "c1", "--all")
	wantDetail(t, all, []string{
		"teal {standard,claude,sonnet} done",
		"birch {standard,claude,opus} done",
	})
}

func TestRetroDraftOutcomeIsTheWorkersOwnLastRow(t *testing.T) {
	h, s := newRetroSeed(t)
	draftDispatch(s, "feat/1186-scope-guard", 1000, "onyx", "standard", "claude", "sonnet")
	s.statusBody(wid("feat/1186-scope-guard", "s1"), 1100, map[string]any{
		"state": "pr_open", "pr_url": "https://github.com/noamsto/dispatcher/pull/1192"})
	// A watchdog's rewrite and a reap's row are not the worker's own last word,
	// and a role pane's row is not the worker's at all.
	s.statusBody(wid("feat/1186-scope-guard", "s1"), 1200, map[string]any{"state": "blocked", "source": "watchdog"})
	s.statusBody(wid("feat/1186-scope-guard", "s1"), 1300, map[string]any{"state": "reaped", "source": "reap"})
	s.statusBody("role:feat/1186-scope-guard:reviewer", 1350, map[string]any{"state": "done"})

	draftDispatch(s, "feat/1187-only-reaped", 1400, "teal", "standard", "claude", "sonnet")
	s.statusBody(wid("feat/1187-only-reaped", "s1"), 1500, map[string]any{"state": "reaped", "source": "reap"})

	// A branch dispatched but never reporting still gets its line.
	draftDispatch(s, "feat/1188-silent", 1600, "moss", "deep", "codex", "sol")

	wantDetail(t, draftParts(t, h, "--crew", "c1"), []string{
		"onyx {standard,claude,sonnet} pr_open PR #1192",
		"teal {standard,claude,sonnet} —",
		"moss {deep,codex,sol} —",
	})
}

func TestRetroDraftNamesTheTagsTwoWorkersCarried(t *testing.T) {
	h, s := newRetroSeed(t)
	branches := map[string]string{"feat/1186-one": "onyx", "feat/1187-two": "teal", "feat/1188-three": "birch"}
	for i, branch := range []string{"feat/1186-one", "feat/1187-two", "feat/1188-three"} {
		ts := 1000 + i*100
		draftDispatch(s, branch, ts, branches[branch], "standard", "claude", "sonnet")
		s.status(wid(branch, "s1"), ts+10, "done")
	}
	own := func(branch, tag string, ts int) {
		s.msg(wid(branch, "s1"), "retro:c1", ts, note(tag, "detail"))
	}
	own("feat/1186-one", "spec_too_thin", 1400)
	own("feat/1187-two", "spec_too_thin", 1410)
	// Twice on one worker is one worker, and so no pattern.
	own("feat/1186-one", "gate_thrash", 1420)
	own("feat/1186-one", "gate_thrash", 1430)
	own("feat/1188-three", "misrouted", 1440)
	// A note addressed to another crew's retro sink is no evidence here, even under
	// the same branch name: it would tag onyx and fake a pattern.
	s.msg(wid("feat/1186-one", "s1"), "retro:c2", 1450, note("misrouted", "another crew"))

	_, _, parts := draftNote(t, h, "--crew", "c1")
	wantDetail(t, parts, []string{
		"onyx {standard,claude,sonnet} done [spec_too_thin,gate_thrash]",
		"teal {standard,claude,sonnet} done [spec_too_thin]",
		"birch {standard,claude,sonnet} done [misrouted]",
		"tags on ≥2 workers: spec_too_thin",
	})
}

func TestRetroDraftCodenameCarriesRostersCollisionSuffix(t *testing.T) {
	h, s := newRetroSeed(t)
	// Two branches hashed to one codename: `crew roster` prints each with its
	// issue number, and --draft prints what the roster prints.
	draftDispatch(s, "feat/1186-one", 1000, "onyx", "standard", "claude", "sonnet")
	s.status(wid("feat/1186-one", "s1"), 1100, "done")
	draftDispatch(s, "feat/1187-two", 1200, "onyx", "standard", "claude", "opus")
	s.status(wid("feat/1187-two", "s1"), 1300, "pr_open")

	wantDetail(t, draftParts(t, h, "--crew", "c1"), []string{
		"onyx·1186 {standard,claude,sonnet} done",
		"onyx·1187 {standard,claude,opus} pr_open",
	})
}

func TestRetroDraftNothingInScopePrintsNothing(t *testing.T) {
	h, s := newRetroSeed(t)
	// Another crew's branch, and this crew's branch already written up.
	s.raw(`{"ts":1000,"kind":"dispatch","crew_id":"c2","branch":"feat/1200-other",` +
		`"tier":"deep","engine":"claude","model":"opus"}`)
	draftDispatch(s, "feat/1180-old", 1100, "teal", "standard", "claude", "sonnet")
	s.status(wid("feat/1180-old", "s1"), 1200, "done")
	s.msg("dispatcher:c1", "retro:c1", 2000, summaryNote("teal #1180 done"))

	for _, args := range [][]string{{"--crew", "c1"}, {"--crew", "c9"}} {
		if out, errOut, code := draftRun(t, h, args...); out != "" || errOut != "" || code != 0 {
			t.Errorf("retro --draft %v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestRetroDraftCrewDefaultsToTheEnvironment(t *testing.T) {
	h, s := newRetroSeed(t)
	draftDispatch(s, "feat/1186-scope-guard", 1000, "onyx", "standard", "claude", "sonnet")
	s.status(wid("feat/1186-scope-guard", "s1"), 1100, "done")

	t.Setenv("CREW_ID", "c1")
	wantDetail(t, draftParts(t, h), []string{"onyx {standard,claude,sonnet} done"})

	// --crew wins over it, and a crew with no rows prints nothing.
	if out, errOut, code := draftRun(t, h, "--crew", "c2"); out != "" || errOut != "" || code != 0 {
		t.Errorf("--crew c2: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// TestRetroDraftSaysWhenTheRosterFoldFails pins the degraded path: with no roster
// rows the draft still prints, naming each branch by the identity it read off its
// own dispatch row, and says the roster failed.
func TestRetroDraftSaysWhenTheRosterFoldFails(t *testing.T) {
	h, s := newRetroSeed(t)
	draftDispatch(s, "feat/1186-scope-guard", 1000, "onyx", "standard", "claude", "sonnet")
	// An exited row is what makes the roster read `git worktree list`.
	s.status(wid("feat/1186-scope-guard", "s1"), 1100, "exited")

	e := env{getwd: func() (string, error) { return h.repo, nil }, probes: func(context.Context, string) roster.Probes {
		return roster.Probes{
			Tmux:      &tmuxtest.Fake{},
			Worktrees: func() (string, error) { return "", errors.New("git worktree list failed") },
		}
	}}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"retro", "--draft", "--crew", "c1"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "crew: retro: roster:") {
		t.Errorf("stderr %q, want the roster failure named", stderr.String())
	}
	if want := `"detail":"onyx {standard,claude,sonnet} exited"`; !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout %q, want the summary on the identity name (%s)", stdout.String(), want)
	}
}

func TestRetroDraftUsageErrors(t *testing.T) {
	h := newHoldCLI(t)
	for _, tc := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"--draft", "--report"}, retroDraftPair},
		{[]string{"--draft", "--json"}, retroDraftPair},
		{[]string{"--draft", "--report", "--json"}, retroDraftPair},
		{[]string{"--crew", "c1"}, retroDraftOnly},
		{[]string{"--all"}, retroDraftOnly},
		{[]string{"--draft", "--crew", "c1", "--report"}, retroDraftPair},
		{[]string{"--draft"}, retroDraftNoCrew},
	} {
		out, errOut, code := h.run(append([]string{"retro"}, tc.args...)...)
		if code != 1 || out != "" || errOut != tc.stderr+"\n" {
			t.Errorf("retro %v: code %d, stdout %q, stderr %q, want 1 and %q", tc.args, code, out, errOut, tc.stderr)
		}
	}
}
