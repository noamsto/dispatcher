package bus

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// IndexRepos is the header of every repo index under state, byte-sorted by
// Common. A file without a v1 header, or whose name is not IndexPath of its
// header's Common, is skipped.
func IndexRepos(state string) []IndexHeader {
	entries, err := os.ReadDir(indexDir(state))
	if err != nil {
		return nil
	}
	var hdrs []IndexHeader
	for _, e := range entries {
		if !indexFileName(e.Name()) {
			continue
		}
		hdr, ok := readIndexHeader(indexDir(state) + "/" + e.Name())
		if !ok || filepath.Base(IndexPath(state, hdr.Common)) != e.Name() {
			continue
		}
		hdrs = append(hdrs, hdr)
	}
	slices.SortFunc(hdrs, func(a, b IndexHeader) int { return strings.Compare(a.Common, b.Common) })
	return hdrs
}

// readIndexHeader is line 1 of an index file; ok is false for a missing file or
// one without a v1 header.
func readIndexHeader(path string) (IndexHeader, bool) {
	if !isFile(path) {
		return IndexHeader{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return IndexHeader{}, false
	}
	line, _ := bufio.NewReader(f).ReadBytes('\n')
	_ = f.Close()
	var hdr IndexHeader
	if json.Unmarshal(line, &hdr) != nil || hdr.V != indexVersion {
		return IndexHeader{}, false
	}
	return hdr, true
}

// RepoRows are the layout-2 crew rows that belong to one repo.
type RepoRows struct {
	Events []Event
	Crews  map[string]string // branch → id of the crew that owns it
}

// RepoCrewRows is the rows of the indexed live layout-2 crews, kept for each
// branch the crew dispatched in p's repo. A RepoTag error means no rows; a crew
// log that exists but cannot be read is an error naming the crew. When two
// crews claim a branch, the crew with the newest dispatch row wins and the
// loser's rows of it are dropped. A non-dispatch row whose repo field names
// another repo is dropped.
func RepoCrewRows(p Paths) (RepoRows, error) {
	if p.State == "" || p.Common == "" {
		return RepoRows{}, nil
	}
	tag, err := RepoTag(p.Common)
	if err != nil {
		return RepoRows{}, nil
	}
	if hdr, ok := readIndexHeader(IndexPath(p.State, tag)); !ok || hdr.Common != tag {
		return RepoRows{}, nil
	}
	type claim struct {
		id string
		ts float64
	}
	type crewRows struct {
		id   string
		rows []Event
		bs   []string
	}
	var crews []crewRows
	winner := map[string]claim{}
	for _, id := range IndexCrews(p.State, tag) {
		c, err := p.Crew(id)
		if err != nil || c.Layout != 2 {
			continue
		}
		evs, err := ReadEvents(c.Log)
		if errors.Is(err, ErrNoLog) {
			continue
		}
		if err != nil {
			return RepoRows{}, fmt.Errorf("crew %s: %w", id, err)
		}
		cr := crewRows{id: id}
		mine := map[string]float64{}
		for _, ev := range evs {
			b := rowBranch(ev)
			if b == "" {
				continue
			}
			obj, _ := ev.Doc.(map[string]any)
			repo, hasRepo := obj["repo"]
			if ev.Kind != KindDispatch && hasRepo && repo != tag {
				continue
			}
			cr.rows = append(cr.rows, ev)
			cr.bs = append(cr.bs, b)
			if ev.Kind == KindDispatch && ev.CrewID == id && stringField(obj, "repo") == tag {
				ts := rowTS(ev)
				if old, ok := mine[b]; !ok || ts > old {
					mine[b] = ts
				}
			}
		}
		for b, ts := range mine {
			if w, ok := winner[b]; !ok || ts > w.ts {
				winner[b] = claim{id: id, ts: ts}
			}
		}
		crews = append(crews, cr)
	}
	out := RepoRows{Crews: map[string]string{}}
	for b, w := range winner {
		out.Crews[b] = w.id
	}
	for _, cr := range crews {
		for i, ev := range cr.rows {
			if out.Crews[cr.bs[i]] == cr.id {
				out.Events = append(out.Events, ev)
			}
		}
	}
	return out, nil
}

// ReadRepoRows is the repo log (a missing one reads as none) in file order
// with RepoCrewRows merged in by ts, a tie keeping the repo log row first, and
// the crews' branches. Without crew rows the repo log comes back as read.
func ReadRepoRows(p Paths) (RepoRows, error) {
	evs, err := ReadEvents(p.Log)
	if err != nil && !errors.Is(err, ErrNoLog) {
		return RepoRows{}, err
	}
	rows, err := RepoCrewRows(p)
	if err != nil {
		return RepoRows{}, err
	}
	if len(rows.Events) == 0 {
		rows.Events = evs
		return rows, nil
	}
	crew := rows.Events
	sort.SliceStable(crew, func(i, j int) bool { return rowTS(crew[i]) < rowTS(crew[j]) })
	merged := make([]Event, 0, len(evs)+len(crew))
	i, j := 0, 0
	for i < len(evs) && j < len(crew) {
		if rowTS(crew[j]) < rowTS(evs[i]) {
			merged = append(merged, crew[j])
			j++
			continue
		}
		merged = append(merged, evs[i])
		i++
	}
	merged = append(merged, evs[i:]...)
	rows.Events = append(merged, crew[j:]...)
	return rows, nil
}

// ReadRepoEvents is ReadRepoRows' events.
func ReadRepoEvents(p Paths) ([]Event, error) {
	rows, err := ReadRepoRows(p)
	return rows.Events, err
}

func rowTS(ev Event) float64 {
	obj, _ := ev.Doc.(map[string]any)
	n, _ := obj["ts"].(json.Number)
	ts, _ := n.Float64()
	return ts
}

// rowBranch is the branch the reap folds file the event under: a worker From's
// (as their wid_branch derives it), else the event's branch, else the one a
// role From or the To names. A worker From that disagrees with the event's
// branch is "", so the row is dropped.
func rowBranch(ev Event) string {
	if rest, ok := strings.CutPrefix(ev.From, "worker:"); ok {
		b := workerBranch(rest)
		if ev.Branch != "" && ev.Branch != b {
			return ""
		}
		return b
	}
	if ev.Branch != "" {
		return ev.Branch
	}
	if b := addrBranch(ev.From); b != "" {
		return b
	}
	return addrBranch(ev.To)
}

// workerBranch is jq's `sub("#[^#]*$";"")` over a worker address's rest.
func workerBranch(rest string) string {
	if i := strings.LastIndex(rest, "#"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// addrBranch parses `worker:<b>` (optionally `#<session>`) and
// `role:<b>:<role>`; a branch may hold `/` and role names hold no `:`.
func addrBranch(addr string) string {
	if rest, ok := strings.CutPrefix(addr, "worker:"); ok {
		return workerBranch(rest)
	}
	if rest, ok := strings.CutPrefix(addr, "role:"); ok {
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			return rest[:i]
		}
	}
	return ""
}
