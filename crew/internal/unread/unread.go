// Package unread is the unread-message scan of #290: crew.sh's `_unread_scan`,
// ported in process (#968). `stall-watch`'s D6 detector runs it every fourth
// tick and `crew nudge` runs it per call; both used to call back into bash
// through `stall-watch --sh unread`, which this package retires.
//
// It is a bus read over the delivered marks, so it lives beside — not inside —
// `internal/marks`: marks owns the marks file's path, read and merge, while a
// scan is a fold of the log that consults it, the same split that keeps
// `internal/bus` a reader rather than a second marks writer.
//
// The scan is the helper's jq program through jqrun (unread.jq), the same
// exact-port rule `internal/marks` follows: the verdict hinges on jq's type
// ordering (`.ts > ($marks[.from] // 0)` keeps a string ts) and on min_by/max_by
// over mixed values, none of which reads better re-typed in Go.
package unread

import (
	_ "embed"
	"encoding/json"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

//go:embed unread.jq
var scanProgram string

// Mode is the output shape, the helper's seventh argv.
type Mode string

const (
	// Oldest prints "<ts-ms> role|dispatcher" for the oldest undelivered
	// role/dispatcher msg to the lead.
	Oldest Mode = "oldest"
	// Dispatcher prints "<oldest-ts> <newest-ts>" of the undelivered
	// dispatcher msgs only.
	Dispatcher Mode = "dispatcher"
)

// tailLines is the helper's `tail -n 2000`: a msg scrolled out of the tail
// reads as gone.
const tailLines = 2000

// Request is the helper's argv: the crew dir and bus log come from bus.Paths,
// the rest are the arm's `<crew> <branch> <me> <from_id> <t0>`. Me is the
// lead's branch-keyed id, FromID the session whose delivered marks the scan
// reads, and T0 the run start in ms (the scope a sessioned scan ignores).
type Request struct {
	Dir    string
	Log    string
	Crew   string
	Branch string
	Me     string
	FromID string
	T0     int64
}

// Scan answers the helper: empty when nothing is undelivered, and otherwise
// per Mode. Every silence the helper had reads alike here — a branch-keyed
// watchdog (FromID == Me) cannot name the lead's session, so it cannot read
// that session's marks and stays silent rather than misreporting; a missing
// log is no log; and the helper's `2>/dev/null || true` turned a jq that could
// not run over a corrupt row into the empty scan too.
func Scan(r Request, mode Mode) string {
	if r.FromID == r.Me {
		return ""
	}
	rows, ok := probe.BusRows(r.Log, 0, tailLines)
	if !ok {
		return ""
	}
	out, err := jqrun.RunAny(scanProgram, rows, 0, map[string]any{
		"c":     r.Crew,
		"b":     "role:" + r.Branch + ":",
		"d":     "dispatcher:" + r.Crew,
		"me":    r.Me,
		"f":     r.FromID,
		"t0":    json.Number(strconv.FormatInt(r.T0, 10)),
		"mode":  string(mode),
		"marks": jsonv.ToAny(marks.Read(r.Dir, r.Crew, r.FromID)),
	})
	if err != nil {
		return ""
	}
	s, _ := out.(string)
	return s
}
