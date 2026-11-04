// Package lane reads a stream lane's liveness and names its scratch paths. It
// is a leaf: `stream` writes a lane's lock and tick, and `stream` and `fleet`
// both read them back through here.
package lane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/proc"
)

// State is the state machine behind `--status` for the lane whose lock and
// tick live in cdir: the state, its exit status, the holder pid and the tick's
// age. nowMS is the wall clock the tick is measured against.
//
// A live pid is not evidence notifications are flowing (a rate-limit
// auto-stop, a closed stdout and pid reuse all leave a pid that looks alive),
// so `alive` also needs a tick younger than 2×park+60 — computed from the park
// THE TICK ITSELF recorded, so a stale `--park` on the status call cannot
// shift the answer. A lock with no readable tick counts as `stale`, the safe
// direction: the tick is written immediately on acquiring the lock.
func State(cdir string, p proc.Table, nowMS int64) (state string, rc int, pid json.RawMessage, age *int64) {
	holder := statusPID(lock.Holder(filepath.Join(cdir, "stream.lock.d")))
	live := lock.ValidHolder(holder) && p.Alive(pidNumber(holder))

	// The holder is reported whether or not it answers: `dead` still names the
	// pid that is gone.
	pid = holderPID(holder)
	state, rc = "dead", 2
	if live {
		state, rc = "stale", 1
		if tick := ReadTick(filepath.Join(cdir, "stream.tick")); tick.OK {
			ageS := (nowMS - tick.TS) / 1000
			age = &ageS
			if ageS < 2*tick.Park+60 {
				state, rc = "alive", 0
			}
		}
	}
	return state, rc, pid, age
}

// holderPID is the pid `--status` reports for a holder's digits: those digits
// with the leading zeros jq drops (`00` reads 0, `007` reads 7), and nil for no
// holder.
func holderPID(holder string) json.RawMessage {
	if holder == "" {
		return nil
	}
	text := strings.TrimLeft(holder, "0")
	if text == "" {
		text = "0"
	}
	return json.RawMessage(text)
}

// statusPID is the arm's `case "$lockpid" in ” | *[!0-9]* | 0)`: empty,
// non-numeric and a literal 0 are all no holder at all.
func statusPID(text string) string {
	if !ascii.IsDigits(text) || text == "0" {
		return ""
	}
	return text
}

// pidNumber is the arm's arithmetic context: the digits as an int. Every caller
// is behind lock.ValidHolder, so an unparsable or out-of-range holder never
// reaches a probe; -1 keeps that true if a caller forgets.
func pidNumber(text string) int {
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || n > lock.MaxPID {
		return -1
	}
	return n
}

// Tick is a stream.tick: the two numbers the writer recorded, and OK for
// whether the file read as both of them.
type Tick struct {
	TS, Park int64
	OK       bool
}

// ReadTick is the arm's `jq -r '.ts // empty'` plus its digits-only `case`: a
// missing file, one that will not decode, and a non-numeric field all read the
// same — no tick.
func ReadTick(path string) Tick {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tick{}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return Tick{}
	}
	ts, okTS := digitsField(obj, "ts")
	park, okPark := digitsField(obj, "park")
	if !okTS || !okPark {
		return Tick{}
	}
	return Tick{TS: ts, Park: park, OK: true}
}

// digitsField is a field read as the arm's digits-only `case` did: a number's
// own text or a string, and false for anything that is not digits.
func digitsField(obj map[string]json.RawMessage, key string) (int64, bool) {
	text := strings.TrimSpace(string(obj[key]))
	if strings.HasPrefix(text, `"`) {
		var s string
		if json.Unmarshal([]byte(text), &s) != nil {
			return 0, false
		}
		text = s
	}
	if !ascii.IsDigits(text) {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil
}

// BusScratch is a bus's own dir under a lane: sha256(common)[:8], hex-encoded,
// 16 characters. Every scratch file a bus loop writes lives here.
func BusScratch(lane, common string) string {
	sum := sha256.Sum256([]byte(common))
	return filepath.Join(lane, "buses", hex.EncodeToString(sum[:8]))
}

// OverseerCursor is the overseer's msg cursor for one bus, kept outside the
// bus: crews/<crew>/cursor is the dispatcher's own, and advancing it would
// steal that dispatcher's wakes. stateDir is ${XDG_STATE_HOME:-$HOME/.local/state}.
func OverseerCursor(stateDir, common string) string {
	return filepath.Join(BusScratch(filepath.Join(stateDir, "crew", "overseer"), common), "cursor")
}

// OverseerHostDir is the overseer's per-partner state dir: its cursor and ok
// files for host.
func OverseerHostDir(stateDir, host string) string {
	return filepath.Join(stateDir, "crew", "overseer", "hosts", host)
}
