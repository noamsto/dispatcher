package probe

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// tailChunk is the first window the bus tail reader reads. It doubles until a
// stop or the head of the file, so the first run-scope read of a 3 MB log costs
// 5 windows and of a 30 MB one 8.
const tailChunk = 256 << 10

// tailDisorder is how far below its floor a row may sit and still end the scan,
// in ms. A writer stamps a row before appending it, so two writers can land the
// later row first; without a margin that slip hides the newer row, and a
// watchdog that reads no status at all treats a worker that has gone pr_open
// like one it should nudge. Five minutes is orders past any stamp-to-append
// latency, and the previous run it has to reach is hours away.
const tailDisorder = 5 * 60 * 1000

// BusRows is the production Probes.BusRows: the log's rows, oldest first, read
// from the tail.
//
// The log is append-only with one JSON row per line, and both callers only ever
// want its end — refresh the rows at or after sinceMS, nudged the last maxLines
// lines — so decoding all of it costs memory and time proportional to the whole
// file (about 13x its size in allocations), paid by every resident watchdog
// every 4th tick. Instead the reader takes a window off the end, walks its
// complete lines newest first, and stops at the first row older than the run or
// once maxLines lines are read; a window that runs out before either stop
// doubles. Rows are appended in ts order but not strictly (see tailDisorder),
// and a row whose ts is not a number cannot be shown older and is kept, which
// is what the callers' jq (`.ts >= $t0`, where strings outrank numbers) does.
//
// A line that does not decode to exactly one value is skipped — the arm's
// `jq -R 'fromjson?'` — so the torn tail of a concurrent append drops out alone.
func BusRows(path string, sinceMS int64, maxLines int) ([]jsonv.Value, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	size := st.Size()
	window := int64(tailChunk)
	if window > size {
		window = size
	}
	for {
		rows, stop, ok := scanWindow(f, size, window, sinceMS, maxLines)
		if !ok {
			return nil, false
		}
		if stop || window >= size {
			return rows, true
		}
		if window *= 2; window > size {
			window = size
		}
	}
}

// scanWindow reads the log's last window bytes and walks their complete lines
// newest first, returning the rows it kept oldest first. stop is true when the
// caller may return them: an older-than-sinceMS row ended the scan, maxLines
// lines were read, or the window covers the file.
func scanWindow(f *os.File, size, window, sinceMS int64, maxLines int) (rows []jsonv.Value, stop, ok bool) {
	buf := make([]byte, window)
	if window > 0 {
		if _, err := f.ReadAt(buf, size-window); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, false
		}
		// A window that does not start at the head starts inside a line, unless
		// the byte before it is the previous row's newline. That row belongs to
		// an older window: dropped here, read whole once the window doubles.
		if window < size && !afterNewline(f, size-window) {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				return nil, false, true
			}
			buf = buf[i+1:]
		}
	}
	// The last split element is whatever follows the final newline: nothing for
	// a whole log, the torn tail of an in-flight append otherwise. Neither
	// decodes to a row, so the same rule skips both.
	parts := bytes.Split(buf, []byte("\n"))
	stop = window == size
	rev := make([]jsonv.Value, 0, len(parts))
	read := 0
	for i := len(parts) - 1; i >= 0; i-- {
		if len(parts[i]) == 0 {
			continue
		}
		read++
		if v, decoded := row(parts[i]); decoded {
			if older(v, sinceMS-tailDisorder) {
				stop = true
				break
			}
			if !older(v, sinceMS) {
				rev = append(rev, v)
			}
		}
		if maxLines > 0 && read >= maxLines {
			stop = true
			break
		}
	}
	return reverse(rev), stop, true
}

// afterNewline is the byte before off being a newline: the window at off starts
// a line.
func afterNewline(f *os.File, off int64) bool {
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off-1); err != nil {
		return false
	}
	return b[0] == '\n'
}

// row decodes one line, the caller's `fromjson?`: anything but exactly one
// value is not a row.
func row(line []byte) (jsonv.Value, bool) {
	vs, err := jsonv.DecodeStream(bytes.NewReader(line))
	if err != nil || len(vs) != 1 {
		return jsonv.Value{}, false
	}
	return vs[0], true
}

// older is a row's ts falling before bound. A bound of 0 or less bounds nothing,
// and a ts that is not a number cannot be shown to fall before one.
func older(v jsonv.Value, sinceMS int64) bool {
	if sinceMS <= 0 {
		return false
	}
	ts, ok := v.Get("ts")
	if !ok {
		return false
	}
	f, ok := ts.AsFloat()
	return ok && f < float64(sinceMS)
}

func reverse(vs []jsonv.Value) []jsonv.Value {
	for i, j := 0, len(vs)-1; i < j; i, j = i+1, j-1 {
		vs[i], vs[j] = vs[j], vs[i]
	}
	return vs
}
