package roster

import (
	"bytes"
	"context"
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
func MergeRemote(ctx context.Context, rows jsonv.Value, events []bus.Event, crew string, capture Capture) jsonv.Value {
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
	answers := make([][]jsonv.Value, len(byHost))
	var wg sync.WaitGroup
	for i, ws := range byHost {
		wg.Go(func() { answers[i] = hostRows(ctx, ws[0].Host, ws, crew, capture) })
	}
	wg.Wait()
	merged := slices.Clone(rows.Elems())
	for _, a := range answers {
		merged = append(merged, a...)
	}
	return jsonv.Array(merged...)
}

func hostRows(ctx context.Context, host string, workers []remote.Worker, crew string, capture Capture) []jsonv.Value {
	reply, err := askHost(ctx, host, crew, capture)
	if err != nil {
		return placeholders(host, workers, err.Error())
	}
	var kept []jsonv.Value
	for _, row := range reply {
		b, _ := row.Get("branch")
		branch, _ := b.AsString()
		if slices.ContainsFunc(workers, func(w remote.Worker) bool { return w.Branch == branch }) && row.Kind() == jsonv.KindObject {
			row.Set("host", jsonv.Str(host))
			kept = append(kept, row)
		}
	}
	return kept
}

// askHost is the host's roster rows, or why it gave none: the first line of its
// stderr when it failed, else its status.
func askHost(ctx context.Context, host, crew string, capture Capture) ([]jsonv.Value, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if rc := capture(ctx, host, []string{"crew", "roster", "--live", "--", crew}, &stdout, &stderr); rc != 0 {
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			return nil, errors.New(line)
		}
		return nil, fmt.Errorf("exit status %d", rc)
	}
	vs, err := jsonv.DecodeStream(&stdout)
	if err != nil {
		return nil, fmt.Errorf("remote roster: %v", err)
	}
	if len(vs) != 1 || vs[0].Kind() != jsonv.KindArray {
		return nil, errors.New("remote roster is not an array")
	}
	return vs[0].Elems(), nil
}

func placeholders(host string, workers []remote.Worker, reason string) []jsonv.Value {
	rows := make([]jsonv.Value, len(workers))
	for i, w := range workers {
		rows[i] = jsonv.Object(
			jsonv.Member{Key: "branch", Val: jsonv.Str(w.Branch)},
			jsonv.Member{Key: "name", Val: jsonv.Str(w.Name)},
			jsonv.Member{Key: "host", Val: jsonv.Str(host)},
			jsonv.Member{Key: "state", Val: jsonv.Null()},
			jsonv.Member{Key: "live", Val: jsonv.Null()},
			jsonv.Member{Key: "ts", Val: w.TS},
			jsonv.Member{Key: "host_error", Val: jsonv.Str(reason)},
		)
	}
	return rows
}
