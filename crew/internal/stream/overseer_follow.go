package stream

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	lanepkg "github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

const (
	// defaultFollowPark keeps one follow under the Monitor's 30-minute cap.
	defaultFollowPark = 1680

	daemonStartTries = 100
	daemonStartPoll  = 100 * time.Millisecond

	takeoverName = "follow.takeover"
)

// followLine is the marker a follow prints before any log line.
type followLine struct {
	Stream string `json:"stream"`
	Lane   string `json:"lane"`
	Reason string `json:"reason"`
	PID    int64  `json:"pid"`
	Offset int64  `json:"offset"`
	TS     int64  `json:"ts"`
}

// follow is `--follow`: a reader of the daemon's segment log that resumes at
// the offset its last run saved.
type follow struct {
	fr             FleetRequest
	lane           string
	o              FleetOptions
	spawn          func(args []string, stderrPath string) error
	stdout, stderr io.Writer
	crew           string
	heartbeat      int64
	sup            *suppress
	sigs           <-chan os.Signal
	signalled      bool
	info           daemonInfo
	offset         int64
	stale          int
	upgraded       bool
}

// runFollow is `--follow`. It never stops the daemon it ensures.
func runFollow(fr FleetRequest, c call, lane string, stdout, stderr io.Writer, o FleetOptions) int {
	park := int64(defaultFollowPark)
	if fr.Park != "" {
		park = c.park
	}
	if err := os.MkdirAll(lane, 0o755); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	lockd := filepath.Join(lane, "follow.lock.d")
	if !takeFollow(lockd, o.Options, stderr) {
		return 1
	}
	defer lock.Release(lockd)

	f := &follow{fr: fr, lane: lane, o: o, spawn: o.Spawn, stdout: stdout, stderr: stderr,
		crew: c.crew, heartbeat: c.heartbeat, sup: &suppress{key: map[string]string{}, ts: map[string]int64{}}, sigs: sigs}
	if f.spawn == nil {
		f.spawn = spawnDaemon(o.Self)
	}
	deadline := o.Clock.Now().Add(time.Duration(park) * time.Second)
	force := fr.Force
	if !force && f.state() == "alive" {
		if info, ok := daemonHolds(lane); ok && f.binaryChanged(info) {
			force, f.upgraded = true, true
		}
	}
	spawned, ok := f.ensure(force)
	if !ok {
		return f.failed()
	}
	if !f.start(spawned) {
		return 1
	}
	for {
		if !f.drain() {
			return 1
		}
		select {
		case <-sigs:
			return f.ended()
		case <-o.Rediscover(time.Duration(overseerPoll) * time.Second):
		}
		if !o.Clock.Now().Before(deadline) {
			return 0
		}
		if !f.recheck() {
			return f.failed()
		}
	}
}

// failed is the exit code of a failed ensure, which a signal interrupts.
func (f *follow) failed() int {
	if f.signalled {
		return f.ended()
	}
	return 1
}

// ended is a signalled follow's exit. One TERMed by a newer follow says so,
// so its Monitor reads the end as a handover rather than a death.
func (f *follow) ended() int {
	if !takenOver(f.lane, os.Getpid()) {
		return 0
	}
	say(f.stdout, "%s\n", encode(followLine{Stream: "follow", Lane: "ended", Reason: "taken_over", PID: f.info.PID, Offset: f.offset, TS: clockMS(f.o.Options)}))
	if f.o.Flush != nil {
		_ = f.o.Flush()
	}
	return 0
}

// takenOver is whether the lane's takeover marker names pid.
func takenOver(lane string, pid int) bool {
	b, err := os.ReadFile(filepath.Join(lane, takeoverName))
	return err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(pid)
}

// takeFollow takes follow.lock.d, TERMing a live holder first: a re-armed
// follow always wins over an orphaned older one. The takeover marker names
// the holder before it is TERMed, so it can tell a handover from a stop.
func takeFollow(lockd string, o Options, stderr io.Writer) bool {
	pid := strconv.Itoa(os.Getpid())
	takeover := filepath.Join(filepath.Dir(lockd), takeoverName)
	if lock.Acquire(lockd, pid) {
		dropStaleTakeover(takeover)
		return true
	}
	holder := lock.Holder(lockd)
	if target, ok := signalTarget(holder); ok {
		if err := writeFileAtomic(takeover, []byte(strconv.Itoa(target))); err != nil {
			say(stderr, "crew: fleet stream: follow: %s\n", oneLine(err))
		}
	}
	if valid, gone := reclaim(holder, o); valid && gone && lock.Acquire(lockd, pid) {
		dropStaleTakeover(takeover)
		return true
	}
	say(stderr, "crew: fleet stream: could not take over the overseer follow from pid %s\n", orUnknown(holder))
	return false
}

// dropStaleTakeover removes a takeover marker unless it names this process: a
// concurrent newer follow may have just named this holder before TERMing it.
func dropStaleTakeover(takeover string) {
	if takenOver(filepath.Dir(takeover), os.Getpid()) {
		return
	}
	_ = os.Remove(takeover)
}

// binaryChanged is whether the daemon runs a different binary than this
// follow: a replaced path or file, or a daemon too old to record one.
func (f *follow) binaryChanged(info daemonInfo) bool {
	exe, id := f.o.Binary()
	return info.Exe != exe || info.ExeID != id
}

func (f *follow) infoPath() string { return filepath.Join(f.lane, daemonInfoName) }

func (f *follow) state() string {
	s, _, _, _ := lanepkg.State(f.lane, f.o.Proc, clockMS(f.o.Options))
	return s
}

// ensure leaves f.info naming a live daemon, spawning one unless a live one
// is there and force is unset. spawned reports whether this call started it.
func (f *follow) ensure(force bool) (spawned, ok bool) {
	state := f.state()
	if state == "alive" && !force {
		if info, ok := daemonHolds(f.lane); ok {
			f.info = info
			return false, true
		}
		if state == "alive" {
			say(f.stderr, "crew: fleet stream: the overseer lane is held by a non-daemon lane (pid %s); use --follow --force to replace it\n", orUnknown(lock.Holder(filepath.Join(f.lane, "stream.lock.d"))))
			return false, false
		}
	}
	before, had := readDaemonInfo(f.infoPath())
	errPath := filepath.Join(f.lane, "daemon.err")
	if err := f.spawn(f.daemonArgs(force || state == "stale"), errPath); err != nil {
		say(f.stderr, "crew: fleet stream: spawn the overseer daemon: %s\n", oneLine(err))
		return false, false
	}
	for range daemonStartTries {
		select {
		case <-f.sigs:
			f.signalled = true
			return false, false
		case <-f.o.Timer(daemonStartPoll):
		}
		info, ok := readDaemonInfo(f.infoPath())
		if ok && (!had || info.PID != before.PID || info.StartedMS != before.StartedMS) && f.state() == "alive" {
			f.info = info
			return true, true
		}
	}
	say(f.stderr, "crew: fleet stream: the overseer daemon did not start (see %s)\n", errPath)
	return false, false
}

func (f *follow) daemonArgs(force bool) []string {
	args := []string{"fleet", "stream", "--overseer", "--daemon"}
	if force {
		args = append(args, "--force")
	}
	for _, a := range []struct{ flag, text string }{
		{"--interval", f.fr.Interval},
		{"--rediscover", f.fr.Rediscover},
		{"--heartbeat", f.fr.Heartbeat},
	} {
		if a.text != "" {
			args = append(args, a.flag, a.text)
		}
	}
	return args
}

// start picks the offset to resume from and prints the first marker. Reasons
// rank started > restarted > gap > first > resume.
func (f *follow) start(spawned bool) bool {
	saved, have := readFollowState(filepath.Join(f.lane, followStateName))
	var oldest, end int64
	segs, err := listSegments(filepath.Join(f.lane, logDirName))
	if err != nil {
		say(f.stderr, "crew: fleet stream: follow: %s\n", oneLine(err))
		return false
	}
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		oldest, end = segs[0].start, last.start+last.size
	}
	reason, off := "resume", saved.Offset
	switch {
	case !have:
		reason, off = "first", f.info.StartOffset
	case saved.Offset > end:
		reason, off = "gap", f.info.StartOffset
	}
	if off < oldest {
		off, reason = oldest, "gap"
	}
	if have && saved.StartedMS != f.info.StartedMS {
		reason = "restarted"
	}
	if spawned {
		reason = "started"
		if f.upgraded {
			reason = "upgraded"
		}
	}
	f.offset = off
	return f.mark(reason)
}

// mark prints a marker at f.offset and saves it as the follow's state.
func (f *follow) mark(reason string) bool {
	lane := "fresh"
	if reason == "resume" {
		lane = "resume"
	}
	line := encode(followLine{Stream: "follow", Lane: lane, Reason: reason, PID: f.info.PID, Offset: f.offset, TS: clockMS(f.o.Options)})
	return f.write([]byte(line + "\n"))
}

// write puts p on stdout before saving the offset, so a clean exit never
// repeats or loses a line.
func (f *follow) write(p []byte) bool {
	if _, err := f.stdout.Write(p); err != nil {
		say(f.stderr, "crew: fleet stream: follow: %s\n", oneLine(err))
		return false
	}
	if f.o.Flush != nil {
		if err := f.o.Flush(); err != nil {
			say(f.stderr, "crew: fleet stream: follow: %s\n", oneLine(err))
			return false
		}
	}
	if err := writeFollowState(filepath.Join(f.lane, followStateName), followState{Offset: f.offset, StartedMS: f.info.StartedMS}); err != nil {
		say(f.stderr, "crew: fleet stream: follow: %s\n", oneLine(err))
	}
	return true
}

// drain prints the complete lines after f.offset; lines pruned before they
// were read are announced by a gap marker.
func (f *follow) drain() bool {
	chunk, next, oldest, _, err := readLog(filepath.Join(f.lane, logDirName), f.offset)
	if err != nil {
		detail := "follow: " + oneLine(err)
		ts := clockMS(f.o.Options)
		if f.sup.repeat("readlog", digitsToN(detail), ts, f.heartbeat*1000) {
			say(f.stdout, "%s\n", encode(errorLine{Stream: "error", Crew: f.crew, RC: 1, Detail: detail, TS: ts}))
		}
		return true
	}
	if oldest > f.offset {
		f.offset = oldest
		if !f.mark("gap") {
			return false
		}
	}
	if len(chunk) == 0 {
		return true
	}
	f.offset = next
	return f.write(chunk)
}

// recheck replaces a daemon that died or was replaced under this follow. A
// stale one gets one poll's grace before it is forced.
func (f *follow) recheck() bool {
	state := f.state()
	if state == "stale" {
		f.stale++
	} else {
		f.stale = 0
	}
	info, ok := daemonHolds(f.lane)
	same := ok && info.PID == f.info.PID && info.StartedMS == f.info.StartedMS
	force := f.stale >= 2
	if same && !force && state != "dead" {
		return true
	}
	spawned, ok := f.ensure(force)
	if !ok {
		return false
	}
	f.stale = 0
	if spawned {
		return f.mark("started")
	}
	return f.mark("restarted")
}

// spawnDaemon starts `self fleet stream --overseer --daemon …` in its own
// session, so it outlives the follow and its Monitor.
func spawnDaemon(self string) func(args []string, stderrPath string) error {
	return func(args []string, stderrPath string) error {
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer func() { _ = null.Close() }()
		errf, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = errf.Close() }()
		// The environment is inherited unchanged: the daemon's sender gate reads
		// CREW_OVERSEER and its lane root XDG_STATE_HOME.
		cmd := exec.Command(self, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, errf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		// Reaped here so a daemon dying under this follow stops reading as alive.
		go func() { _ = cmd.Wait() }()
		return nil
	}
}
