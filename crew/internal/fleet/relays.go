package fleet

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/proc"
)

// RelayStatus is one host's relay of a crew: Live when its lock holder is a
// running process, CursorTS the cursor file's mtime in ms (null with no cursor).
type RelayStatus struct {
	Host     string `json:"host"`
	Live     bool   `json:"live"`
	CursorTS *int64 `json:"cursor_ts"`
}

// crewRelays reads the relay dir of the crew whose dir is crewDir, shared by
// the per-repo stream and a fleet stream lane: one entry per host with a lock
// or a cursor.
func crewRelays(crewDir string, procs proc.Table) []RelayStatus {
	dir := filepath.Join(crewDir, "relay")
	entries, _ := os.ReadDir(dir)
	hosts := map[string]bool{}
	for _, e := range entries {
		if h, ok := strings.CutSuffix(e.Name(), ".lock.d"); ok && e.IsDir() {
			hosts[h] = true
		} else if h, ok := strings.CutSuffix(e.Name(), ".cursor"); ok {
			hosts[h] = true
		}
	}
	out := []RelayStatus{}
	for _, h := range slices.Sorted(maps.Keys(hosts)) {
		r := RelayStatus{Host: h}
		if holder := lock.Holder(filepath.Join(dir, h+".lock.d")); lock.ValidHolder(holder) {
			pid, _ := strconv.Atoi(holder)
			r.Live = procs.Alive(pid)
		}
		if st, err := os.Stat(filepath.Join(dir, h+".cursor")); err == nil {
			ms := st.ModTime().UnixMilli()
			r.CursorTS = &ms
		}
		out = append(out, r)
	}
	return out
}
