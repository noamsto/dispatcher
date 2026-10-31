// Package fleet implements `crew fleet`: one roster across every crew bus on
// the machine, discovered through the repo registry and the sweep roots.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/claimindex"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// Usage is the line every fleet grammar refusal prints.
const Usage = "crew: fleet [--all] [--crew ID] [--json]"

// Options carry every outside read, so tests use temp dirs and stubs.
type Options struct {
	Getenv   func(string) string
	Registry string
	Tmux     tmux.Client
	Git      git.Client
	Clock    clock.Clock
	// Remote asks one host for the rows of the crew's remote workers, from dir:
	// the checkout of the repo whose bus is being folded, since that checkout's
	// origin is what names the clone the call lands in. nil asks no host.
	Remote func(dir, crew string) roster.Capture
	// PeerHosts loads the settings hosts, once per run, for the peer rows of
	// each repo's peer store. nil shows no peer rows.
	PeerHosts func() (map[string]remote.Host, error)
}

// onceTmux reads each argv once and shares it, a failed read as no output, so
// every crew's fold sees one pane list.
type onceTmux struct {
	tmux.Client
	out map[string]string
}

func (m onceTmux) Run(ctx context.Context, args ...string) (string, error) {
	key := strings.Join(args, " ")
	if _, ok := m.out[key]; !ok {
		m.out[key] = tmux.Output(ctx, m.Client, args...)
	}
	return m.out[key], nil
}

// Request is `crew fleet`'s flags: Crew limits the rows to one crew id.
type Request struct {
	All  bool
	JSON bool
	Crew string
}

// Run lists the rows of every discovered bus. A repo it cannot read is noted
// on stderr and skipped, so one bad bus never blinds the machine view.
func Run(ctx context.Context, req Request, stdout, stderr io.Writer, o Options) int {
	all, asJSON, onlyCrew := req.All, req.JSON, req.Crew

	once := onceTmux{o.Tmux, map[string]string{}}
	windowBranches := func() map[string]bool { return roster.Windows(ctx, once) }
	now := o.Clock.Now()
	hidden := 0
	nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6

	var peerHosts func() (map[string]remote.Host, error)
	if o.PeerHosts != nil {
		peerHosts = sync.OnceValues(o.PeerHosts)
	}

	rows := []map[string]any{}
	for _, r := range repos.Discover(repos.Options{Registry: o.Registry, Getenv: o.Getenv, Stderr: stderr, Prefix: "crew: fleet"}) {
		if r.Status != repos.StatusOK {
			say(stderr, "crew: fleet: %s: skipped (%s)\n", r.Common, r.Status)
			continue
		}
		events, err := bus.ReadEventsTolerant(r.Common + "/crew/events.jsonl")
		var decode *bus.DecodeError
		switch {
		case errors.Is(err, bus.ErrNoLog):
			say(stderr, "crew: fleet: %s: skipped (%s)\n", r.Common, repos.StatusNoBus)
			continue
		case err != nil && !errors.As(err, &decode):
			say(stderr, "crew: fleet: %s: skipped (%s)\n", r.Common, oneLine(err))
			continue
		case decode != nil:
			say(stderr, "crew: fleet: %s: %s (rows from the readable prefix)\n", r.Common, oneLine(decode))
		}
		docs := make([]any, len(events))
		for i, e := range events {
			docs[i] = e.Doc
		}
		worktrees := sync.OnceValues(func() (string, error) { return git.Worktrees(ctx, o.Git, "", r.Common) })
		live := roster.Liveness{Windows: windowBranches, Worktrees: worktrees}
		// A discovered repo's checkout is its common dir's parent, the same
		// derivation `rate --sweep-all` makes, and it is that checkout's origin
		// that names the clone a host call lands in.
		var remote func(crew string) roster.Capture
		if o.Remote != nil {
			dir := filepath.Dir(r.Common)
			remote = func(crew string) roster.Capture { return o.Remote(dir, crew) }
		}
		repoRows, err := foldRepo(ctx, r.Common, events, docs, onlyCrew, nowSec, roster.Probes{
			Tmux:      once,
			Worktrees: worktrees,
		}, remote)
		if err != nil {
			say(stderr, "crew: fleet: %s: skipped (%s)\n", r.Common, oneLine(err))
			continue
		}
		origin, _ := git.Origin(ctx, o.Git, r.Common)
		name := repos.Name(r.Common, origin)
		peerRows, err := peer.RosterRows(peer.StoreFor(bus.Paths{Common: r.Common, Dir: r.Common + "/crew"}), peerHosts, now)
		if err != nil {
			say(stderr, "crew: fleet: %s: peer rows skipped (%s)\n", r.Common, oneLine(err))
		}
		peerRows, dropped := peer.DropLocal(peerRows, events)
		if dropped > 0 {
			say(stderr, "crew: fleet: %s: %d peer rows dropped (a local crew id or branch)\n", r.Common, dropped)
		}
		for _, p := range peerRows {
			id, _ := p["crew_id"].(string)
			if onlyCrew != "" && id != onlyCrew {
				continue
			}
			repoRows = append(repoRows, crewRow{Row: p, crew: id})
		}
		for _, row := range repoRows {
			row.Row["repo"] = name
			row.Row["crew_id"] = row.crew
			// A remote row answers for a window and a worktree on another host,
			// so this host's liveness reads nothing about it. A host that did not
			// answer has no state either, and no state is not a terminal one, so
			// --all never hides it.
			_, remoteRow := row.Row["host"].(string)
			_, unanswered := row.Row["host_error"]
			_, peerErr := row.Row["peer_error"]
			isPeer := row.Row["peer"] == true
			nonTerminal := unanswered || peerErr || slices.Contains(roster.LiveStates, str(row.Row, "state"))
			stale := !remoteRow && !isPeer && nonTerminal && live.Stale(row.Row)
			row.Row["stale"] = stale
			switch {
			case !all && !nonTerminal:
			case stale && !all && !asJSON:
				hidden++
			default:
				rows = append(rows, row.Row)
			}
		}
	}

	if hidden > 0 {
		say(stderr, "crew fleet: %d stale rows hidden (--all shows them)\n", hidden)
	}
	if asJSON {
		var buf bytes.Buffer
		if err := jsonval.Encode(&buf, rows, true); err != nil {
			say(stderr, "crew: fleet: %v\n", err)
			return 1
		}
		return write(stdout, stderr, buf.Bytes())
	}
	return write(stdout, stderr, table(rows))
}

type crewRow struct {
	Row  map[string]any
	crew string
}

// foldRepo folds each of the repo's crews: the one asked for, else every crew
// that has a worker status row, which is exactly what roster can produce rows
// for, and then merges each crew's rows with its remote workers'. remote is nil
// when the caller asks no host.
//
// Every fold runs before any host is asked: Fold reads the tmux probe's cache,
// which is not synchronized, and a fold that fails skips the repo with nobody
// waiting on a host behind it. The merges start together because a host that is
// down spends its whole timeout, and asking one crew at a time pays that per
// crew.
func foldRepo(ctx context.Context, common string, events []bus.Event, docs []any, onlyCrew string, nowSec float64, p roster.Probes, remote func(crew string) roster.Capture) ([]crewRow, error) {
	crews := []string{onlyCrew}
	if onlyCrew == "" {
		crews = crews[:0]
		for _, e := range events {
			if e.Kind == bus.KindStatus && e.CrewID != "" && strings.HasPrefix(e.From, "worker:") {
				crews = append(crews, e.CrewID)
			}
		}
		crews = append(crews, recentRemoteCrews(events, nowSec)...)
		slices.Sort(crews)
		crews = slices.Compact(crews)
	}
	folded := make([][]map[string]any, len(crews))
	for i, c := range crews {
		rows, err := roster.Fold(ctx, docs, c, nowSec, p)
		if err != nil {
			return nil, err
		}
		folded[i] = rows
	}
	rows := folded
	if remote != nil {
		rows = make([][]map[string]any, len(crews))
		var wg sync.WaitGroup
		for i, c := range crews {
			wg.Go(func() {
				rows[i] = roster.MergeRemote(ctx, folded[i], events, c, remote(c))
			})
		}
		wg.Wait()
	}
	var out []crewRow
	for i, c := range crews {
		for _, row := range rows[i] {
			out = append(out, crewRow{Row: row, crew: c})
		}
	}
	return out, nil
}

// recentRemoteCrews are the crews whose newest remote dispatch row is younger
// than claimindex.StaleAfter and whose branch has no reaped row. A remote
// worker's status lands on its host's bus, so such a crew can have no status
// row here for foldRepo to find.
func recentRemoteCrews(events []bus.Event, nowSec float64) []string {
	type remoteDispatch struct {
		branch string
		ts     float64
	}
	type crewBranch struct{ crew, branch string }
	newest := map[string]remoteDispatch{}
	reaped := map[crewBranch]bool{}
	for _, e := range events {
		doc, ok := e.Doc.(map[string]any)
		if !ok || e.CrewID == "" {
			continue
		}
		if e.Kind == bus.KindDispatch {
			if host, _ := doc["host"].(string); host == "" || doc["remote"] != true {
				continue
			}
			ms, _ := doc["ts"].(json.Number)
			ts, _ := ms.Float64()
			newest[e.CrewID] = remoteDispatch{branch: e.Branch, ts: ts}
		}
		if e.Kind == bus.KindStatus {
			body, _ := doc["body"].(map[string]any)
			if body["state"] == "reaped" && strings.HasPrefix(e.From, "worker:") {
				reaped[crewBranch{e.CrewID, workerBranch(e.From)}] = true
			}
		}
	}
	var crews []string
	for crew, d := range newest {
		if nowSec-d.ts/1000 < claimindex.StaleAfter.Seconds() && !reaped[crewBranch{crew, d.branch}] {
			crews = append(crews, crew)
		}
	}
	return crews
}

// workerBranch is the branch in a worker's from, `worker:<branch>#<session>`.
func workerBranch(from string) string {
	b := strings.TrimPrefix(from, "worker:")
	if i := strings.LastIndex(b, "#"); i >= 0 {
		b = b[:i]
	}
	return b
}

// oneLine flattens an error to one line: git's stderr ends in a newline and
// may span several.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// say writes to a stream whose failure is not worth failing the command over.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func write(stdout, stderr io.Writer, b []byte) int {
	if _, err := stdout.Write(b); err != nil {
		say(stderr, "crew: fleet: %v\n", err)
		return 1
	}
	return 0
}

func table(rows []map[string]any) []byte {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	say(tw, "REPO\tCODENAME\tSTATE\tAGE\tENGINE\tCREW\tBRANCH\tTITLE\n")
	for _, r := range rows {
		engine := strings.Join([]string{str(r, "engine"), str(r, "model"), str(r, "tier")}, "/")
		name, state := str(r, "name"), str(r, "state")
		if r["peer"] == true {
			name += "@" + str(r, "host")
			if _, ok := r["peer_error"]; ok {
				state = str(r, "peer_error")
			}
		}
		say(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			str(r, "repo"), name, state, age(r), engine, str(r, "crew_id"), str(r, "branch"), str(r, "title"))
	}
	_ = tw.Flush()
	return buf.Bytes()
}

// str is a row field as table text: "-" for null or anything but a string.
// Workers write these fields, so control runes become "?" before they can
// forge a row or drive the terminal.
func str(row map[string]any, key string) string {
	if s, ok := row[key].(string); ok && s != "" {
		return strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return '?'
			}
			return r
		}, s)
	}
	return "-"
}

func age(row map[string]any) string {
	var s int64
	switch v := row["age_s"].(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "-"
		}
		s = int64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return "-"
		}
		s = int64(f)
	default:
		return "-"
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh%dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
}
