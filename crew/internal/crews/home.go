package crews

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/proc"
)

// HomePath is the home record of crew id: `$dir/homes/<id>`, holding the git
// common dir of the repo the crew's dispatcher runs in. It lives beside
// crews/, not inside it, so `crew deregister` removing the crew dir keeps it.
func HomePath(paths bus.Paths, id string) string { return paths.Dir + "/homes/" + id }

// WriteHome records this repo as crew id's home. Only a dispatcher acting in
// its own repo calls it: a cross-repo dispatch re-attaches through
// `adopt --reattach`, which writes it only after proving the cwd.
func WriteHome(paths bus.Paths, id string) error {
	if err := os.MkdirAll(paths.Dir+"/homes", 0o755); err != nil {
		return err
	}
	return os.WriteFile(HomePath(paths, id), []byte(paths.Common+"\n"), 0o644)
}

// RepoVerdict is where a process's cwd sits relative to a bus.
type RepoVerdict int

const (
	// RepoUnknown is no cwd entry for the pid (macOS, a vanished process) or a
	// cwd that is no git repo: no evidence either way.
	RepoUnknown RepoVerdict = iota
	RepoHere
	RepoElsewhere
)

// cwdIndex answers "which common dir does this pid's cwd resolve to", reading
// the process table once and resolving each distinct cwd once.
type cwdIndex struct {
	ctx    context.Context
	tbl    proc.Table
	dirs   map[int]string
	loaded bool
	repos  map[string]string
}

func (c *cwdIndex) common(pid int) (string, bool) {
	if !c.loaded {
		c.loaded = true
		c.dirs = map[int]string{}
		c.repos = map[string]string{}
		for _, e := range c.tbl.Cwds(c.ctx) {
			c.dirs[e.PID] = e.Dir
		}
	}
	dir, ok := c.dirs[pid]
	if !ok {
		return "", false
	}
	if r, seen := c.repos[dir]; seen {
		return r, r != ""
	}
	r := ""
	if p, err := bus.Locate(c.ctx, dir); err == nil {
		r = realpath(p.Common)
	}
	c.repos[dir] = r
	return r, r != ""
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// PidRepo compares the repo pid's cwd resolves to with paths' repo.
func PidRepo(ctx context.Context, t proc.Table, paths bus.Paths, pid int) RepoVerdict {
	c, ok := (&cwdIndex{ctx: ctx, tbl: t}).common(pid)
	switch {
	case !ok:
		return RepoUnknown
	case c == realpath(paths.Common):
		return RepoHere
	}
	return RepoElsewhere
}

// Homes is the per-run home predicate. The pidfile.log and the process table
// are each read at most once, and only when a crew has no home record.
type Homes struct {
	ctx     context.Context
	paths   bus.Paths
	o       Options
	common  string
	cwds    cwdIndex
	logRead bool
	logged  map[string]bool
}

// NewHomes is the predicate for paths' repo.
func NewHomes(ctx context.Context, paths bus.Paths, o Options) *Homes {
	return &Homes{ctx: ctx, paths: paths, o: o, common: realpath(paths.Common),
		cwds: cwdIndex{ctx: ctx, tbl: o.Proc}}
}

// IsHome reports whether crew id's dispatcher lives in this repo, as opposed
// to having only dispatched workers into it from another one.
//
// A `homes/<id>` record naming this repo is home, live or dead. A record
// naming another repo is only a hint (the repo may have moved or be
// bind-mounted), so it falls through to the legacy checks, as does a crew with
// no record (minted before homes existed): home when pidfile.log holds a
// `register ok`, `adopt ok` or `adopt forced` line for it — only a dispatcher
// in its own repo writes those; a cross-repo re-attach logs `reattach` — or
// when its recorded pid is live and that process's cwd resolves to this repo.
// Anything else is foreign. Where Cwds is unavailable (macOS) the cwd test
// never matches, so nothing is wrongly claimed.
func (h *Homes) IsHome(id string) bool {
	if data, err := os.ReadFile(HomePath(h.paths, id)); err == nil {
		if realpath(strings.TrimRight(string(data), "\n")) == h.common {
			return true
		}
	}
	if h.loggedHome(id) {
		return true
	}
	bc, err := h.paths.Crew(id)
	if err != nil {
		return false
	}
	pidfile := bc.Dir + "/pid"
	text, _ := PidFileText(pidfile)
	n, ok := validPid(text)
	if !ok || !RecordedLive(h.o.Proc, h.o.Clock.Now(), n, pidfile) {
		return false
	}
	c, ok := h.cwds.common(n)
	return ok && c == h.common
}

func (h *Homes) loggedHome(id string) bool {
	if !h.logRead {
		h.logRead = true
		h.logged = map[string]bool{}
		if f, err := os.Open(h.paths.Dir + "/pidfile.log"); err == nil {
			defer func() { _ = f.Close() }()
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				// "<ts> <action> <outcome> crew=<id> old=..."
				fs := strings.Fields(sc.Text())
				if len(fs) >= 4 && (fs[2] == "ok" && (fs[1] == "register" || fs[1] == "adopt") || fs[1] == "adopt" && fs[2] == "forced") {
					if v, ok := strings.CutPrefix(fs[3], "crew="); ok {
						h.logged[v] = true
					}
				}
			}
		}
	}
	return h.logged[id]
}
