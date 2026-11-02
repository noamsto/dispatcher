package watch

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/playbook"
)

//go:embed overseer.jq
var overseerProgram string

// OverseerFold is one poll of a bus for the overseer. Batch is nil when no
// msg qualified, else the `crew watch` batch {cursor, events}; Cursor is the
// newest msg to the overseer either way.
type OverseerFold struct {
	Batch     map[string]any
	Misrouted []Misrouted
	Cursor    int64
}

// Misrouted is a msg to the overseer from anyone but the dispatcher of the
// crew it names: who sent it, never what it said.
type Misrouted struct {
	TS         int64
	Crew, From string
}

// FoldOverseer folds the msgs to me above since on the log at path. false with
// no error is "poll again": nothing above since. An error is a log that could
// not be read or folded.
func FoldOverseer(path, me string, since int64) (OverseerFold, bool, error) {
	events, err := bus.ReadEvents(path)
	if err != nil {
		return OverseerFold{}, false, err
	}
	in := make([]any, len(events))
	for i, ev := range events {
		in[i] = ev.Doc
	}
	res, err := jqrun.RunAny(overseerProgram, in, 0, map[string]any{
		"me":    me,
		"since": json.Number(strconv.FormatInt(since, 10)),
	})
	if err != nil || res == nil {
		return OverseerFold{}, false, err
	}
	out, _ := res.(map[string]any)
	cursor, ok := wholeMS(out["cursor"])
	if !ok {
		return OverseerFold{}, false, fmt.Errorf("cursor %v is not a whole ms", out["cursor"])
	}
	f := OverseerFold{Cursor: cursor}
	misrouted, _ := out["misrouted"].([]any)
	for _, m := range misrouted {
		row, _ := m.(map[string]any)
		ts, _ := wholeMS(row["ts"])
		crew, _ := row["crew_id"].(string)
		from, _ := row["from"].(string)
		f.Misrouted = append(f.Misrouted, Misrouted{TS: ts, Crew: crew, From: from})
	}
	if qualified, _ := out["events"].([]any); len(qualified) > 0 {
		batch := map[string]any{"cursor": json.Number(strconv.FormatInt(cursor, 10)), "events": qualified}
		playbook.Annotate(batch)
		f.Batch = invisible.Value(jsonval.Clamp(batch)).(map[string]any)
	}
	return f, true, nil
}

// wholeMS is a ts the fold selected, as jqrun hands it back: a ts the log
// wrote as 2500.0 or 2.5e3 is still that json.Number text, a computed one a
// float64.
func wholeMS(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case json.Number:
		if ms, err := n.Int64(); err == nil {
			return ms, true
		}
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if f != math.Trunc(f) || f < 0 || f >= 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// ReadCursor is a cursor file's ms: its digits without trailing newlines, and
// false for a file that is missing, empty or not all digits — seedCursor's rule.
func ReadCursor(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	text := strings.TrimRight(string(data), "\n")
	if !ascii.IsDigits(text) {
		return 0, false
	}
	ts, err := strconv.ParseInt(text, 10, 64)
	return ts, err == nil
}

// WriteCursor replaces the cursor at path with ts through a tmp file renamed
// over it in the same dir, so a reader never sees a half-written cursor.
func WriteCursor(path string, ts int64) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cursor.")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(strconv.FormatInt(ts, 10) + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}
