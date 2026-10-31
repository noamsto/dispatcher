package peer

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// DecisionRow is the bus line `crew decide` appends for text: a dispatcher's
// decision, addressed to nobody. The shape lives here so the writer and the
// export allowlist cannot drift.
func DecisionRow(ts int64, crew, body string) string {
	line, err := encode(struct {
		TS     int64  `json:"ts"`
		CrewID string `json:"crew_id"`
		From   string `json:"from"`
		Kind   string `json:"kind"`
		Body   string `json:"body"`
	}{ts, crew, "dispatcher:" + crew, "decision", body})
	if err != nil {
		panic(err)
	}
	return string(line)
}

// Decision is one announced decision: Origin is "local" for this host's log,
// else the peer host that sent it.
type Decision struct {
	TS     int64
	Origin string
	CrewID string
	Body   string
}

type decisionRow struct {
	TS     int64  `json:"ts"`
	CrewID string `json:"crew_id"`
	Kind   string `json:"kind"`
	From   string `json:"from"`
	Body   string `json:"body"`
}

// Decisions is every decision at ts >= since: the local log's dispatcher
// decisions plus those in each peer store file, oldest first (stable: local
// before peers on a tie, then file order). A peer reset re-delivers rows
// already stored, so a repeat of (origin, ts, crew_id, body) is dropped.
// Admission has already checked peer rows; the local log is held to the same
// from rule so a worker cannot forge a decision.
func Decisions(p bus.Paths, since int64) ([]Decision, error) {
	var out []Decision
	seen := map[Decision]bool{}
	add := func(origin string, r decisionRow) {
		d := Decision{TS: r.TS, Origin: origin, CrewID: r.CrewID, Body: r.Body}
		if r.Kind != "decision" || r.TS < since || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	if _, err := eachLine(p.Log, 0, func(line []byte, _ int64) bool {
		var r decisionRow
		if json.Unmarshal(line, &r) == nil && bus.ValidCrewID(r.CrewID) && r.From == "dispatcher:"+r.CrewID {
			add("local", r)
		}
		return true
	}); err != nil {
		return nil, err
	}
	s := StoreFor(p)
	entries, err := os.ReadDir(s.Dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		host, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !remote.HostNameRe.MatchString(host) {
			continue
		}
		if _, err := eachLine(s.path(host, ".jsonl"), 0, func(line []byte, _ int64) bool {
			var sl storedLine
			var r decisionRow
			if json.Unmarshal(line, &sl) == nil && json.Unmarshal(sl.Row, &r) == nil {
				add(host, r)
			}
			return true
		}); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(out, func(a, b Decision) int { return cmp.Compare(a.TS, b.TS) })
	return out, nil
}
