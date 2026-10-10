// Package crews is `crew crews`: the per-crew discovery table over a repo
// bus (#29) and the --mine ancestor scan. The two jq halves of the table are
// the original programs from adapters/core/crew.sh, run through gojq
// (crews_stats.jq, crews_final.jq). The #838 fix is structural: the stats
// map reaches the final pass as a jqrun variable, never a `--argjson stats`
// argv argument, which grew past MAX_ARG_STRLEN near ~1700 crews and killed
// the command with "Argument list too long". The dir scan and the pid
// liveness / ancestor probes stay Go, as in the arm's bash loop.
package crews

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed crews_stats.jq
var statsProgram string

//go:embed crews_final.jq
var finalProgram string

// header is the arm's printf, matching the final pass's column order.
const header = "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive"

// Options is everything Run reads beyond the bus: the process probes,
// whether $JQ_COLORS was invalid (jq warns once where the arm's several jq
// processes warned once each), and the clock.
type Options struct {
	Probes          Probes
	JQColorsInvalid bool
	Now             func() time.Time
}

// say writes to a stream whose failure is reported elsewhere (the exit
// status, or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm: --mine lists this caller's crews, no argument prints the
// table, anything else is a usage exit. Table and --mine exit 0; only the
// unknown argument fails.
func Run(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	// The arm's `case "${1:-}"`: only the first argument is inspected, and
	// an empty one takes the table branch.
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	switch arg {
	case "--mine":
		mine(paths, stdout, o)
		return 0
	case "":
		return table(paths, stdout, stderr, o)
	default:
		say(stderr, "crew: crews: unknown arg '%s'\n", arg)
		return 64
	}
}

// mine is the arm's --mine branch: crews whose recorded dispatcher pid is a
// live ancestor of this process. Only pid files count, and the log is never
// read.
func mine(paths bus.Paths, stdout io.Writer, o Options) {
	now := o.Now()
	for _, id := range crewDirs(paths) {
		pidfile := paths.Dir + "/crews/" + id + "/pid"
		pid, ok := PidFileText(pidfile)
		if !ok {
			continue // no readable file reads as the empty pid
		}
		// The same positive-integer guard as the table: `kill -0 0` signals
		// our own process group, so a junk pid is never a liveness probe.
		n, ok := validPid(pid)
		if !ok {
			continue
		}
		if !o.Probes.RecordedLive(now, n, pidfile) || !o.Probes.IsAncestor(n) {
			continue
		}
		say(stdout, "%s\n", id)
	}
}

// table is the arm's table branch. The header goes out first, so an empty
// bus still prints it, and no log outcome — missing, unreadable, corrupt —
// fails the command.
func table(paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	say(stdout, "%s\n", header)
	now := o.Now()
	nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6

	events, logErr := bus.ReadEventsTolerant(paths.Log)
	_, logDecodable := logErr.(*bus.DecodeError)
	logOK := logErr == nil

	// Discovery union: every crews/ subdirectory plus every crew_id the log
	// carries. A corrupt log keeps its well-formed prefix here (`jq -r ...
	// || true` printed it) and contributes nothing to stats (jq -s failed
	// whole, `|| echo '{}'`).
	ids := crewDirs(paths)
	if logOK || logDecodable {
		for _, ev := range events {
			if ev.CrewID != "" {
				ids = append(ids, ev.CrewID)
			}
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return 0
	}

	meta := make([]jsonv.Value, 0, len(ids))
	for _, id := range ids {
		pidfile := paths.Dir + "/crews/" + id + "/pid"
		pid, hasFile := PidFileText(pidfile)
		if !hasFile {
			pid = ""
		}
		// `$(cat ... || true)` of an empty value is null, a junk or 0 pid is
		// not alive, and only a positive-integer live-and-not-recycled pid
		// is.
		pidV, alive := jsonv.Null(), jsonv.Null()
		if pid != "" {
			pidV = jsonv.Str(pid)
			if a := PidAlive(o.Probes, now, pidfile, pid); a != nil {
				alive = jsonv.Bool(*a)
			}
		}
		meta = append(meta, jsonv.Object(
			jsonv.Member{Key: "id", Val: jsonv.Str(id)},
			jsonv.Member{Key: "pid", Val: pidV},
			jsonv.Member{Key: "alive", Val: alive},
		))
	}

	stats := jsonv.Object()
	if logOK {
		raws := make([]jsonv.Value, len(events))
		for i, ev := range events {
			raws[i] = ev.Raw
		}
		if v, err := jqrun.Run(statsProgram, raws, nowSec, nil); err == nil {
			stats = v
		}
	}

	if o.JQColorsInvalid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	out, err := jqrun.Run(finalProgram, meta, nowSec, map[string]jsonv.Value{"stats": stats})
	if err != nil {
		// Reachable on a corrupt bus: a stats row whose last/first is null
		// (a crew whose only ts is missing) makes gojq subtract null, and
		// both engines' jq rejects that. The arm's jq -r had streamed the
		// rows before the failing one; gojq yields nothing until its single
		// value completes, so Go prints zero rows — docs/crew-go-port.md's
		// "otherwise empty stdout" rule. Exit 5 matches the arm.
		say(stderr, "crew: crews: %v\n", err)
		return 5
	}
	if s, _ := out.AsString(); s != "" {
		say(stdout, "%s\n", s)
	}
	return 0
}

// crewDirs is the arm's `for d in "$dir"/crews/*/` glob: subdirectory
// basenames — symlinked dirs count, the `/` and the `[ -d ]` both follow —
// in glob order. Bash sorted the glob with the caller's collation; byte
// order is the documented, locale-free choice (docs/crew-go-port.md).
func crewDirs(paths bus.Paths) []string {
	ents, err := os.ReadDir(paths.Dir + "/crews")
	if err != nil {
		return nil // the arm's `[ -d "$dir/crews" ]` miss
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(paths.Dir + "/crews/" + e.Name()); err == nil && st.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

// PidAlive is the three-state liveness of the pid a crew's pidfile records,
// read off the text `t` that file holds: nil for the empty text — no file, or an
// empty one, which is *unknown* rather than dead, so an unregistered crew is
// never dropped for it — false for `0` or any non-digit, and the probe's answer
// for a positive integer. `crews` prints it and `reply` filters candidates on it.
func PidAlive(p Probes, now time.Time, pidfile, t string) *bool {
	if t == "" {
		return nil
	}
	n, ok := validPid(t)
	if !ok {
		return boolPtr(false)
	}
	return boolPtr(p.RecordedLive(now, n, pidfile))
}

func boolPtr(b bool) *bool { return &b }

// PidFileText is `$(cat <pidfile> 2>/dev/null || true)`: the file's text
// without its trailing newlines, "" when it cannot be read.
func PidFileText(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(data), "\n"), true
}

// validPid is the arm's `case "$pid" in *[!0-9]* | 0)` guard: all digits and
// not the single "0". "00" passes and so does bash's `kill -0 00` (it signals
// our own process group, signal 0), which Kill(0, 0) mirrors. A digit string
// beyond int, or past the kernel's pid ceiling (kill(2) would truncate it to a
// pid_t, e.g. 4294967295 to -1), fails here exactly where bash's kill and ps
// both fail on it.
func validPid(s string) (int, bool) {
	if s == "" || s == "0" || !isDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > lock.MaxPID {
		return 0, false
	}
	return n, true
}
