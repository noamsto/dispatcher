package roster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// remoteTimeout bounds each host's answer, so one dead host cannot stall a roster.
const remoteTimeout = 30 * time.Second

// Capture runs argv on host and returns its status, with the output it printed
// in stdout and stderr.
type Capture func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int

// MergeRemote appends the rows of the crew's remote workers to the local rows:
// per host, the rows of `crew roster --live <crew>` run there, for the branches
// this bus dispatched to it, tagged with `host`. Hosts are asked at once and
// answer in host order. A host that cannot answer leaves one placeholder per
// branch, `state` and `live` null, `ts` the dispatch row's and the reason in
// `host_error`: it is never reported dead for having no local window. With no
// remote worker the rows come back untouched.
func MergeRemote(ctx context.Context, rows []map[string]any, events []bus.Event, crew string, capture Capture) []map[string]any {
	workers := remote.RemoteWorkers(events, crew)
	if len(workers) == 0 {
		return rows
	}
	var byHost [][]remote.Worker
	for len(workers) > 0 {
		n := 0
		for n < len(workers) && workers[n].Host == workers[0].Host {
			n++
		}
		byHost = append(byHost, workers[:n])
		workers = workers[n:]
	}
	answers := make([][]map[string]any, len(byHost))
	var wg sync.WaitGroup
	for i, ws := range byHost {
		wg.Go(func() { answers[i] = hostRows(ctx, ws[0].Host, ws, crew, capture) })
	}
	wg.Wait()
	merged := slices.Clone(rows)
	for _, a := range answers {
		merged = append(merged, a...)
	}
	return merged
}

func hostRows(ctx context.Context, host string, workers []remote.Worker, crew string, capture Capture) []map[string]any {
	reply, err := askHost(ctx, host, crew, capture)
	if err != nil {
		return placeholders(host, workers, err.Error())
	}
	var kept []map[string]any
	for _, r := range reply {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		branch, _ := row["branch"].(string)
		if slices.ContainsFunc(workers, func(w remote.Worker) bool { return w.Branch == branch }) {
			row["host"] = host
			kept = append(kept, row)
		}
	}
	return kept
}

// askHost is the host's roster rows, or why it gave none: the first line of its
// stderr when it failed, else its status.
func askHost(ctx context.Context, host, crew string, capture Capture) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if rc := capture(ctx, host, []string{"crew", "roster", "--live", "--", crew}, &stdout, &stderr); rc != 0 {
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			return nil, errors.New(line)
		}
		return nil, fmt.Errorf("exit status %d", rc)
	}
	dec := json.NewDecoder(&stdout)
	dec.UseNumber()
	var vs []any
	for {
		var v any
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("remote roster: %v", err)
		}
		vs = append(vs, v)
	}
	if len(vs) != 1 {
		return nil, errors.New("remote roster is not an array")
	}
	rows, ok := vs[0].([]any)
	if !ok {
		return nil, errors.New("remote roster is not an array")
	}
	return rows, nil
}

func placeholders(host string, workers []remote.Worker, reason string) []map[string]any {
	rows := make([]map[string]any, len(workers))
	for i, w := range workers {
		rows[i] = map[string]any{
			"branch":     w.Branch,
			"name":       w.Name,
			"host":       host,
			"state":      nil,
			"live":       nil,
			"ts":         jsonv.ToAny(w.TS),
			"host_error": reason,
		}
	}
	return rows
}
