package watch

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

const defaultHeartbeat = "30"

// RelayRequest is the typed `crew relay` call. Since nil starts from 0; every
// other "" is the default (watch's states and interval, a 30 second heartbeat,
// the checkout's crew).
type RelayRequest struct {
	Since                             *string
	States, Interval, Heartbeat, Crew string
}

// Relay is the remote half of a relayed dispatch: a lock-free follower of this
// bus that prints watch's own batch line whenever something qualifies past its
// in-memory cursor, and a heartbeat line when nothing has for --heartbeat
// seconds. It takes no watch.lock.d and writes no cursor file, so it never
// fights a `crew watch` on the same crew; the reader that holds the real cursor
// is on the other end of the pipe.
//
// A failed write ends it with 0: that is how a dropped ssh link stops the
// remote process, and there is no one left to tell.
func Relay(req RelayRequest, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = clock.Clock{Now: o.Now}.Sleep
	}
	c, msg := check(Request{
		Since: req.Since, States: req.States, Timeout: "1", Interval: req.Interval, Crew: req.Crew,
	}, o)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return 1
	}
	beatText := orDefault(req.Heartbeat, defaultHeartbeat)
	if !isDigits(beatText) || parseIntSaturate(beatText) == 0 {
		say(stderr, "crew: --heartbeat must be a positive integer number of seconds\n")
		return 1
	}
	beat := time.Duration(parseIntSaturate(beatText)) * time.Second

	realMS := clock.Clock{Now: o.Now}.RealMS
	lastLine := o.Now()
	for {
		var line bytes.Buffer
		if folded, ok := fold(paths.Log, c); ok {
			out := jsonval.Clamp(folded).(map[string]any)
			if err := jsonval.Encode(&line, out, false); err != nil {
				say(stderr, "crew: %v\n", err)
				return 1
			}
			c.since = parseIntSaturate(cursorText(out))
		} else if o.Now().Sub(lastLine) >= beat {
			b, _ := json.Marshal(struct {
				Relay string `json:"relay"`
				TS    int64  `json:"ts"`
			}{"heartbeat", realMS()})
			line.Write(b)
			line.WriteByte('\n')
		}
		if line.Len() > 0 {
			if _, err := stdout.Write(line.Bytes()); err != nil {
				return 0
			}
			if o.Flush != nil && o.Flush() != nil {
				return 0
			}
			lastLine = o.Now()
		}
		if err := o.Sleep(c.interval, stderr); err != nil {
			return exitStatus(err)
		}
	}
}
