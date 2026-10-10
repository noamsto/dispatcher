package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// alive is _pid_alive over just the pids a fixture names live.
func alive(live ...string) func(int) bool {
	set := map[string]bool{}
	for _, p := range live {
		set[p] = true
	}
	return func(pid int) bool { return set[fmt.Sprint(pid)] }
}

func fixture(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	return bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
}

// writeLock plants a stream.lock.d whose pid file holds exactly text — the
// bytes `--status` and `--force` have to judge, newlines included.
func writeLock(t *testing.T, p bus.Paths, text string) string {
	t.Helper()
	lockd := filepath.Join(p.CrewDir("c1"), "stream.lock.d")
	if err := os.MkdirAll(lockd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockd, "pid"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return lockd
}

func writeTick(t *testing.T, p bus.Paths, text string) {
	t.Helper()
	cdir := p.CrewDir("c1")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "stream.tick"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runStatus is one `--status` call: its exit status and its one stdout line.
func runStatus(t *testing.T, p bus.Paths, live ...string) (int, map[string]any) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := Run([]string{"--status", "--crew", "c1"}, p, &out, &errb,
		Options{Probes: crews.Probes{Alive: alive(live...)}})
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("--status printed %q (%v), stderr %q", out.String(), err, errb.String())
	}
	return rc, line
}

func TestStatusNoLockIsDead(t *testing.T) {
	p := fixture(t)
	rc, line := runStatus(t, p)
	if rc != 2 {
		t.Errorf("status exit = %d, want 2", rc)
	}
	if line["state"] != "dead" || line["crew"] != "c1" || line["pid"] != nil || line["age_s"] != nil {
		t.Errorf("line = %v, want dead c1 with null pid and age", line)
	}
}

// A live holder is reported whether or not the tick saves it: `dead` names the
// pid that is gone, and `stale` the one that stopped ticking.
func TestStatusStates(t *testing.T) {
	p := fixture(t)
	writeLock(t, p, "4242\n")
	writeTick(t, p, `{"pid":4242,"ts":1,"park":1}`)
	rc, line := runStatus(t, p, "4242")
	if rc != 1 || line["state"] != "stale" {
		t.Errorf("aged tick = (%d, %v), want (1, stale)", rc, line["state"])
	}
	if age, ok := line["age_s"].(float64); !ok || age < 121 {
		t.Errorf("age_s = %v, want the tick's own age", line["age_s"])
	}
	if pid, ok := line["pid"].(float64); !ok || pid != 4242 {
		t.Errorf("pid = %v, want 4242", line["pid"])
	}

	// The tick's park, not this call's, sets the bound: 2×park+60.
	writeTick(t, p, fmt.Sprintf(`{"pid":4242,"ts":%d,"park":300}`, time.Now().UnixMilli()-350_000))
	if rc, line := runStatus(t, p, "4242"); rc != 0 || line["state"] != "alive" {
		t.Errorf("tick inside 2*300+60 = (%d, %v), want (0, alive)", rc, line["state"])
	}

	// A live lock with no tick yet is stale — the safe direction.
	cdir := p.CrewDir("c1")
	if err := os.Remove(filepath.Join(cdir, "stream.tick")); err != nil {
		t.Fatal(err)
	}
	if rc, line := runStatus(t, p, "4242"); rc != 1 || line["state"] != "stale" || line["age_s"] != nil {
		t.Errorf("no tick = (%d, %v, age %v), want (1, stale, null)", rc, line["state"], line["age_s"])
	}
	// Neither is a tick that will not decode, nor one with no park.
	writeTick(t, p, "not json")
	if rc, line := runStatus(t, p, "4242"); rc != 1 || line["state"] != "stale" {
		t.Errorf("garbage tick = (%d, %v), want (1, stale)", rc, line["state"])
	}
	writeTick(t, p, "5")
	if rc, line := runStatus(t, p, "4242"); rc != 1 || line["state"] != "stale" {
		t.Errorf("tick with no park = (%d, %v), want (1, stale)", rc, line["state"])
	}
}

// The three sanitised holder shapes — empty, non-numeric, and a literal 0 — are
// no holder at all, so the state is dead and the pid is null. A pid that is
// simply gone is dead too, but still named — and so is one past the pid ceiling,
// which a probe would answer for: syscall.Kill truncates to pid_t, making
// 4294967295 a kill(-1) that succeeds.
func TestStatusHolders(t *testing.T) {
	for _, tc := range []struct {
		holder string
		live   []string
		pid    any
	}{
		{"", nil, nil},
		{"0\n", []string{"0"}, nil},
		{"abc\n", []string{"abc"}, nil},
		{"999999\n", nil, float64(999999)},
		{"4242\n", nil, float64(4242)},
		{"4294967295\n", []string{"4294967295"}, float64(4294967295)},
		{"4194305\n", []string{"4194305"}, float64(4194305)},
	} {
		p := fixture(t)
		writeLock(t, p, tc.holder)
		rc, line := runStatus(t, p, tc.live...)
		if rc != 2 || line["state"] != "dead" {
			t.Errorf("holder %q = (%d, %v), want (2, dead)", tc.holder, rc, line["state"])
		}
		if line["pid"] != tc.pid {
			t.Errorf("holder %q pid = %v, want %v", tc.holder, line["pid"], tc.pid)
		}
	}
}

func TestRefusals(t *testing.T) {
	p := fixture(t)
	for _, tc := range []struct {
		argv []string
		msg  string
	}{
		{[]string{"--bogus"}, "crew: stream: unknown arg '--bogus'"},
		{[]string{"--crew"}, "crew: --crew needs a value"},
		{[]string{"--crew", ""}, "crew: --crew needs a value"},
		{[]string{"--park"}, "crew: --park needs a value"},
		{[]string{"--park", "0"}, "crew: --park must be a positive integer number of seconds"},
		{[]string{"--park", "-1"}, "crew: --park must be a positive integer number of seconds"},
		{[]string{"--park", "1x"}, "crew: --park must be a positive integer number of seconds"},
		{[]string{"--heartbeat", "0"}, "crew: --heartbeat must be a positive integer number of seconds"},
		{[]string{"--coalesce", "abc"}, "crew: --coalesce must be a positive integer number of seconds"},
		{[]string{"--retry", "0"}, "crew: --retry must be a positive integer number of seconds"},
		{[]string{"--interval", "0"}, "crew: --interval must be a positive integer number of seconds"},
		// Non-negative, not positive: 0 is how the cadence reap is disabled.
		{[]string{"--reap-every", "-1"}, "crew: --reap-every must be a non-negative integer number of seconds"},
		{[]string{"--reap-every", "nope"}, "crew: --reap-every must be a non-negative integer number of seconds"},
		{[]string{"--states", ","}, "crew: --states must be non-empty"},
		// An empty value is "missing" to the arm's `[ -n "${2:-}" ]`, not the
		// empty set: the refusal is the flag's, before --states is judged.
		{[]string{"--states", ""}, "crew: --states needs a value"},
		{[]string{"--crew", "../../escaped"}, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'"},
		{[]string{"--crew", "-x"}, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'"},
		{[]string{"--status"}, "crew: CREW_ID unset and no WORKER_TASK.md crew_id"},
	} {
		var out, errb bytes.Buffer
		rc := Run(tc.argv, p, &out, &errb, Options{CrewID: func() string { return "" }})
		if rc != exitUsage {
			t.Errorf("%v exit = %d, want %d (%s)", tc.argv, rc, exitUsage, errb.String())
		}
		if got := strings.TrimSuffix(errb.String(), "\n"); got != tc.msg {
			t.Errorf("%v stderr = %q, want %q", tc.argv, got, tc.msg)
		}
		if out.Len() != 0 {
			t.Errorf("%v printed %q on stdout", tc.argv, out.String())
		}
	}
}

// --force signals one holder and nothing else: the arm's `case` guard plus the
// range a pid can be, since the kernel sees only the low 32 bits of the holder
// text — 4294967295 is kill(-1), every process this user may signal.
func TestSignalTargetIsTheOnlyTarget(t *testing.T) {
	for _, holder := range []string{"1", "4242", "4194304"} {
		pid, ok := signalTarget(holder)
		if !ok || strconv.Itoa(pid) != holder {
			t.Errorf("signalTarget(%q) = (%d, %t), want the pid it names", holder, pid, ok)
		}
	}
	for _, holder := range []string{"", "0", "00", "-1", "-0", "12x", "4194305", "4294967295", "9223372036854775807", "99999999999999999999999"} {
		if pid, ok := signalTarget(holder); ok {
			t.Errorf("signalTarget(%q) = (%d, true); this holder must never be a signal target", holder, pid)
		}
	}
}

// The flags the inner `crew watch` is re-entered with keep the caller's bytes,
// and the defaults are the arm's.
func TestParseKeepsInnerWatchTextVerbatim(t *testing.T) {
	c, msg := parse([]string{"--crew", "c1", "--park", "07", "--interval", "02", "--states", "a,,b"},
		Options{CrewID: func() string { return "" }})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	if c.parkText != "07" || c.intervalText != "02" || c.statesText != "a,,b" {
		t.Errorf("texts = %q %q %q", c.parkText, c.intervalText, c.statesText)
	}
	if c.park != 7 || c.interval != 2 || strings.Join(c.states, ",") != "a,b" {
		t.Errorf("values = %d %d %v", c.park, c.interval, c.states)
	}
	if c.reapEvery != defaultReapEvery || c.heartbeat != defaultHeartbeat ||
		c.coalesce != defaultCoalesce || c.retry != defaultRetry {
		t.Errorf("defaults = %d/%d/%d/%d", c.reapEvery, c.heartbeat, c.coalesce, c.retry)
	}
	if c.statesText != "a,,b" || c.parkText != "07" {
		t.Errorf("the inner watch must get the caller's text")
	}
	// --reap-every 0 parses: it disables the cadence reap.
	if c, msg := parse([]string{"--crew", "c1", "--reap-every", "0"}, Options{}); msg != "" || c.reapEvery != 0 {
		t.Errorf("--reap-every 0 = (%d, %q), want (0, \"\")", c.reapEvery, msg)
	}
	// A crew resolved from the environment still passes the charset guard.
	if c, msg := parse(nil, Options{CrewID: func() string { return "c9" }}); msg != "" || c.crew != "c9" {
		t.Errorf("CrewID fallback = (%q, %q)", c.crew, msg)
	}
}

func TestSuppressionRepeatAndClear(t *testing.T) {
	s := &suppress{key: map[string]string{}, ts: map[string]int64{}}
	if !s.repeat("watch", "1:x", 1000, 5000) {
		t.Fatal("a new key earns a line")
	}
	if s.repeat("watch", "1:x", 2000, 5000) {
		t.Error("the same key inside the window is silent")
	}
	if !s.repeat("watch", "1:x", 6001, 5000) {
		t.Error("the same key re-emits once --heartbeat has passed")
	}
	if !s.repeat("watch", "2:x", 6500, 5000) {
		t.Error("a different key earns a line at once")
	}
	if !s.repeat("watch", "1:x", 7000, 5000) {
		t.Error("a key that came back earns a line")
	}
	// The hold path clears rather than keeps, so a released hold that comes
	// back announces again.
	s.clear("hold")
	if s.key["hold"] != "" {
		t.Error("clear forgets the key")
	}
}

func TestWantsReap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch string
		want  bool
	}{
		{"done", `{"cursor":1,"events":[{"kind":"status","from":"w","body":{"state":"done"}}]}`, true},
		{"failed", `{"cursor":1,"events":[{"kind":"status","from":"w","body":{"state":"failed"}}]}`, true},
		{"exited", `{"cursor":1,"events":[{"kind":"status","from":"w","body":{"state":"exited"}}]}`, true},
		{"blocked", `{"cursor":1,"events":[{"kind":"status","from":"w","body":{"state":"blocked"}}]}`, false},
		{"pr merged", `{"cursor":1,"events":[{"kind":"msg","from":"pr-watch:1","body":"{\"changed\":[\"state\"],\"state\":{\"state\":\"MERGED\"}}"}]}`, true},
		{"pr open", `{"cursor":1,"events":[{"kind":"msg","from":"pr-watch:1","body":"{\"changed\":[\"state\"],\"state\":{\"state\":\"OPEN\"}}"}]}`, false},
		{"pr other change", `{"cursor":1,"events":[{"kind":"msg","from":"pr-watch:1","body":"{\"changed\":[\"head\"],\"state\":{\"state\":\"MERGED\"}}"}]}`, false},
		{"empty", `{"cursor":1,"events":[]}`, false},
		{"two lines, one terminal", "{\"cursor\":1,\"events\":[]}\n{\"cursor\":2,\"events\":[{\"kind\":\"status\",\"body\":{\"state\":\"done\"}}]}", true},
		{"not json", "not json", false},
		{"empty text", "", false},
	} {
		if got := wantsReap(tc.batch); got != tc.want {
			t.Errorf("%s: wantsReap = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReapLinesAndHoldIDs(t *testing.T) {
	// The arm's `sub("^crew reap: ";"')`, once per line, empty lines dropped.
	got := encode(reapLines("crew reap: reaped feat/x\nreaped feat/y\n\n"))
	if want := `["reaped feat/x","reaped feat/y"]`; got != want {
		t.Errorf("reapLines = %s, want %s", got, want)
	}
	ids := holdIDs(mustDecode(t, `[{"id":"h2"},{"id":"h1"},{"id":"h10"}]`))
	if ids != "h1,h10,h2" {
		t.Errorf("holdIDs = %q, want the ids sorted and joined", ids)
	}
	if ids := holdIDs(mustDecode(t, `[]`)); ids != "" {
		t.Errorf("holdIDs of no holds = %q", ids)
	}
}

func TestDigitsToN(t *testing.T) {
	for _, tc := range [][2]string{
		{"crew: watch park ended after 1s — no new events (cursor 1700000000000)",
			"crew: watch park ended after Ns — no new events (cursor N)"},
		{"no digits", "no digits"},
		{"12a34", "NaN"},
		{"", ""},
	} {
		if got := digitsToN(tc[0]); got != tc[1] {
			t.Errorf("digitsToN(%q) = %q", tc[0], got)
		}
	}
}

func TestFieldTextAndDigits(t *testing.T) {
	v := mustDecode(t, `{"n":1700000000000,"s":"done","f":1.5,"z":null,"o":{},"neg":-1}`)
	if got := fieldText(v, "s"); got != "done" {
		t.Errorf("fieldText(s) = %q", got)
	}
	if got := fieldText(v, "n"); got != "1700000000000" {
		t.Errorf("fieldText(n) = %q, want the digits the file holds", got)
	}
	for _, k := range []string{"z", "o", "missing", "neg", "f"} {
		if n, ok := digitsField(v, k); ok && k != "neg" && k != "f" {
			t.Errorf("digitsField(%s) = %d, want absent", k, n)
		}
	}
	// `jq -r` prints a number's own text; only digits-only survives the case.
	if got := fieldText(v, "f"); got != "1.5" {
		t.Errorf("fieldText(f) = %q", got)
	}
}

func TestFirstLineAndReadIfNonEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "err")
	if got := firstLine(path); got != "" {
		t.Errorf("missing file firstLine = %q", got)
	}
	if _, ok := readIfNonEmpty(path); ok {
		t.Error("a missing file is not non-empty")
	}
	writeFile(t, path, "")
	if _, ok := readIfNonEmpty(path); ok {
		t.Error("an empty file is not non-empty")
	}
	writeFile(t, path, "one\ntwo\n")
	if got := firstLine(path); got != "one" {
		t.Errorf("firstLine = %q", got)
	}
	// Command substitution's strip: the batch the stream prints is one line.
	if got, _ := readIfNonEmpty(path); got != "one\ntwo" {
		t.Errorf("readIfNonEmpty = %q, want the trailing newline stripped", got)
	}
	truncate(path)
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Errorf("truncate left %v bytes (%v)", info.Size(), err)
	}
}

func TestSweepReapedRemovesPairs(t *testing.T) {
	dir := t.TempDir()
	out, errf := filepath.Join(dir, "stream.reap.out"), filepath.Join(dir, "stream.reap.err")
	for _, name := range []string{out, errf, out + ".111", errf + ".111", out + ".222"} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sweepReaped(out, errf)
	left, _ := filepath.Glob(filepath.Join(dir, "stream.reap.*"))
	if len(left) != 0 {
		t.Errorf("sweepReaped left %v", left)
	}
	sweepReaped(out, errf) // idempotent: nothing to remove
}

// TestSignalReleasesEverything is the port's own contract, at the process
// level: TERM stops the inner watch, drains what it printed, unlinks the temp
// files and releases the lock — and leaves 0 behind, the status bash's handler
// ended with. crew.bats drives the same three signals end to end; this is the
// same claim without a bus.
func TestSignalReleasesEverything(t *testing.T) {
	p := fixture(t)
	script := filepath.Join(t.TempDir(), "crew-stub.sh")
	batch := `{"cursor":1,"events":[{"ts":1,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}]}`
	if err := os.WriteFile(script, []byte(
		"#!/usr/bin/env bash\ncase \"${1:-}\" in\n"+
			"  watch) printf '%s\\n' "+`'`+batch+`'`+"; exit 0 ;;\n"+
			"  hold) printf '[]\\n'; exit 1 ;;\n"+
			"  *) exit 0 ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := &lockedWriter{}
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"--crew", "c1", "--park", "1", "--interval", "1", "--coalesce", "1",
			"--heartbeat", "3600", "--retry", "1", "--reap-every", "0"},
			p, out, os.Stderr, Options{Self: script, Timer: func(time.Duration) <-chan time.Time { return ready() }})
	}()

	cdir := p.CrewDir("c1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(cdir, "stream.lock.d")); err == nil && out.len() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out.len() == 0 {
		t.Fatal("the stream printed no batch")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var rc int
	select {
	case rc = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not stop on TERM")
	}

	if rc != 0 {
		t.Errorf("exit = %d, want 0 (a caught signal is a deliberate stop)", rc)
	}
	if _, err := os.Stat(filepath.Join(cdir, "stream.lock.d")); !os.IsNotExist(err) {
		t.Error("stream.lock.d survived the cleanup")
	}
	for _, name := range []string{"stream.out", "stream.err", "stream.hold.err"} {
		if _, err := os.Stat(filepath.Join(cdir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the cleanup", name)
		}
	}
	// The tick stays: it is what `--status` reads after the process is gone.
	if _, err := os.Stat(filepath.Join(cdir, "stream.tick")); err != nil {
		t.Error("stream.tick was removed")
	}
	if lines := strings.Count(out.str(), "\n"); lines < 1 {
		t.Errorf("the batch was never printed: %q", out.str())
	}
}

// lockedWriter is a bytes.Buffer with the lock `go test -race` asks for: Run
// prints from its own goroutine while the test polls the same buffer.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

func (w *lockedWriter) str() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// ready is an already-fired timer: the loop's waits cost nothing, so a test
// spins the cycle rather than sleeping through it.
func ready() <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Now()
	return c
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustDecode(t *testing.T, text string) jsonv.Value {
	t.Helper()
	v, err := decodeOne(text)
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return v
}
