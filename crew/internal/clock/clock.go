// Package clock is the $CREW_CLOCK pair the bash helpers share: `_clock_now`'s
// seconds, `_clock_now_f`'s fraction, `_clock_now_ms`'s stamp and
// `_clock_sleep`'s advance-instead-of-waiting. `crew hold` carried the first two
// privately (#883); `crew await` needs all four, so they live here once.
//
// CREW_CLOCK is test-only: a file of epoch seconds that the waits advance
// instead of sleeping, so a suite of ten-minute waits runs in seconds. It starts
// at the real time and only moves forward, so bus rows (real ms) posted during a
// run still sort at or before `now`. Unset, these are exactly `date +%s`, jq's
// `now`, jq's `now*1000|floor` and `sleep`.
package clock

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// Clock is the two things the helpers read: the real time source (required;
// injected so tests can freeze it) and $CREW_CLOCK, empty when unset.
type Clock struct {
	Now       func() time.Time
	CrewClock string
}

// Text is `_clock_now`: the seconds in the clock file, seeded from the real
// clock when the file is missing or empty (the helper's `[ -s ] || date +%s >`),
// and the real clock itself with no CREW_CLOCK.
func (c Clock) Text() string {
	real := strconv.FormatInt(c.Now().Unix(), 10)
	if c.CrewClock == "" {
		return real
	}
	data, err := os.ReadFile(c.CrewClock)
	if err != nil || len(data) == 0 {
		_ = os.WriteFile(c.CrewClock, []byte(real+"\n"), 0o644)
		return real
	}
	return strings.TrimRight(string(data), "\n")
}

// Seconds is `_clock_now` as a number. A file holding text bash's arithmetic
// would reject reads as 0, which is how its caller reads a clock it cannot use.
func (c Clock) Seconds() int64 {
	n, _ := strconv.ParseInt(c.Text(), 10, 64)
	return n
}

// realMS is jq's `now*1000` before the floor: the same microsecond-precision
// double the arm's `jq -nc 'now*1000|floor'` computed from.
func (c Clock) realMS() float64 {
	now := c.Now()
	return float64(now.Unix())*1000 + float64(now.Nanosecond()/1000)/1000
}

// RealMS is jq's `now*1000|floor` — the real clock, which is what bus rows stamp
// even under CREW_CLOCK.
func (c Clock) RealMS() int64 {
	return int64(math.Floor(c.realMS()))
}

// NowF is `_clock_now_f`: the virtual clock's seconds when it is set, jq's `now`
// otherwise. The arm handed either to `--argjson`, so a computed double is what
// jq compared against either way, and the un-virtual one keeps the sub-second
// precision a hold maturing inside the current second depends on.
func (c Clock) NowF() jsonv.Value {
	if c.CrewClock == "" {
		return jsonv.Num(c.realMS() / 1000)
	}
	n, _ := strconv.ParseFloat(c.Text(), 64)
	return jsonv.Num(n)
}

// NowMS is `_clock_now_ms`: the virtual clock's whole seconds times 1000 under
// CREW_CLOCK, jq's `now*1000|floor` otherwise.
func (c Clock) NowMS() int64 {
	if c.CrewClock == "" {
		return c.RealMS()
	}
	return c.Seconds() * 1000
}

// virtualPollFloor is how long a virtual-clock poll still lasts in real time.
// The arm's poll cost a `jq` process start — about 8ms — which is what kept a
// CREW_CLOCK wait from reading the whole bus at full CPU. Go's fold is in
// process, so an unpaced loop burns a 300s virtual timeout in a fifth of a
// second and hammers the log 20x harder than the arm it replaces. The floor
// keeps the arm's poll rate, and therefore its bus-load profile; the clock
// advance and the poll count are untouched.
const virtualPollFloor = 10 * time.Millisecond

// Sleep is `_clock_sleep`. With no CREW_CLOCK it is the real `sleep`, same argv:
// GNU sleep owns the accepted grammar (`1.5`, `2m`, `1d`, combinations), and on a
// value it rejects the child's own message and status 1 are what killed the arm
// under `set -e`.
//
// Under CREW_CLOCK nothing sleeps for the interval: the file moves forward by
// the interval's integer part, a fraction rounded up (bash's `${1%%.*}` plus 1),
// through a pid-suffixed tmp file and a rename — the helper's
// `> "$CREW_CLOCK.$$"; mv -f`, so two polling callers cannot interleave a
// half-written clock. A value bash would evaluate as an unset variable (`abc`)
// advances 0, matching `$((now+abc))`; bash's other arithmetic forms are not
// mirrored — `010` advances 10 where bash adds 8, and a token Go cannot read
// (`0x10`) advances 0, a duration flag never being a hex literal.
func (c Clock) Sleep(interval string, stderr io.Writer) error {
	if c.CrewClock == "" {
		cmd := exec.Command("sleep", interval)
		cmd.Stderr = stderr
		return cmd.Run()
	}
	s := interval
	if i := strings.IndexByte(interval, '.'); i >= 0 {
		s = strconv.FormatInt(arith(interval[:i])+1, 10)
	}
	out := strconv.FormatInt(c.Seconds()+arith(s), 10)
	tmp := fmt.Sprintf("%s.%d", c.CrewClock, os.Getpid())
	if err := os.WriteFile(tmp, []byte(out+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.CrewClock); err != nil {
		return err
	}
	time.Sleep(virtualPollFloor)
	return nil
}

// arith is what bash's `$(( ))` makes of the interval token: the decimal integer
// it holds, or 0 for anything it reads as an unset variable.
func arith(tok string) int64 {
	n, err := strconv.ParseInt(tok, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
