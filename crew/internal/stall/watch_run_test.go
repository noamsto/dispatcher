package stall

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// Whole-watch driver shared by the quota, load and role-mode tests: one Run
// over a scripted pane, on the virtual clock with --max-life as the only bound,
// so no test sleeps in real time and none of them reaches a tmux server.
//
// The detector-level halves of the same behaviour stay where they are (the
// fdScript rigs and the pdWatch ticks); this is for the cases whose claim is
// about the rows a whole run leaves on the bus.

// watchScript is one watch run: the pane's frames one per sample (the last one
// repeats forever), the host and process reads the test scripts, and the
// Request the watch runs under.
type watchScript struct {
	// frames are the pane's captures by fixture name; empty is one idle box.
	frames []string
	// texts are pane captures given as text instead of by name, for a run that
	// rewrites one line of a fixture per sample (D2's meter clock). It wins over
	// frames.
	texts []string
	// load answers each D4 read, 1-based; nil is the empty read of a host
	// whose load cannot be parsed.
	load func(read int) string
	// top is D4's consumer list.
	top string
	// paneCmd is the pane's command: D5, quiet:'s dead: corroboration and the
	// role end-of-life exit all read it.
	paneCmd string
	// noEngine leaves --engine off, the way a watchdog launched without one
	// runs.
	noEngine bool
	// anchor is what the #908 sender check sees; zero checks nothing, which is
	// how every other run here reaches the loop.
	anchor sender.Probes
	req    Request
}

// watchRun runs the watch to its exit and returns its harness with every
// status row the run wrote, as from|state|source|detail.
func watchRun(t *testing.T, s watchScript) (*harness, []string) {
	t.Helper()
	f := newFake()
	texts := s.texts
	if len(texts) == 0 {
		frames := s.frames
		if len(frames) == 0 {
			frames = []string{"idle_box.txt"}
		}
		texts = make([]string, len(frames))
		for i, name := range frames {
			texts[i] = frameText(t, name)
		}
	}
	f.sample = func(n int) (string, bool) { return texts[min(n, len(texts)-1)], true }
	f.paneCmd = s.paneCmd
	// D8 quiet, the way it is on a machine with no exhausted window.
	f.sh = budgetSh("", 1, "", 1)

	req := s.req
	if req.Target == "" {
		req.Target = "worker:feat/x"
	}
	def := func(p **string, v string) {
		if *p == nil {
			*p = ptr(v)
		}
	}
	def(&req.Pane, "%1")
	if !s.noEngine {
		def(&req.Engine, "claude")
	}
	def(&req.Grace, "0")
	def(&req.Interval, "1")
	def(&req.Window, "0")
	def(&req.Idle, "999")
	def(&req.Dead, "999")
	def(&req.BudgetRefresh, "0")
	def(&req.MaxLife, "6")

	h := newHarness(t, f)
	o := h.options()
	o.Sender = s.anchor
	if s.load != nil || s.top != "" {
		newProbes := o.NewProbes
		reads := 0
		o.NewProbes = func(pane string) probe.Probes {
			p := newProbes(pane)
			if s.load != nil {
				p.Load = func(context.Context) string {
					reads++
					return s.load(reads)
				}
			}
			if s.top != "" {
				p.Top = func(context.Context) string { return s.top }
			}
			return p
		}
	}
	if code := Run(h.ctx, req, h.paths, &h.stderr, o); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	return h, statusRows(t, h)
}

// statusRows is every status row on the bus as from|state|source|detail, the
// shape the bats cases read off it with jq.
func statusRows(t *testing.T, h *harness) []string {
	t.Helper()
	data, err := os.ReadFile(h.paths.Log)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var rows []string
	for {
		var v any
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		row, _ := v.(map[string]any)
		if k, _ := row["kind"].(string); k != "status" {
			continue
		}
		body, _ := row["body"].(map[string]any)
		from, _ := row["from"].(string)
		st, _ := body["state"].(string)
		dt, _ := body["detail"].(string)
		src, ok := body["source"].(string)
		if !ok {
			src = "null"
		}
		rows = append(rows, strings.Join([]string{from, st, src, dt}, "|"))
	}
	return rows
}
