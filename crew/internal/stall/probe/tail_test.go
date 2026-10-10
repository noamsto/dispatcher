package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// rowAt is one log row: a status of `from` at ts, the shape refresh filters on.
func rowAt(ts int64, from string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":"dispatcher:c1","kind":"status","body":{"state":"working","detail":"x"}}`, ts, from)
}

// writeLog writes n rows older than runStart followed by tail, and returns the
// path and its size.
func writeLog(t *testing.T, n int, runStart int64, tail ...string) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range n {
		// One row a second apart: a log's rows are seconds apart, and the
		// disorder margin only reaches back over a few minutes of them.
		fmt.Fprintf(&b, "%s\n", rowAt(runStart-int64(n-i)*1000, "worker:old/x"))
	}
	for _, r := range tail {
		b.WriteString(r + "\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, st.Size()
}

func TestBusRowsRunFloor(t *testing.T) {
	const run = int64(1_700_000_000_000)
	// want names each row by its ts: a number, the string, or "-" for none.
	cases := []struct {
		name    string
		rows    []string
		sinceMS int64
		want    []string
	}{
		{"older rows dropped", []string{
			rowAt(run-1, "worker:a"), rowAt(run, "worker:b"), rowAt(run+5, "worker:c"),
		}, run, []string{itoa(run), itoa(run + 5)}},
		{"boundary row kept", []string{rowAt(run, "worker:b")}, run, []string{itoa(run)}},
		{"no floor keeps everything", []string{
			rowAt(run-9e7, "worker:a"), rowAt(run, "worker:b"),
		}, 0, []string{itoa(run - 9e7), itoa(run)}},
		// A row whose ts is not a number cannot be shown older, so the reader
		// keeps it and the caller's jq — where a string outranks a number — is
		// what drops it. Same for a row with no ts at all.
		{"non-numeric ts is not older", []string{
			`{"ts":"1900000000000","crew_id":"c1","kind":"status"}`, rowAt(run+1, "worker:b"),
		}, run, []string{"1900000000000", itoa(run + 1)}},
		{"row without ts is not older", []string{`{"kind":"status"}`, rowAt(run+1, "worker:b")}, run, []string{"-", itoa(run + 1)}},
		// A row appended out of ts order — two writers, each stamping before it
		// appends — must not hide the newer row written before it.
		{"an older row does not hide a newer one", []string{
			rowAt(run+1, "worker:newest"), rowAt(run-30_000, "worker:old"),
		}, run, []string{itoa(run + 1)}},
		// The scan stops where it meets a row older than the floor by more than
		// the disorder margin: rows appended before that one are older still.
		{"scan stops past the margin", []string{
			rowAt(run+2, "worker:oldest"), rowAt(run-tailDisorder-1, "worker:old"), rowAt(run+3, "worker:newer"),
		}, run, []string{itoa(run + 3)}},
		{"blank lines and a torn tail are skipped", []string{
			rowAt(run, "worker:b"), "", `{"ts":`,
		}, run, []string{itoa(run)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body strings.Builder
			for _, r := range tc.rows {
				body.WriteString(r + "\n")
			}
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
				t.Fatal(err)
			}
			rows, ok := BusRows(path, tc.sinceMS, 0)
			if !ok {
				t.Fatal("ok = false")
			}
			if len(rows) != len(tc.want) {
				t.Fatalf("%d rows, want %d", len(rows), len(tc.want))
			}
			for i, want := range tc.want {
				if got := tsOf(t, rows[i]); got != want {
					t.Errorf("row %d ts = %s, want %s", i, got, want)
				}
			}
		})
	}
}

// tsOf names a row by its ts: the number, the string, or "-" for a row without
// one.
func tsOf(t *testing.T, v jsonv.Value) string {
	t.Helper()
	ts, ok := v.Get("ts")
	if !ok {
		return "-"
	}
	if f, ok := ts.AsFloat(); ok {
		return itoa(int64(f))
	}
	s, _ := ts.AsString()
	return s
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestBusRowsMaxLines(t *testing.T) {
	const run = int64(1_700_000_000_000)
	var rows []string
	for i := range 10 {
		rows = append(rows, rowAt(run+int64(i), fmt.Sprintf("worker:x%d", i)))
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	// The torn tail counts as a line read, the way `tail -n` counts it.
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n{\"ts\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ maxLines, want int }{{1, 0}, {3, 2}, {10, 9}, {11, 10}, {1000, 10}}
	for _, tc := range cases {
		got, ok := BusRows(path, 0, tc.maxLines)
		if !ok {
			t.Fatal("ok = false")
		}
		if len(got) != tc.want {
			t.Errorf("maxLines %d = %d rows, want %d", tc.maxLines, len(got), tc.want)
		}
	}
}

// A log larger than one window is read in windows that double: every row of the
// run comes back once, in order, including one the first window split in two.
func TestBusRowsDoublesWindows(t *testing.T) {
	const run = int64(1_700_000_000_000)
	// Old rows past the first window, then the run's rows, one of which is
	// large enough to straddle the window boundary at tailChunk.
	big := rowAt(run+2, "worker:"+strings.Repeat("b", tailChunk-60))
	path, size := writeLog(t, 4*tailChunk/140, run,
		rowAt(run+1, "worker:a"), big, rowAt(run+3, "worker:c"))
	if size < 2*tailChunk {
		t.Fatalf("log is %d bytes, want more than two windows", size)
	}
	rows, ok := BusRows(path, run, 0)
	if !ok {
		t.Fatal("ok = false")
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	for i, want := range []int64{run + 1, run + 2, run + 3} {
		v, _ := rows[i].Get("ts")
		got, _ := v.AsFloat()
		if int64(got) != want {
			t.Errorf("row %d ts = %d, want %d", i, int64(got), want)
		}
	}
	// The straddling row survived both windows whole, not split or doubled.
	v, _ := rows[1].Get("from")
	s, _ := v.AsString()
	if s != "worker:"+strings.Repeat("b", tailChunk-60) {
		t.Errorf("the straddling row was misread: %d bytes, want %d", len(s), tailChunk-60+7)
	}
}

// allocBytes is what one call put on the heap, at its smallest over a few runs.
func allocBytes(t *testing.T, f func()) uint64 {
	t.Helper()
	var best uint64
	for range 3 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		got := after.TotalAlloc - before.TotalAlloc
		if best == 0 || got < best {
			best = got
		}
	}
	return best
}

// The tail reader's cost tracks the run's rows, not the log's: rows written
// before the run started — earlier runs on this branch, other branches'
// traffic — must cost nothing.
func TestBusRowsAllocationDoesNotGrowWithOldRows(t *testing.T) {
	const run = int64(1_700_000_000_000)
	const inRun = 5
	measure := func(t *testing.T, oldRows int) (allocated uint64, size int64) {
		t.Helper()
		path, size := writeLog(t, oldRows, run, rowsInRun(run, inRun)...)
		allocated = allocBytes(t, func() {
			rows, ok := BusRows(path, run, 0)
			if !ok {
				t.Fatalf("ok = false on a %d B log", size)
			}
			// Rows older than the run must not reach the reader at all, or
			// this measures their decoding.
			if len(rows) != inRun {
				t.Fatalf("%d rows, want %d on a %d B log", len(rows), inRun, size)
			}
		})
		return allocated, size
	}

	// One just past a single window, one a hundred times larger.
	small, smallSize := measure(t, tailChunk/140+10)
	big, bigSize := measure(t, 100*(tailChunk/140))
	if bigSize < 50*smallSize {
		t.Fatalf("the large log is %d B, want 50x the %d B one", bigSize, smallSize)
	}
	t.Logf("%d B log -> %d B, %d B log -> %d B", smallSize, small, bigSize, big)
	if limit := small + small/2 + 128<<10; big > limit {
		t.Errorf("a %d B log allocated %d B, want at most %d (a %d B log allocated %d B)", bigSize, big, limit, smallSize, small)
	}
	// And the bound is a window, not a fraction of the file: decoding every row,
	// which is what this reader replaced, costs an order of magnitude more.
	path, size := writeLog(t, 100*(tailChunk/140), run, rowsInRun(run, inRun)...)
	if whole := allocBytes(t, func() { wholeLogRead(t, path) }); whole < 10*big {
		t.Errorf("the whole-log read of a %d B log allocated %d B against the tail read's %d B; this test would not catch that regression", size, whole, big)
	}
}

// rowsInRun is `n` rows of this run, newest last.
func rowsInRun(run int64, n int) []string {
	var out []string
	for i := range n {
		out = append(out, rowAt(run+int64(i)*1000, "worker:feat/x"))
	}
	return out
}

// wholeLogRead is what refresh did before: decode every row of the file.
func wholeLogRead(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := jsonv.DecodeStreamPrefix(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
}
