package peer

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// SnapshotRow is one live worker of a dispatcher's roster, as it crosses to a
// peer.
type SnapshotRow struct {
	CrewID string `json:"crew_id"`
	Branch string `json:"branch"`
	Name   string `json:"name,omitempty"`
	State  string `json:"state"`
	Tier   string `json:"tier,omitempty"`
	Engine string `json:"engine,omitempty"`
	TS     int64  `json:"ts"`
}

// Snapshot is a dispatcher's roster at TS, folded from its own bus at export
// time. It is latest-wins data, never a log row.
type Snapshot struct {
	TS   int64         `json:"ts"`
	Rows []SnapshotRow `json:"rows"`
}

// snapshotStates are the worker states a snapshot row may carry.
var snapshotStates = []string{"working", "blocked", "pr_open", "done", "failed", "exited"}

// Per-field caps of a snapshot row, in bytes.
const (
	snapBranchMax = 200
	snapNameMax   = 64
	snapTierMax   = 16
	snapEngineMax = 32
)

type snapLine struct {
	TS     json.Number `json:"ts"`
	CrewID string      `json:"crew_id"`
	Kind   string      `json:"kind"`
	From   string      `json:"from"`
	Branch string      `json:"branch"`
	Name   string      `json:"name"`
	Tier   string      `json:"tier"`
	Engine string      `json:"engine"`
	Body   struct {
		State string `json:"state"`
	} `json:"body"`
}

type snapKey struct{ crew, branch string }

// TakeSnapshot folds the bus log at log into this dispatcher's roster: per
// crew and branch, the latest worker status within keepFor of now (and no more
// than maxSkew ahead), with name, tier and engine from that crew's last
// dispatch of the branch. A latest state outside snapshotStates (reaped) drops
// the branch. Rows a peer's AdmitSnapshot would drop are left out, and the
// newest MaxSnapshotRows are kept, sorted by branch.
func TakeSnapshot(log string, now time.Time) (Snapshot, error) {
	latest := map[snapKey]SnapshotRow{}
	dispatched := map[snapKey]snapLine{}
	_, err := eachLine(log, 0, func(line []byte, _ int64) bool {
		var l snapLine
		if json.Unmarshal(line, &l) != nil {
			return true
		}
		ts, err := l.TS.Int64()
		if err != nil {
			return true
		}
		if t := time.UnixMilli(ts); now.Sub(t) > keepFor || t.After(now.Add(maxSkew)) {
			return true
		}
		switch l.Kind {
		case "dispatch":
			dispatched[snapKey{l.CrewID, l.Branch}] = l
		case "status":
			wid, ok := strings.CutPrefix(l.From, "worker:")
			if !ok {
				return true
			}
			if i := strings.LastIndexByte(wid, '#'); i >= 0 {
				wid = wid[:i]
			}
			k := snapKey{l.CrewID, wid}
			if prev, ok := latest[k]; !ok || ts >= prev.TS {
				latest[k] = SnapshotRow{CrewID: l.CrewID, Branch: wid, State: l.Body.State, TS: ts}
			}
		}
		return true
	})
	if err != nil {
		return Snapshot{}, err
	}
	rows := []SnapshotRow{}
	for k, r := range latest {
		d := dispatched[k]
		r.Name, r.Tier, r.Engine = d.Name, d.Tier, d.Engine
		if snapshotDrop(r, now) == "" {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b SnapshotRow) int { return cmp.Compare(b.TS, a.TS) })
	rows = rows[:min(len(rows), MaxSnapshotRows)]
	slices.SortFunc(rows, func(a, b SnapshotRow) int {
		return cmp.Or(cmp.Compare(a.Branch, b.Branch), cmp.Compare(a.CrewID, b.CrewID))
	})
	return Snapshot{TS: now.UnixMilli(), Rows: rows}, nil
}

// AdmitSnapshot is the reader-side check of a peer's snapshot: the rows that
// pass, and the drop counts of the rest. Rows past MaxSnapshotRows drop as
// oversize.
func AdmitSnapshot(raw *Snapshot, now time.Time) ([]SnapshotRow, map[string]int) {
	dropped := map[string]int{}
	var rows []SnapshotRow
	for i, r := range raw.Rows {
		reason := snapshotDrop(r, now)
		if i >= MaxSnapshotRows {
			reason = reasonOversize
		}
		if reason != "" {
			dropped[reason]++
			continue
		}
		rows = append(rows, r)
	}
	return rows, dropped
}

// snapshotDrop is why r may not cross, or "": the same ts and identity rules
// as Admit, an enum state, and capped text free of control and invisible
// characters.
func snapshotDrop(r SnapshotRow, now time.Time) string {
	t := time.UnixMilli(r.TS)
	if now.Sub(t) > keepFor {
		return reasonStale
	}
	if t.After(now.Add(maxSkew)) {
		return reasonMalformed
	}
	if !bus.ValidCrewID(r.CrewID) {
		return reasonIdentity
	}
	if r.Branch == "" || !slices.Contains(snapshotStates, r.State) {
		return reasonMalformed
	}
	text := []struct {
		s   string
		max int
	}{{r.Branch, snapBranchMax}, {r.Name, snapNameMax}, {r.Tier, snapTierMax}, {r.Engine, snapEngineMax}}
	for _, f := range text {
		if hasControl(f.s, false) {
			return reasonControl
		}
	}
	for _, f := range text {
		if len(f.s) > f.max {
			return reasonOversize
		}
	}
	return ""
}

// rosterFile is `<host>.roster`: the peer's last admitted snapshot and when it
// arrived.
type rosterFile struct {
	Synced int64         `json:"synced"`
	Rows   []SnapshotRow `json:"rows"`
}

// readRoster is host's stored roster rows; a missing file has none.
func (s Store) readRoster(host string) ([]SnapshotRow, error) {
	path := s.path(host, ".roster")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r rosterFile
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r.Rows, nil
}

// RosterRows are the roster rows of every peer host in the store, by host and
// then branch, tagged `host`, `peer` and `live: null`. A host whose last good
// exchange is not the latest, or is older than PeerStale, gives its rows with
// `state: null` and why in `peer_error`, or one placeholder when it has none.
// peers, the settings loader, is called once and only when the store holds a
// link or roster file, so a clone with no peers never loads settings; a nil
// loader or one that finds no dispatch-config means no peer rows.
func RosterRows(s Store, peers func() (map[string]remote.Host, error), now time.Time) ([]map[string]any, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".link")
		if !ok {
			name, ok = strings.CutSuffix(e.Name(), ".roster")
		}
		if ok && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 || peers == nil {
		return nil, nil
	}
	hosts, err := peers()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	var out []map[string]any
	for _, host := range names {
		if !hosts[host].Peer {
			continue
		}
		rows, err := hostRosterRows(s, host, now)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func hostRosterRows(s Store, host string, now time.Time) ([]map[string]any, error) {
	l, err := s.readLink(host)
	if err != nil {
		return nil, err
	}
	snap, err := s.readRoster(host)
	if err != nil {
		return nil, err
	}
	var why string
	switch {
	case l.Err > l.OK:
		why = "last sync failed: " + l.Detail
	case l.OK == 0:
		why = "no sync yet"
	case now.Sub(time.UnixMilli(l.OK)) > PeerStale:
		why = fmt.Sprintf("no sync for %dm", int64(now.Sub(time.UnixMilli(l.OK))/time.Minute))
	}
	if why != "" && len(snap) == 0 {
		return []map[string]any{{"branch": nil, "name": nil, "state": nil, "live": nil, "host": host, "peer": true, "peer_error": why}}, nil
	}
	slices.SortStableFunc(snap, func(a, b SnapshotRow) int { return cmp.Compare(a.Branch, b.Branch) })
	rows := make([]map[string]any, len(snap))
	for i, r := range snap {
		row := map[string]any{
			"branch": r.Branch, "name": orNull(r.Name), "state": r.State, "tier": orNull(r.Tier), "engine": orNull(r.Engine),
			"crew_id": r.CrewID, "ts": r.TS, "age_s": max(now.UnixMilli()-r.TS, 0) / 1000,
			"host": host, "peer": true, "live": nil,
		}
		if why != "" {
			row["state"] = nil
			row["peer_error"] = why
		}
		rows[i] = row
	}
	return rows, nil
}

// orNull is s, or JSON null for "", as local roster rows give a missing field.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// localKey folds case, a refs/heads/ or heads/ prefix and trailing slashes, so
// a peer cannot dodge DropLocal by respelling a local id or branch.
func localKey(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "refs/heads/")
	s = strings.TrimPrefix(s, "heads/")
	return strings.TrimRight(s, "/")
}

// DropLocal removes peer rows that name a local crew id or a local worker
// branch: a peer's snapshot is another host's data, so a row that collides
// with a local one is a forgery or a mix-up, and never stands in for it.
// Rows with no crew_id or branch (an unreachable peer's placeholder) stay.
func DropLocal(rows []map[string]any, events []bus.Event) (kept []map[string]any, dropped int) {
	crews := map[string]bool{}
	branches := map[string]bool{}
	for _, e := range events {
		if e.CrewID != "" {
			crews[localKey(e.CrewID)] = true
		}
		if e.Branch != "" {
			branches[localKey(e.Branch)] = true
		}
		if b, ok := strings.CutPrefix(e.From, "worker:"); ok {
			b, _, _ = strings.Cut(b, "#")
			if b != "" {
				branches[localKey(b)] = true
			}
		}
	}
	for _, r := range rows {
		id, _ := r["crew_id"].(string)
		br, _ := r["branch"].(string)
		if (id != "" && crews[localKey(id)]) || (br != "" && branches[localKey(br)]) {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped
}
