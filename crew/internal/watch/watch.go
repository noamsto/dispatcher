// Package watch is `crew watch`: the dispatcher's park. Block until a worker
// event qualifies — a status in --states, or a msg to the dispatcher — print
// {"cursor":…,"events":[…]} on one line, advance the crew's cursor file and
// exit 0. An expired park prints nothing on stdout (that is the marker, so a
// backgrounded park is not a failed command) and one line on stderr.
//
// The fold is the arm's jq — same decisions, one pass since #910 — embedded
// and handed to jqrun (see watch.jq).
// Two contracts with `crew stream`, which re-enters this command as its child,
// shape the rest of the file:
//
//   - The clock is the wall clock. The arm stamped `start` and every deadline
//     check with jq's `now*1000|floor` and slept with the real `sleep`, so
//     unlike `await` and `hold` this park never reads $CREW_CLOCK.
//   - `watch.lock.d` is held for the whole park through the same internal/lock
//     protocol `crew stream` takes. It stops and retries by sending TERM, and
//     watch releases the lock itself (a deferred release, where the bash arm had
//     an EXIT trap); a watch killed before it releases leaves its record behind
//     until the next watch replaces it.
package watch

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

//go:embed watch.jq
var program string

const (
	defaultStates  = "blocked,pr_open,done,failed,exited"
	defaultTimeout = "3300"
	defaultInterv  = "2"

	noCrewMsg  = "crew: CREW_ID unset and no WORKER_TASK.md crew_id"
	badCrewMsg = "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'"
)

// Options is everything Run reads beyond the bus: how to resolve the crew the
// arm defaults to (`_crew_id`), how to put the buffered stdout on the wire (the
// cursor moves only once the batch is on it), the clock and the interval wait.
type Options struct {
	CrewID func() string
	Flush  func() error
	// Now is the real clock: the arm timed its deadline with jq's `now` and
	// never read $CREW_CLOCK, which the suite exports for `await`'s sake.
	Now func() time.Time
	// Sleep waits one interval. nil is clock.Clock.Sleep with no CREW_CLOCK —
	// the real `sleep` with the same argv, so an interval coreutils rejects
	// costs its own message and status 1, which is what killed the arm under
	// `set -e`. Tests stub it so a poll loop does not wait.
	Sleep func(interval string, stderr io.Writer) error
	// JQColorsInvalid is $JQ_COLORS holding something jq could not parse.
	JQColorsInvalid bool
}

// call is one parsed invocation. sinceText and timeoutText stay the caller's
// bytes: the expiry line prints them verbatim, as the arm's `$since` and
// `${timeout}s` did.
type call struct {
	sinceText, timeoutText, statesText, interval, crew string
	sinceSet                                           bool
	states                                             []string
	since, timeout                                     int64
}

// Run is the arm, flag for flag and refusal for refusal.
func Run(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = clock.Clock{Now: o.Now}.Sleep
	}
	// The arm's first statement, before it had read a flag: every later path
	// writes under here.
	_ = os.MkdirAll(paths.Dir, 0o755)

	c, msg := check(req, o)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return 1
	}
	cdir := paths.CrewDir(c.crew)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}

	wlock := filepath.Join(cdir, "watch.lock.d")
	if !lock.Acquire(wlock, strconv.Itoa(os.Getpid())) {
		say(stderr, "crew: another watch is already running for this crew (%s)\n", c.crew)
		return 1
	}
	defer lock.Release(wlock)

	// Without --since the cursor self-seeds from the crew's file, so a stale
	// caller cursor cannot re-deliver. A file that is missing, empty or not all
	// digits seeds 0: `$(cat …)` plus the arm's case. Read under the lock: a seed
	// taken before it could predate a consumer that delivered and released in
	// between, and would re-deliver that batch.
	if !c.sinceSet {
		c.sinceText = seedCursor(filepath.Join(cdir, "cursor"))
		c.since = parseIntSaturate(c.sinceText)
	}

	// `stream` stops and retries by sending TERM to this process while it is
	// parked, and bash ran its EXIT trap for TERM, INT and HUP alike. Go's
	// default would die holding the lock, so the signals are caught and acted
	// on at the loop boundary — where bash acted on them, once the command in
	// flight (the `sleep`) had returned.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	realMS := clock.Clock{Now: o.Now}.RealMS
	// The arm's one jq whose stderr it did not throw away was
	// `start=$(jq -nc 'now*1000|floor')`, so a bad $JQ_COLORS surfaced once for
	// the call — await's rule, and hold's. The fold's jq never started a process.
	warned := false
	nowMS := func() int64 {
		if !warned && o.JQColorsInvalid {
			warned = true
			say(stderr, "Failed to set $JQ_COLORS\n")
		}
		return realMS()
	}

	start := nowMS()
	// A stream-spawned inner watch names its owner in CREW_WATCH_PARENT. Once
	// the owner is gone the watch is an orphan: it must not fold or advance the
	// cursor, or it would swallow events a restarted lane should deliver. A
	// directly armed `crew watch` carries no such variable and is unchecked.
	owner := 0
	if v := os.Getenv("CREW_WATCH_PARENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			owner = n
		}
	}
	orphaned := func() bool { return owner != 0 && os.Getppid() != owner }
	// The arm's `$((start + timeout * 1000))` in bash's intmax_t. Go's int64
	// wraps the same way, and a product that overflows lands in the past: an
	// expiry, never the unbounded park the flag exists to prevent.
	deadline := start + c.timeout*1000

	for {
		if s, ok := pending(sigs); ok {
			return signalStatus(s)
		}
		if orphaned() {
			return 1
		}
		if folded, ok := fold(paths.Log, c); ok {
			out := jsonval.Clamp(folded).(map[string]any)
			if err := jsonval.Encode(stdout, out, false); err != nil {
				say(stderr, "crew: %v\n", err)
				return 1
			}
			// On the wire before the cursor moves: a reader that sees the new
			// cursor has to have been able to see the batch it came from.
			if o.Flush != nil {
				if err := o.Flush(); err != nil {
					return 1
				}
			}
			// Re-checked here as well as at the loop top: the owner can die while
			// the fold runs, and an orphan must not advance the cursor.
			if orphaned() {
				return 1
			}
			writeCursor(paths.Dir, filepath.Join(cdir, "cursor"), cursorText(out))
			return 0
		}
		if now := nowMS(); now >= deadline {
			say(stderr, "crew: watch park ended after %ss — no new events (cursor %s)\n",
				c.timeoutText, c.sinceText)
			return 0
		}
		if err := o.Sleep(c.interval, stderr); err != nil {
			// `set -e` ended the arm with whatever `sleep` returned, so the
			// status is the child's: 1 for an interval it rejects, and 128+n
			// when a group-directed signal (a terminal's Ctrl-C) killed it out
			// from under the park. A signal aimed at this process too is already
			// queued, and reports the same number either way.
			if s, ok := pending(sigs); ok {
				return signalStatus(s)
			}
			return exitStatus(err)
		}
	}
}

// Request is the typed watch call. Since nil seeds the cursor from the crew's
// file; every other "" is the default (3300 seconds, the five terminal states,
// interval 2, the checkout's crew).
type Request struct {
	Since                           *string
	States, Timeout, Interval, Crew string
}

// check judges a request in the arm's order: since, timeout (digits, then > 0),
// states, the crew, and the crew's charset.
func check(r Request, o Options) (call, string) {
	c := call{
		sinceText:   "0",
		timeoutText: orDefault(r.Timeout, defaultTimeout),
		statesText:  orDefault(r.States, defaultStates),
		interval:    orDefault(r.Interval, defaultInterv),
		crew:        r.Crew,
	}
	if r.Since != nil {
		c.sinceText, c.sinceSet = *r.Since, true
	}
	if !isDigits(c.sinceText) {
		return c, "crew: --since must be an integer ms timestamp"
	}
	if !isDigits(c.timeoutText) {
		return c, "crew: --timeout must be a positive integer number of seconds"
	}
	// The arm's `[ "$timeout" -gt 0 ]`, on the same digits. bash's `test` errors
	// past its signed long and `set -e` turned that into this same refusal, so
	// out-of-range is refused rather than parked on. (bash's own `[: … integer
	// expected` diagnostic is a shell message and is not mirrored.)
	timeout, err := strconv.ParseInt(c.timeoutText, 10, 64)
	if err != nil || timeout == 0 {
		return c, "crew: --timeout must be > 0 (indefinite watch unsupported: a reaped watch would be undetectable)"
	}
	c.timeout = timeout
	c.states = splitStates(c.statesText)
	if len(c.states) == 0 {
		return c, "crew: --states must be non-empty"
	}
	if c.crew == "" {
		c.crew = o.CrewID()
	}
	if c.crew == "" {
		return c, noCrewMsg
	}
	if !bus.ValidCrewID(c.crew) {
		return c, badCrewMsg
	}
	c.since = parseIntSaturate(c.sinceText)
	return c, ""
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// splitStates is the arm's `jq -Rc 'split(",") | map(select(length>0))'`, so
// `a,,b` is two states and `,` alone is the empty set it refuses.
func splitStates(text string) []string {
	var out []string
	for _, s := range strings.Split(text, ",") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fold is the arm's one pass over the log: `jq -c -s` on the file, so a missing
// or non-regular log, an unreadable one and one it cannot parse all fold to
// "nothing yet" — the arm ran jq under `2>/dev/null || true` and kept polling.
func fold(path string, c call) (map[string]any, bool) {
	events, err := bus.ReadEvents(path)
	if err != nil {
		return nil, false
	}
	in := make([]any, len(events))
	for i, ev := range events {
		in[i] = ev.Doc
	}
	states := make([]any, len(c.states))
	for i, s := range c.states {
		states[i] = s
	}
	res, err := jqrun.RunAny(program, in, 0, map[string]any{
		"crew":   c.crew,
		"me":     "dispatcher:" + c.crew,
		"since":  json.Number(strconv.FormatInt(c.since, 10)),
		"states": states,
	})
	if err != nil {
		return nil, false
	}
	out, ok := res.(map[string]any)
	return out, ok
}

// cursorText is `jq -r '.cursor'`: the batch's cursor as the number text the
// cursor file holds.
func cursorText(out map[string]any) string {
	b, err := json.Marshal(out["cursor"])
	if err != nil {
		return ""
	}
	return string(b)
}

// writeCursor is the arm's `mktemp "$dir/.cursor.XXXXXX"` + `printf` + `mv -f`:
// one tmp file under the bus dir renamed over the crew's cursor, so a reader
// never sees a half-written cursor.
func writeCursor(dir, path, text string) {
	tmp, err := os.CreateTemp(dir, ".cursor.")
	if err != nil {
		return
	}
	var failed bool
	if _, err := tmp.WriteString(text + "\n"); err != nil {
		failed = true
	}
	if err := tmp.Close(); err != nil {
		failed = true
	}
	if failed {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

// seedCursor is `$(cat "$cursor_file" 2>/dev/null || true)` plus the arm's
// `case "$seed" in ”|*[!0-9]*) seed=0`: the file's text without its trailing
// newlines, and 0 for anything that is not all digits.
func seedCursor(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "0"
	}
	seed := strings.TrimRight(string(data), "\n")
	if !isDigits(seed) {
		return "0"
	}
	return seed
}

// isDigits is the arm's `case … in ”|*[!0-9]*)` rejection: one digit or more,
// digits only. Empty fails, which is what makes a missing cursor 0.
func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseIntSaturate reads the arm's `--argjson since`, whose input the arm had
// already narrowed to digits. A value past int64 is the one jq divergence here:
// jq widened it to a double and the park found nothing newer, so saturating
// lands in the same place. (`--timeout` is checked separately, and refused.)
func parseIntSaturate(text string) int64 {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return math.MaxInt64
	}
	return n
}

// exitStatus is what `set -e` would have exited the arm with: the child's own
// status, or 128+n when it died of a signal. Anything Go could not read a
// status from (the exec itself failed) is the shell's generic 1.
func exitStatus(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}

// signalStatus is the conventional 128+n a shell reports for a command that
// died of that signal — the status `stream`'s `wait` saw from the bash arm.
func signalStatus(s os.Signal) int {
	if sig, isSig := s.(syscall.Signal); isSig {
		return 128 + int(sig)
	}
	return 1
}

// pending is a non-blocking look at the signal channel: the park acts on a
// caught signal at the loop boundary, never mid-fold.
func pending(sigs <-chan os.Signal) (os.Signal, bool) {
	select {
	case s := <-sigs:
		return s, true
	default:
		return nil, false
	}
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
