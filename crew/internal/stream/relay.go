// Relay sources: for each host a remote:true dispatch row on the loop's bus
// names, one `crew relay` on that host, reached through `crew remote-run`, whose
// batches the loop forwards tagged with the host.
//
// Relayed events exist only as stream lines. Nothing here appends to the local
// bus, so a remote row satisfies no local gate (waiver, sender, claim): the relay
// parses each line as JSON and only adds fields to it.
package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

const (
	// defaultRelayHeartbeat is the remote `crew relay`'s own default --heartbeat.
	defaultRelayHeartbeat = 30 * time.Second
	// watchdogBeats missed heartbeats read as a lost link.
	watchdogBeats = 3
	// relayDrain bounds the wait for a killed relay's pipes: a descendant that
	// left the process group could otherwise hold them open forever.
	relayDrain   = time.Second
	maxRelayLine = 16 << 20
)

// relayTiming is the relays' clock, in Options so a test can shrink it. Zero
// fields take the defaults: the remote's heartbeat, the stream's --retry and
// the stream's reap cadence (which a non-zero reapEvery replaces only while
// that cadence is on).
type relayTiming struct {
	heartbeat, retry, reapEvery time.Duration
}

// relays is a loop's relay sources. hosts and the log's last size and mtime
// are touched only by sync, which after the first call runs on start's own
// goroutine; each relay runs in its own goroutine and writes only through out,
// which is the loop's shared, line-locked writer. wg counts the sync goroutine
// as well as the relays, so every relay's Add happens while it is non-zero.
type relays struct {
	out          io.Writer
	procs        children
	log, cdir    string
	crew, states string
	repo         string
	owner        string
	retry        time.Duration
	heartbeat    time.Duration
	reapEvery    time.Duration
	heartbeatMS  int64
	now          func() int64

	hosts map[string]*relay
	stop  chan struct{}
	wg    sync.WaitGroup

	size   int64
	mtime  time.Time
	missed bool // a host the last sync could not start, retried whatever the log does
}

// newRelays is l's relay sources over the bus log, keeping their locks and
// cursors under cdir/relay. A fleet loop's c.reapEvery is forced to 0, yet the
// remote host has no other reaper, so it reaps on the default cadence instead.
func newRelays(l *loop, log, cdir string) *relays {
	t := l.o.relay
	reapEvery := time.Duration(l.c.reapEvery) * time.Second
	if l.repo != "" {
		reapEvery = defaultReapEvery * time.Second
	}
	if reapEvery != 0 && t.reapEvery != 0 {
		reapEvery = t.reapEvery
	}
	retry := time.Duration(l.c.retry) * time.Second
	if t.retry != 0 {
		retry = t.retry
	}
	heartbeat := defaultRelayHeartbeat
	if t.heartbeat != 0 {
		heartbeat = t.heartbeat
	}
	return &relays{
		out:         l.stdout,
		procs:       l.procs,
		log:         log,
		cdir:        cdir,
		crew:        l.c.crew,
		states:      l.c.statesText,
		repo:        l.repo,
		owner:       strconv.Itoa(os.Getpid()),
		retry:       retry,
		heartbeat:   heartbeat,
		reapEvery:   reapEvery,
		heartbeatMS: l.heartbeatMS(),
		now:         l.nowMS,
		hosts:       map[string]*relay{},
		stop:        make(chan struct{}),
	}
}

// start syncs now and then every interval until shutdown, on its own
// goroutine: the loop spends most of its time parked, and a new remote host
// must not wait out the park for its relay.
func (rs *relays) start(interval time.Duration) {
	rs.sync()
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-rs.stop:
				return
			case <-t.C:
				rs.sync()
			}
		}
	}()
}

// sync starts a relay for each remote host on the bus that has none yet. The
// bus is append-only, so a log whose size and mtime have not moved is not
// read again, unless a host whose lock another loop held is waiting for a retry.
func (rs *relays) sync() {
	st, err := os.Stat(rs.log)
	if err != nil || !rs.missed && st.Size() == rs.size && st.ModTime().Equal(rs.mtime) {
		return
	}
	rs.size, rs.mtime, rs.missed = st.Size(), st.ModTime(), false
	events, _ := bus.ReadEventsTolerant(rs.log)
	for _, h := range remote.RemoteHosts(events, rs.crew) {
		if rs.hosts[h] != nil || !remote.HostNameRe.MatchString(h) {
			continue
		}
		dir := filepath.Join(rs.cdir, "relay")
		if os.MkdirAll(dir, 0o755) != nil {
			rs.missed = true
			continue
		}
		lockd := filepath.Join(dir, h+".lock.d")
		if !lock.Acquire(lockd, rs.owner) {
			rs.missed = true
			continue
		}
		r := &relay{
			rs:      rs,
			host:    h,
			lockd:   lockd,
			cursor:  filepath.Join(dir, h+".cursor"),
			sup:     &suppress{key: map[string]string{}, ts: map[string]int64{}},
			reapSup: &suppress{key: map[string]string{}, ts: map[string]int64{}},
		}
		rs.hosts[h] = r
		rs.wg.Add(1)
		go r.run()
	}
}

// shutdown stops the syncs, kills every relay child's process group and waits
// for each relay to release its lock. A remote reap in flight is left to
// finish, as a local one is.
func (rs *relays) shutdown() {
	close(rs.stop)
	rs.wg.Wait()
}

// command is a child in its own process group, so killing the group also takes
// the ssh under remote-run, and the lane's own group signals never reach it.
func (rs *relays) command(args []string) *exec.Cmd {
	cmd := rs.procs.command(args)
	if rs.procs.dir != "" {
		cmd.Dir = rs.procs.dir
		cmd.Env = withoutGitDirs(os.Environ())
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func (rs *relays) remoteArgs(host string, argv ...string) []string {
	return append([]string{"remote-run", "--host", host, "--env", "CREW_ID=" + rs.crew, "--", "crew"}, argv...)
}

// relay is one host's source. sup is its own goroutine's; reapSup belongs to
// the one reap allowed in flight.
type relay struct {
	rs      *relays
	host    string
	lockd   string
	cursor  string
	sup     *suppress
	reapSup *suppress
	reaping atomic.Bool
}

func (r *relay) run() {
	defer r.rs.wg.Done()
	defer lock.Release(r.lockd)
	var reapTick <-chan time.Time
	if r.rs.reapEvery > 0 {
		t := time.NewTicker(r.rs.reapEvery)
		defer t.Stop()
		reapTick = t.C
	}
	for {
		rc, reason, stopped := r.connect(reapTick)
		if stopped {
			return
		}
		r.report("lost", int64(rc), "relay to "+r.host+" lost: "+reason)
		retry := time.NewTimer(r.rs.retry)
		select {
		case <-r.rs.stop:
			retry.Stop()
			return
		case <-retry.C:
		}
	}
}

// connect runs one relay child from the stored cursor until it exits, falls
// silent for watchdogBeats heartbeats, or the loop stops. It returns the
// child's status, why the link went, and whether the loop stopped it.
func (r *relay) connect(reapTick <-chan time.Time) (rc int, reason string, stopped bool) {
	cmd := r.rs.command(r.rs.remoteArgs(r.host, "relay", "--crew", r.rs.crew,
		"--since", r.readCursor(), "--states", r.rs.states))
	outR, outW, err := os.Pipe()
	if err != nil {
		return exitCantExec, err.Error(), false
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return exitCantExec, err.Error(), false
	}
	defer func() {
		_ = outR.Close()
		_ = errR.Close()
	}()
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		return exitCantExec, err.Error(), false
	}
	pgid := cmd.Process.Pid

	lines := make(chan string)
	go scanLines(outR, lines)
	stderr := make(chan string, 1)
	go firstStderrLine(errR, stderr)
	exited := make(chan int, 1)
	go func() { exited <- waitStatus(cmd.Wait()) }()

	silence := watchdogBeats * r.rs.heartbeat
	watchdog := time.NewTimer(silence)
	defer watchdog.Stop()
	dog, stop := watchdog.C, r.rs.stop
	var drain <-chan time.Time
	killed := false
	kill := func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if drain == nil {
			drain = time.After(relayDrain)
		}
	}
	for lines != nil || exited != nil {
		select {
		case line, ok := <-lines:
			if !ok {
				// Stdout gone is the link gone, whatever the child is still doing.
				lines = nil
				kill()
				continue
			}
			watchdog.Reset(silence)
			r.forward(line)
		case rc = <-exited:
			exited = nil
			// Sweeps whatever the child left in its group.
			kill()
		case <-dog:
			dog, killed = nil, true
			kill()
		case <-stop:
			stop, stopped = nil, true
			kill()
		case <-reapTick:
			r.reap()
		case <-drain:
			_ = outR.Close()
		}
	}
	if stopped {
		return rc, "", true
	}
	select {
	case reason = <-stderr:
	case <-time.After(relayDrain):
		_ = errR.Close()
		reason = <-stderr
	}
	switch {
	case killed:
		reason = "no line for " + silence.String()
	case reason == "":
		reason = fmt.Sprintf("exit %d", rc)
	}
	return rc, reason, false
}

func scanLines(r io.ReadCloser, lines chan<- string) {
	defer close(lines)
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxRelayLine)
	for sc.Scan() {
		lines <- sc.Text()
	}
}

// firstStderrLine reads the child's stderr to its end and keeps the first line,
// which is ssh's or remote-run's reason.
func firstStderrLine(r io.ReadCloser, first chan<- string) {
	sc := bufio.NewScanner(r)
	line := ""
	if sc.Scan() {
		line = sc.Text()
	}
	_, _ = io.Copy(io.Discard, r)
	first <- line
}

// forward passes one relayed batch through, tagged with the host (and the repo
// in a fleet loop), and only then stores its cursor. A heartbeat only fed the
// watchdog; a line that does not decode is reported and the cursor kept.
func (r *relay) forward(text string) {
	obj, err := decodeObject(text)
	if err == nil && fieldText(obj, "relay") == "heartbeat" {
		return
	}
	line, cursor, ok := r.label(text, obj, err)
	if !ok {
		r.report("line", 1, "relay from "+r.host+": undecodable line")
		return
	}
	if _, err := fmt.Fprintf(r.rs.out, "%s\n", line); err != nil {
		return
	}
	r.writeCursor(cursor)
	if wantsReap(text) {
		r.reap()
	}
}

// label is the batch with host on it and on each event. A "stream" key would
// make it read as one of the stream's own lines, so it is dropped, and repo is
// the loop's own label or nothing.
func (r *relay) label(text string, obj map[string]json.RawMessage, err error) (line, cursor string, ok bool) {
	if err != nil {
		return "", "", false
	}
	var b struct {
		Cursor json.Number                  `json:"cursor"`
		Events []map[string]json.RawMessage `json:"events"`
	}
	if json.Unmarshal([]byte(text), &b) != nil || !ascii.IsDigits(b.Cursor.String()) || b.Events == nil {
		return "", "", false
	}
	host := jsonString(r.host)
	for _, e := range b.Events {
		if e == nil {
			return "", "", false
		}
		e["host"] = host
	}
	obj["events"] = json.RawMessage(encode(b.Events))
	obj["host"] = host
	delete(obj, "stream")
	delete(obj, "repo")
	if r.rs.repo != "" {
		obj["repo"] = jsonString(r.rs.repo)
	}
	return encode(obj), b.Cursor.String(), true
}

// report is an error line naming the host, one per cause per --heartbeat.
func (r *relay) report(kind string, rc int64, detail string) {
	r.reportWith(r.sup, kind, rc, detail)
}

func (r *relay) reportWith(sup *suppress, kind string, rc int64, detail string) {
	ts := r.rs.now()
	if !sup.repeat(kind, fmt.Sprintf("%d:%s", rc, digitsToN(detail)), ts, r.rs.heartbeatMS) {
		return
	}
	say(r.rs.out, "%s\n", encode(errorLine{
		Stream: "error",
		Crew:   r.rs.crew,
		Host:   r.host,
		RC:     rc,
		Detail: detail,
		TS:     ts,
		Repo:   r.rs.repo,
	}))
}

// reap runs the remote's own unmodified `crew reap` in the background, one at a
// time per host. Its group is its own, so the stream's exit never interrupts a
// removal on the remote.
func (r *relay) reap() {
	if r.rs.reapEvery == 0 || !r.reaping.CompareAndSwap(false, true) {
		return
	}
	cmd := r.rs.command(r.rs.remoteArgs(r.host, "reap", "--quiet"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = relayDrain
	go func() {
		defer r.reaping.Store(false)
		rc := waitStatus(cmd.Run())
		ts := r.rs.now()
		if text := strings.TrimRight(out.String(), "\n"); text != "" {
			say(r.rs.out, "%s\n", encode(reapLine{
				Stream: "reap",
				Crew:   r.rs.crew,
				Host:   r.host,
				Lines:  reapLines(text),
				TS:     ts,
			}))
		}
		if rc != 0 {
			first, _, _ := strings.Cut(errb.String(), "\n")
			r.reportWith(r.reapSup, "reap", int64(rc), "reap: "+first)
		}
	}()
}

func (r *relay) readCursor() string {
	if text := strings.TrimSpace(readFile(r.cursor)); ascii.IsDigits(text) {
		return text
	}
	return "0"
}

// writeCursor replaces the cursor file whole, so a crash never leaves a torn one.
func (r *relay) writeCursor(cursor string) {
	f, err := os.CreateTemp(filepath.Dir(r.cursor), filepath.Base(r.cursor)+".")
	if err != nil {
		return
	}
	_, werr := f.WriteString(cursor + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), r.cursor) != nil {
		_ = os.Remove(f.Name())
	}
}
