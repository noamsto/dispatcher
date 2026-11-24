package bus

import (
	"bufio"
	"encoding/json"
	"errors"
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
// whose log cannot be read is skipped. When two crews claim a branch, the crew
// with the newest dispatch row wins and the loser's rows of it are dropped.
func RepoCrewRows(p Paths) RepoRows {
	if p.State == "" || p.Common == "" {
		return RepoRows{}
	}
	tag, err := RepoTag(p.Common)
	if err != nil {
		return RepoRows{}
	}
	if hdr, ok := readIndexHeader(IndexPath(p.State, tag)); !ok || hdr.Common != tag {
		return RepoRows{}
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
		if err != nil {
			continue
		}
		cr := crewRows{id: id}
		mine := map[string]float64{}
		for _, ev := range evs {
			b := rowBranch(ev)
			if b == "" {
				continue
			}
			cr.rows = append(cr.rows, ev)
			cr.bs = append(cr.bs, b)
			if obj, _ := ev.Doc.(map[string]any); ev.Kind == KindDispatch && ev.CrewID == id && stringField(obj, "repo") == tag {
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
	return out
}

// ReadRepoEvents is the repo log (a missing one reads as none) followed by
// RepoCrewRows, stable by ts so a tie keeps the repo log row first.
func ReadRepoEvents(p Paths) ([]Event, error) {
	evs, err := ReadEvents(p.Log)
	if err != nil && !errors.Is(err, ErrNoLog) {
		return nil, err
	}
	evs = append(evs, RepoCrewRows(p).Events...)
	sort.SliceStable(evs, func(i, j int) bool { return rowTS(evs[i]) < rowTS(evs[j]) })
	return evs, nil
}

func rowTS(ev Event) float64 {
	obj, _ := ev.Doc.(map[string]any)
	n, _ := obj["ts"].(json.Number)
	ts, _ := n.Float64()
	return ts
}

// rowBranch is the event's branch, else the one its From or To names.
func rowBranch(ev Event) string {
	if ev.Branch != "" {
		return ev.Branch
	}
	if b := addrBranch(ev.From); b != "" {
		return b
	}
	return addrBranch(ev.To)
}

// addrBranch parses `worker:<b>` (optionally `#s<sid>`) and `role:<b>:<role>`;
// a branch may hold `/` and role names hold no `:`.
func addrBranch(addr string) string {
	if rest, ok := strings.CutPrefix(addr, "worker:"); ok {
		if i := strings.LastIndex(rest, "#s"); i >= 0 {
			rest = rest[:i]
		}
		return rest
	}
	if rest, ok := strings.CutPrefix(addr, "role:"); ok {
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			return rest[:i]
		}
	}
	return ""
}
