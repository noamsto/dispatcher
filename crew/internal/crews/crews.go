// Package crews is `crew crews`: the per-crew discovery table over a repo
// bus (#29) and the --mine ancestor scan. The two jq halves of the table are
// the original programs from adapters/core/crew.sh, run through gojq
// (crews_stats.jq, crews_final.jq). The #838 fix is structural: the stats
// map reaches the final pass as a jqrun variable, never a `--argjson stats`
// argv argument, which grew past MAX_ARG_STRLEN near ~1700 crews and killed
// the command with "Argument list too long". The dir scan and the pid
// liveness / ancestor probes stay Go, as in the arm's bash loop.
//
// `--home` narrows the table to crews whose dispatcher lives in this repo (see
// Homes.IsHome for the record and the legacy rule), because a pid file alone
// also appears for a crew that only dispatched workers into the repo.
package crews

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/lock"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/proc"
)

//go:embed crews_stats.jq
var statsProgram string

//go:embed crews_final.jq
var finalProgram string

// header is the arm's printf, matching the final pass's column order.
const header = "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive"

// Options is everything Run reads beyond the bus: the process table and the
// clock.
type Options struct {
	Proc  proc.Table
	Clock clock.Clock
}

// say writes to a stream whose failure is reported elsewhere (the exit
// status, or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm: --mine lists this caller's crews, otherwise the table
// prints, restricted to crews homed in this repo under homeOnly (see
// Homes.IsHome).
func Run(ctx context.Context, mineOnly, homeOnly bool, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	if mineOnly {
		mine(paths, stdout, o)
		return 0
	}
	return table(ctx, homeOnly, paths, stdout, stderr, o)
}

// RunLegacy lists the live layout-1 crews of this repo — the phase 6 deletion
// gate, "crew crews --legacy prints nothing". Layout 1 means "resolves to
// layout 1 here": a repo dir beside a meta.json is layout 2 and is skipped.
// It prints one id per line in crewDirs order, with no header, and always
// exits 0.
func RunLegacy(paths bus.Paths, stdout io.Writer, o Options) int {
	now := o.Clock.Now()
	for _, id := range crewDirs(paths) {
		crew, err := paths.Crew(id)
		if err != nil || crew.Layout == 2 {
			continue
		}
		pidfile := crew.Dir + "/pid"
		pid, ok := PidFileText(pidfile)
		if !ok {
			continue // no readable file reads as the empty pid
		}
		n, ok := validPid(pid)
		if !ok {
			continue
		}
		if !RecordedLive(o.Proc, now, n, pidfile) {
			continue
		}
		say(stdout, "%s\n", id)
	}
	return 0
}

// mine is the arm's --mine branch: crews whose recorded dispatcher pid is a
// live ancestor of this process. Only pid files count, and the log is never
// read.
func mine(paths bus.Paths, stdout io.Writer, o Options) {
	now := o.Clock.Now()
	ids := append(crewDirs(paths), homedIDs(paths)...)
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		crew, err := paths.Crew(id)
		if err != nil {
			continue
		}
		pidfile := crew.Dir + "/pid"
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
		if !RecordedLive(o.Proc, now, n, pidfile) || !proc.IsAncestor(o.Proc, n) {
			continue
		}
		say(stdout, "%s\n", id)
	}
}

// table is the arm's table branch. The header goes out first, so an empty
// bus still prints it, and no log outcome — missing, unreadable, corrupt —
// fails the command.
func table(ctx context.Context, homeOnly bool, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	say(stdout, "%s\n", header)
	now := o.Clock.Now()
	nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6

	events, logErr := bus.ReadEventsTolerant(paths.Log)
	logOK := logErr == nil

	ids := append(discover(paths, events, logErr), homedIDs(paths)...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if homeOnly {
		homes := NewHomes(ctx, paths, o)
		ids = slices.DeleteFunc(ids, func(id string) bool { return !homes.IsHome(id) })
	}
	if len(ids) == 0 {
		return 0
	}

	// A corrupt log contributes nothing to stats.
	var docs []any
	if logOK {
		for _, ev := range events {
			docs = append(docs, ev.Doc)
		}
	}
	meta := make([]any, 0, len(ids))
	for _, id := range ids {
		var pid, pidfile string
		if crew, err := paths.Crew(id); err == nil {
			if crew.Layout == 2 {
				docs = append(docs, crewDocs(crew)...)
			}
			pidfile = crew.Dir + "/pid"
			var hasFile bool
			if pid, hasFile = PidFileText(pidfile); !hasFile {
				pid = ""
			}
		}
		// `$(cat ... || true)` of an empty value is null, a junk or 0 pid is
		// not alive, and only a positive-integer live-and-not-recycled pid
		// is.
		var pidV, alive any
		if pid != "" {
			pidV = pid
			if a := PidAlive(o.Proc, now, pidfile, pid); a != nil {
				alive = *a
			}
		}
		meta = append(meta, map[string]any{"id": id, "pid": pidV, "alive": alive})
	}

	var stats any = map[string]any{}
	if len(docs) > 0 {
		if v, err := jqrun.RunAny(statsProgram, docs, nowSec, nil); err == nil {
			stats = v
		}
	}

	out, err := jqrun.RunAny(finalProgram, meta, nowSec, map[string]any{"stats": stats})
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
	if s, _ := out.(string); s != "" {
		say(stdout, "%s\n", s)
	}
	return 0
}

// crewDocs is layout-2 crew c's own rows, none when its log is missing or
// corrupt.
func crewDocs(c bus.Crew) []any {
	events, err := bus.ReadEventsTolerant(c.Log)
	if err != nil {
		return nil
	}
	var docs []any
	for _, ev := range events {
		if ev.CrewID == c.ID {
			docs = append(docs, ev.Doc)
		}
	}
	return docs
}

// IDs is the crew set `crew crews` lists for a bus.
func IDs(paths bus.Paths) []string {
	events, logErr := bus.ReadEventsTolerant(paths.Log)
	return discover(paths, events, logErr)
}

// discover is the discovery union: every crews/ subdirectory plus every
// crew_id the log carries, byte-sorted and deduplicated. A corrupt log keeps
// its well-formed prefix here (`jq -r ... || true` printed it).
func discover(paths bus.Paths, events []bus.Event, logErr error) []string {
	ids := crewDirs(paths)
	if _, decodable := logErr.(*bus.DecodeError); logErr == nil || decodable {
		for _, ev := range events {
			if ev.CrewID != "" {
				ids = append(ids, ev.CrewID)
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// crewDirs is the arm's `for d in "$dir"/crews/*/` glob: subdirectory
// basenames — symlinked dirs count, the `/` and the `[ -d ]` both follow —
// in glob order. Bash sorted the glob with the caller's collation; byte
// order is the documented, locale-free choice (docs/crew-go-port.md).
func crewDirs(paths bus.Paths) []string {
	if paths.Common == "" {
		return nil // outside a repo there is no repo bus to glob
	}
	ents, err := os.ReadDir(paths.CrewsDir())
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
			if st, err := os.Stat(paths.CrewDir(e.Name())); err == nil && st.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

// homedIDs is the layout-2 crews homed where paths is, live or dead: every
// crews/*/meta.json under the state root that homedAt accepts.
func homedIDs(paths bus.Paths) []string {
	if paths.State == "" {
		return nil
	}
	tag, ok := homeTag(paths)
	if !ok {
		return nil
	}
	var ids []string
	for _, m := range bus.LiveMetas(paths.State) {
		if homedAt(m, tag) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// homeTag is the home_repo a crew homed where paths is records: the repo's
// RepoTag, "" outside a repo. ok is false when the repo cannot be resolved.
func homeTag(paths bus.Paths) (string, bool) {
	if paths.Common == "" {
		return "", true
	}
	tag, err := bus.RepoTag(paths.Common)
	return tag, err == nil
}

// homedAt reports whether a layout-2 crew's dispatcher lives at tag (see
// homeTag): a visiting crew is never home, and outside a repo only the crews
// registered outside one (null home_repo) are.
func homedAt(m bus.Meta, tag string) bool {
	switch {
	case m.Visiting:
		return false
	case tag == "":
		return m.HomeRepo == nil
	}
	return m.HomeRepo != nil && *m.HomeRepo == tag
}

// RecordedLive is _recorded_pid_live: <pid> can still be the dispatcher
// <pidfile> records — live and not a later-recycled pid (_pid_recycled).
//
// When PidStartFile holds a token recorded for this very pid and the process's
// current start is readable, identity is exact: the current start must equal
// the recorded one. Otherwise (no sidecar, a blank or malformed one, one naming
// another pid, or an unreadable current start — fail closed to the heuristic,
// never to dead) the process now holding <pid> must not have
// started after <pidfile> was last written (plus 2s of slack for ps' second
// granularity). That heuristic says not-recycled on an unreadable timestamp or
// a pidfile that is not a regular file, so a live process is never judged dead.
func RecordedLive(t proc.Table, now time.Time, pid int, pidfile string) bool {
	if !t.Alive(pid) {
		return false
	}
	if data, err := os.ReadFile(PidStartFile(pidfile)); err == nil {
		line, _, _ := strings.Cut(string(data), "\n")
		// Any run of space, tab or CR separates fields, like the shell reader;
		// the macOS token itself contains spaces, so the rest is rejoined.
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' })
		if len(fields) >= 2 && fields[0] == strconv.Itoa(pid) {
			if cur, ok := t.Start(pid); ok {
				return cur == strings.Join(fields[1:], " ")
			}
		}
	}
	elapsed, ok := t.Elapsed(pid)
	st, err := os.Stat(pidfile)
	return !ok || err != nil || !st.Mode().IsRegular() || now.Unix()-elapsed <= st.ModTime().Unix()+2
}

// PidStartFile is the sidecar holding the start token of the process recorded
// in <pidfile>.
func PidStartFile(pidfile string) string { return pidfile + ".start" }

// WritePidStart records "<pid> <token>" for the pid in pidText beside pidfile.
// A sidecar pairs only with the pid it names, so any interleaving of the two
// writes (or a crash between them) falls back to the heuristic instead of
// reading a live dispatcher dead. With no valid pid or no readable start it
// removes the sidecar instead.
func WritePidStart(t proc.Table, pidfile, pidText string) error {
	path := PidStartFile(pidfile)
	if n, ok := validPid(pidText); ok {
		if tok, ok := t.Start(n); ok {
			return os.WriteFile(path, []byte(strconv.Itoa(n)+" "+tok+"\n"), 0o644)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// PidAlive is the three-state liveness of the pid a crew's pidfile records,
// read off the text `t` that file holds: nil for the empty text — no file, or an
// empty one, which is *unknown* rather than dead, so an unregistered crew is
// never dropped for it — false for `0` or any non-digit, and the probe's answer
// for a positive integer. `crews` prints it and `reply` filters candidates on it.
func PidAlive(tbl proc.Table, now time.Time, pidfile, t string) *bool {
	if t == "" {
		return nil
	}
	n, ok := validPid(t)
	if !ok {
		return boolPtr(false)
	}
	return boolPtr(RecordedLive(tbl, now, n, pidfile))
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
	if s == "" || s == "0" || !ascii.IsDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > lock.MaxPID {
		return 0, false
	}
	return n, true
}

// Caller is the process identity a pidfile.log line records: who ran the
// mutation, so a crew dir that vanishes or changes owner can be traced (#432).
// By is the parent's `ps -o args=`.
type Caller struct {
	Now       time.Time
	PID, PPID int
	Pwd, By   string
}

// PidfileLog is `_pidfile_log`: one line per crew pid file mutation (or
// refusal) in `<state>/pidfile.log`, state being the crew's state root
// (bus.Crew.State). It is diagnostic rather than state, so a failed write is
// dropped as the arm drops it.
func PidfileLog(state string, c Caller, action, outcome, crew, oldPID, newPID string) {
	line := fmt.Sprintf("%s %s %s crew=%s old=%s new=%s pid=%d ppid=%d cwd=%s by=%s",
		c.Now.UTC().Format("2006-01-02T15:04:05Z"), action, outcome, crew,
		orDash(oldPID), orDash(newPID), c.PID, c.PPID, c.Pwd, firstRunes(c.By, 120))
	_ = os.MkdirAll(state, 0o755)
	_ = bus.Append(state+"/pidfile.log", line)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// firstRunes is bash's `${by:0:120}`: counts are characters, not bytes.
func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
