package reap

import (
	_ "embed"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// The arm's jq programs; each file's header documents its patches.
var (
	//go:embed latest.jq
	latestProg string
	//go:embed idle.jq
	idleProg string
	//go:embed claimed.jq
	claimedProg string
	//go:embed posted.jq
	postedProg string
	//go:embed findpr.jq
	findPRProg string
)

// claimMaskTTL is `claim_mask_ttl`: a claim older than this many seconds no
// longer masks a prior terminal status.
const claimMaskTTL = 3600

// terminalStates is `reap_terminal_states`, the states a worker session ends
// in, shared by the idle-release and candidate folds so they cannot drift
// apart again (#68).
var terminalStates = jsonv.Array(jsonv.Str("done"), jsonv.Str("failed"), jsonv.Str("exited"))

// latestRow is one `_reap_latest` row. own, earlier and reaprow are the three
// PR sources ("-" when absent): the latest session's pr_url, the newest of the
// branch's other sessions', the newest reap row's. ts is the latest status's
// ts as text; later is "1" when some session of the branch posted after it.
type latestRow struct {
	branch, state, own, earlier, reaprow, ts, later, session, scrow string
}

// readRaws is one `jq -s` read of the bus.
func readRaws(log string) ([]jsonv.Value, error) {
	events, err := bus.ReadEvents(log)
	if err != nil {
		return nil, err
	}
	raws := make([]jsonv.Value, len(events))
	for i, ev := range events {
		raws[i] = ev.Raw
	}
	return raws, nil
}

func (r *run) nowSec() float64 {
	return float64(r.p.Now().UnixNano()) / 1e9
}

// latest is `_reap_latest [branch]`. The error is the bus read's (map it
// through bus.JQFailure) or the program's.
func (r *run) latest(only string) ([]latestRow, error) {
	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return nil, err
	}
	out, err := jqrun.Run(latestProg, raws, r.nowSec(), map[string]jsonv.Value{
		"only":      jsonv.Str(only),
		"terminal":  terminalStates,
		"claim_ttl": jsonv.Num(claimMaskTTL),
	})
	if err != nil {
		return nil, err
	}
	var rows []latestRow
	for _, c := range cells(out) {
		rows = append(rows, latestRow{c[0], c[1], c[2], c[3], c[4], c[5], c[6], c[7], c[8]})
	}
	return rows, nil
}

// idleRows is the idle-release fold: branch, session, state, ts per row. A bus
// or program failure is no rows — the arm's heredoc `$(jq …)` never tripped
// `set -e`.
func (r *run) idleRows() [][]string {
	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return nil
	}
	idle, _ := jsonv.ParseNumber(r.idle)
	out, err := jqrun.Run(idleProg, raws, r.nowSec(), map[string]jsonv.Value{
		"idle":     idle,
		"terminal": terminalStates,
	})
	if err != nil {
		return nil
	}
	return cells(out)
}

// claimed is the claimed-issue fold's `$(jq -s -r …)` text: the branch's
// claimed issues, one per line.
func (r *run) claimed(branch string) (string, error) {
	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return "", err
	}
	out, err := jqrun.Run(claimedProg, raws, r.nowSec(), map[string]jsonv.Value{"b": jsonv.Str(branch)})
	if err != nil {
		return "", err
	}
	return rawLines(out), nil
}

// postedSince reports whether the branch may have posted after ts — anything
// but a clean `false` from the check, an unreadable bus or a ts that is not
// JSON included.
func (r *run) postedSince(branch, ts string) bool {
	tsv, err := jsonv.ParseOne(ts)
	if err != nil {
		return true
	}
	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return true
	}
	out, err := jqrun.Run(postedProg, raws, r.nowSec(), map[string]jsonv.Value{
		"w":  jsonv.Str("worker:" + branch),
		"ts": tsv,
	})
	return err != nil || out.Kind() != jsonv.KindFalse
}

// prFromList is `jq -er` over gh's `pr list` text: the branch's own PR URL,
// "-" for none, or false when the text or a PR in it is unusable (jq's parse
// error, the program's error, or no output at all).
func prFromList(prs string) (string, bool) {
	vals, err := jsonv.DecodeStream(strings.NewReader(prs))
	if err != nil || len(vals) == 0 {
		return "", false
	}
	out, err := jqrun.Run(findPRProg, vals, 0, nil)
	if err != nil {
		return "", false
	}
	return rawLines(out), true
}

// cells reads a patched fold's rows: arrays of strings, by position.
func cells(v jsonv.Value) [][]string {
	var rows [][]string
	for _, row := range v.Elems() {
		var c []string
		for _, cell := range row.Elems() {
			s, _ := cell.AsString()
			c = append(c, s)
		}
		rows = append(rows, c)
	}
	return rows
}

// rawLines is `$(jq -r '.[]')` over an array of strings: one per line,
// trailing newlines stripped.
func rawLines(v jsonv.Value) string {
	var b strings.Builder
	for _, e := range v.Elems() {
		s, _ := e.AsString()
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
