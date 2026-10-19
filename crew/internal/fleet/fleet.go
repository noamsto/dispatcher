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
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// Usage is the line every fleet grammar refusal prints.
const Usage = "crew: fleet [--all] [--crew ID] [--json]"

// Options carry every outside read, so tests use temp dirs and stubs.
type Options struct {
	Env      func(string) string
	Registry string
	Tmux     tmux.Client
	Git      git.Client
	Clock    clock.Clock
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

	rows := []map[string]any{}
	for _, r := range repos.Discover(repos.Options{Registry: o.Registry, Env: o.Env, Stderr: stderr, Prefix: "crew: fleet"}) {
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
		repoRows, err := foldRepo(ctx, r.Common, events, docs, onlyCrew, nowSec, roster.Probes{
			Tmux:      once,
			Worktrees: worktrees,
		})
		if err != nil {
			say(stderr, "crew: fleet: %s: skipped (%s)\n", r.Common, oneLine(err))
			continue
		}
		origin, _ := git.Origin(ctx, o.Git, r.Common)
		name := repos.Name(r.Common, origin)
		for _, row := range repoRows {
			row.Row["repo"] = name
			row.Row["crew_id"] = row.crew
			nonTerminal := slices.Contains(roster.LiveStates, str(row.Row, "state"))
			stale := nonTerminal && live.Stale(row.Row)
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
// that has a worker status row, which is exactly what roster can produce rows for.
func foldRepo(ctx context.Context, common string, events []bus.Event, docs []any, onlyCrew string, nowSec float64, p roster.Probes) ([]crewRow, error) {
	crews := []string{onlyCrew}
	if onlyCrew == "" {
		crews = crews[:0]
		for _, e := range events {
			if e.Kind == bus.KindStatus && e.CrewID != "" && strings.HasPrefix(e.From, "worker:") {
				crews = append(crews, e.CrewID)
			}
		}
		slices.Sort(crews)
		crews = slices.Compact(crews)
	}
	var out []crewRow
	for _, c := range crews {
		folded, err := roster.Fold(ctx, docs, c, nowSec, p)
		if err != nil {
			return nil, err
		}
		for _, row := range folded {
			out = append(out, crewRow{Row: row, crew: c})
		}
	}
	return out, nil
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
		say(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			str(r, "repo"), str(r, "name"), str(r, "state"), age(r), engine, str(r, "crew_id"), str(r, "branch"), str(r, "title"))
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
