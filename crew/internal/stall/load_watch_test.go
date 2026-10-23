package stall

import (
	"testing"
)

// D4 whole-watch tests: the rows a watch writes for a host that stays loaded,
// over a real run rather than one detector tick. The parse contract, the number
// comparison and the top suffix are probe_detectors_test.go's.

const (
	// The 1m load and core count every case below answers with.
	loadedLine = "99.9 32"
	idleLine   = "1.0 32"
	topRows    = "dispatcher/feat-187 yes 4242 99.0\n/tmp/x cc1 9999 44.0"
	// One --load window of a sustained 99.9, with the consumers D4 named.
	loadedDetail = "blocked|load: 1m load 99.9 on 32 cores for 2s " +
		"(top: dispatcher/feat-187 yes 4242 99.0 | /tmp/x cc1 9999 44.0)"
	// The same, with nobody to name.
	loadedBare = "blocked|load: 1m load 99.9 on 32 cores for 2s"
)

// loadSampler is the bats stall_load_sampler: these lines, one per read, the
// last one forever.
func loadSampler(lines ...string) func(int) string {
	return func(read int) string { return lines[min(read-1, len(lines)-1)] }
}

func TestLoadWatchRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		load func(int) string
		top  string
		// noEngine is the watch a dispatcher launches without --engine; every
		// other case runs as codex, the engine the bats cases used.
		noEngine bool
		idle     string
		maxLife  string
		want     []string
	}{
		{
			name:    "posts one load: once the 1m load stays above the cores for --load",
			load:    loadSampler(loadedLine),
			top:     topRows,
			maxLife: "8",
			want:    []string{loadedDetail},
		},
		{
			name:    "is silent at or below the core count",
			load:    loadSampler(idleLine),
			maxLife: "5",
		},
		{
			name:    "ignores a transient burst shorter than --load",
			load:    loadSampler(loadedLine, idleLine),
			maxLife: "6",
		},
		{
			name:    "clears itself when the load drops back to the cores",
			load:    loadSampler(loadedLine, loadedLine, loadedLine, loadedLine, loadedLine, loadedLine, loadedLine, loadedLine, idleLine),
			maxLife: "12",
			want:    []string{loadedBare, "working|load: cleared"},
		},
		{
			name:    "is supersedable by quiet: and does not re-post stale",
			load:    loadSampler(loadedLine, loadedLine, loadedLine, loadedLine, loadedLine, loadedLine, idleLine, loadedLine, idleLine),
			idle:    "2",
			maxLife: "14",
			// load: and quiet: are mutually non-sticky, so the static pane lets
			// quiet: overwrite the load: detail; the clear that follows the drop
			// no-ops on the wrong prefix but still resets the window, so the lone
			// later high sample posts nothing.
			want: []string{loadedBare, "blocked|quiet: pane unchanged for 2s"},
		},
		{
			name:    "stays silent on a malformed load read",
			load:    loadSampler("garbage"),
			maxLife: "4",
		},
		{
			name:    "runs whatever the engine: codex",
			load:    loadSampler(loadedLine),
			maxLife: "8",
			want:    []string{loadedBare},
		},
		{
			name:     "runs whatever the engine: no --engine",
			load:     loadSampler(loadedLine),
			noEngine: true,
			maxLife:  "8",
			want:     []string{loadedBare},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Load: ptr("2"), MaxLife: ptr(tc.maxLife)}
			if !tc.noEngine {
				req.Engine = ptr("codex")
			}
			if tc.idle != "" {
				req.Idle = ptr(tc.idle)
			}
			h, _ := watchRun(t, watchScript{
				load:     tc.load,
				top:      tc.top,
				noEngine: tc.noEngine,
				req:      req,
			})
			wantRows(t, h, tc.want...)
		})
	}
}
