package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// Transport sends one remote-exec request to dest and returns its stdout.
type Transport func(ctx context.Context, dest string, request []byte) ([]byte, error)

// ServeOptions configures Serve. An empty Self is SelfName(); a nil Now is
// time.Now.
type ServeOptions struct {
	Paths bus.Paths
	Self  string
	Hosts func() (map[string]remote.Host, error)
	Now   func() time.Time
}

// SyncOptions configures one Sync with the peer host Name, reached at Target.
// An empty Self is SelfName(); a nil Now is time.Now.
type SyncOptions struct {
	Paths     bus.Paths
	Self      string
	Name      string
	Target    remote.Target
	Slug      string
	Transport Transport
	Now       func() time.Time
}

// Result is one Sync: the peer's rows stored here, the peer's rows dropped
// here, and our rows the peer dropped, each count keyed by reason.
type Result struct {
	Stored        int
	Dropped       map[string]int
	RemoteDropped map[string]int
}

// errMalformed marks a payload Serve refuses with exit 2.
type errMalformed struct{ error }

// errBusy is another local exchange with the same host holding its lock.
var errBusy = errors.New("busy")

// Serve is `crew peer-sync`, the P side of one exchange: it stores the
// requester's rows and answers with its own. It exits 0 on success, 1 on a
// refusal and 2 on a malformed payload.
func Serve(stdin io.Reader, stdout, stderr io.Writer, o ServeOptions) int {
	resp, err := serve(stdin, o)
	if err == nil {
		var line []byte
		if line, err = encode(resp); err == nil {
			_, err = stdout.Write(append(line, '\n'))
		}
	}
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "crew: peer-sync: %v\n", err)
	if errors.As(err, new(errMalformed)) {
		return 2
	}
	return 1
}

func serve(stdin io.Reader, o ServeOptions) (Response, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, MaxPayload+1))
	if err != nil {
		return Response{}, err
	}
	if len(data) > MaxPayload {
		return Response{}, errMalformed{errors.New("payload over 1 MiB")}
	}
	var req Request
	if err := decodeOne(data, &req); err != nil {
		return Response{}, errMalformed{err}
	}
	self, err := selfName(o.Self)
	if err != nil {
		return Response{}, err
	}
	hosts, err := o.Hosts()
	if err != nil {
		return Response{}, err
	}
	if h, ok := hosts[req.Host]; !ok || !h.Peer {
		return Response{}, fmt.Errorf("peer %q is not a peer host in this host's settings", req.Host)
	}
	store := StoreFor(o.Paths)
	id, err := store.BusID()
	if err != nil {
		return Response{}, err
	}
	if req.Expect != "" && req.Expect != id {
		return Response{}, fmt.Errorf("identity: peer %s has pinned another id for this host (remove peers/%s.state there to re-pin)", req.Host, self)
	}
	now := nowFunc(o.Now)()
	// A full parse of our own log, kept out of the host lock; its error is
	// reported where the export's would be.
	snap, snapErr := TakeSnapshot(o.Paths.Log, now)
	unlock, ok := store.Lock(req.Host)
	if !ok {
		return Response{}, errBusy
	}
	defer unlock()
	st, err := store.State(req.Host)
	if err != nil {
		return Response{}, err
	}
	if err := pin(&st, req.Host, req.ID); err != nil {
		return Response{}, err
	}
	if err := takeEpoch(store, req.Host, &st, req.Epoch, now); err != nil {
		return Response{}, err
	}
	kept, dropped, seen := admitRows(req.Rows, st.HWM, req.Cursor, self, now)
	if err := store.Append(req.Host, kept, now); err != nil {
		return Response{}, err
	}
	if err := takeRoster(store, req.Host, req.Roster, dropped, now); err != nil {
		return Response{}, err
	}
	st.HWM = max(st.HWM, seen)
	st.Scan = max(st.Scan, req.Cursor)
	// The requester has stored our rows through Since, so a later dial from
	// this side need not resend them, unless Since or Ack predate our log's
	// epoch. An unreadable epoch is unknown: the state write below must not
	// wait on it.
	own, _ := LogEpoch(o.Paths.Log)
	if changed(req.SinceEpoch, own) || changed(st.AckEpoch, own) {
		st.Ack = 0
	} else {
		st.Ack = max(st.Ack, req.Since)
	}
	if own != "" {
		st.AckEpoch = own
	}
	// Written before the export, so a failed export cannot get the stored rows
	// stored again.
	if err := store.WriteState(req.Host, st); err != nil {
		return Response{}, err
	}
	rows, cursor, more, epoch, err := Export(o.Paths.Log, req.Since, req.SinceEpoch, req.Host, now)
	if err != nil {
		return Response{}, err
	}
	if snapErr != nil {
		return Response{}, snapErr
	}
	// An inbound exchange proves the link as well as an outbound one.
	if err := store.writeLink(req.Host, true, "", now); err != nil {
		return Response{}, err
	}
	return Response{
		V: 1, Host: self, ID: id, Ack: max(st.HWM, st.Scan), AckEpoch: st.Epoch, Epoch: epoch,
		Cursor: cursor, More: more, Rows: rows, Dropped: dropped, Roster: &snap,
	}, nil
}

// Sync is the L side of one exchange with the peer host o.Name: it pushes our
// rows after the peer's ack and stores the peer's rows after our hwm. The
// host's lock is held only around local reads and writes, never across the
// Transport, so two hosts dialing each other cannot deadlock. Every outcome
// but busy is recorded in `<host>.link`.
func Sync(ctx context.Context, o SyncOptions) (Result, error) {
	now := nowFunc(o.Now)()
	res, err := exchange(ctx, o, now)
	if err == nil || errors.Is(err, errBusy) {
		return res, err
	}
	store := StoreFor(o.Paths)
	if unlock, ok := store.Lock(o.Name); ok {
		// Best effort: the sync error is what the caller must see.
		_ = store.writeLink(o.Name, false, err.Error(), now)
		unlock()
	}
	return res, err
}

func exchange(ctx context.Context, o SyncOptions, now time.Time) (Result, error) {
	self, err := selfName(o.Self)
	if err != nil {
		return Result{}, err
	}
	store := StoreFor(o.Paths)
	id, err := store.BusID()
	if err != nil {
		return Result{}, err
	}
	st, err := lockedState(store, o.Name)
	if err != nil {
		return Result{}, err
	}
	rows, cursor, _, epoch, err := Export(o.Paths.Log, st.Ack, st.AckEpoch, o.Name, now)
	if err != nil {
		return Result{}, err
	}
	snap, err := TakeSnapshot(o.Paths.Log, now)
	if err != nil {
		return Result{}, err
	}
	req, err := buildRequest(Request{
		V: 1, Host: self, ID: id, Expect: st.ID, Since: max(st.HWM, st.Scan), SinceEpoch: st.Epoch, Epoch: epoch,
		Cursor: cursor, Rows: rows, Roster: &snap,
	}, o)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, SyncTimeout)
	defer cancel()
	out, err := o.Transport(ctx, o.Target.SSH, req)
	if err != nil {
		var ex SSHExit
		if !errors.As(err, &ex) {
			return Result{}, err
		}
		// The error line points at this file, so terminal escapes and bidi text
		// in it would fire when it is read.
		clean := invisible.Drop(stripControl(string(ex.Stderr)))
		if err := store.replace(store.path(o.Name, ".log"), []byte(clean)); err != nil {
			return Result{}, fmt.Errorf("ssh: exit %d (peer stderr not saved: %v)", ex.Code, err)
		}
		return Result{}, fmt.Errorf("ssh: exit %d (peer stderr in peers/%s.log)", ex.Code, o.Name)
	}
	if len(out) > MaxResponse {
		return Result{}, errors.New("response over 1 MiB")
	}
	var resp Response
	if err := decodeOne(out, &resp); err != nil {
		return Result{}, fmt.Errorf("response: %w", err)
	}
	// The peer's host text is not echoed: it would reach the stream's error line.
	if resp.Host != o.Name {
		return Result{}, errors.New("answered as another host")
	}

	unlock, ok := store.Lock(o.Name)
	if !ok {
		return Result{}, errBusy
	}
	defer unlock()
	// Re-read: a Serve for this host may have pinned it and stored its rows
	// while the request was out.
	st, err = store.State(o.Name)
	if err != nil {
		return Result{}, err
	}
	if err := pin(&st, o.Name, resp.ID); err != nil {
		return Result{}, err
	}
	if err := takeEpoch(store, o.Name, &st, resp.Epoch, now); err != nil {
		return Result{}, err
	}
	kept, dropped, seen := admitRows(resp.Rows, st.HWM, resp.Cursor, self, now)
	if err := store.Append(o.Name, kept, now); err != nil {
		return Result{}, err
	}
	if err := takeRoster(store, o.Name, resp.Roster, dropped, now); err != nil {
		return Result{}, err
	}
	st.HWM = max(st.HWM, seen)
	st.Scan = max(st.Scan, resp.Cursor)
	st.Ack = resp.Ack
	st.AckEpoch = resp.AckEpoch
	if err := store.WriteState(o.Name, st); err != nil {
		return Result{}, err
	}
	if err := store.writeLink(o.Name, true, "", now); err != nil {
		return Result{}, err
	}
	return Result{Stored: len(kept), Dropped: dropped, RemoteDropped: knownReasons(resp.Dropped)}, nil
}

// knownReasons keeps the peer's drop counts under the reasons Admit gives and
// folds every other key, which is peer text, into "other".
func knownReasons(counts map[string]int) map[string]int {
	out := map[string]int{}
	for r, n := range counts {
		switch r {
		case reasonMalformed, reasonOversize, reasonWorker, reasonIdentity, reasonControl, reasonStale:
		default:
			r = "other"
		}
		out[r] += n
	}
	return out
}

func lockedState(store Store, host string) (State, error) {
	unlock, ok := store.Lock(host)
	if !ok {
		return State{}, errBusy
	}
	defer unlock()
	return store.State(host)
}

// maxRequest is remote.MaxRequest, shortened by tests: at the real sizes a
// MaxBatch batch, even doubled by escaping, never reaches it.
var maxRequest = remote.MaxRequest

// buildRequest wraps p in the remote-exec request, halving p's rows until the
// encoded request fits maxRequest: escaping inside the stdin string grows the
// batch, and an unsendable batch would wedge the push for good.
func buildRequest(p Request, o SyncOptions) ([]byte, error) {
	for {
		payload, err := encode(p)
		if err != nil {
			return nil, err
		}
		stdin := string(payload)
		req, err := encode(remote.Request{V: 1, Repo: o.Target.Repo, Slug: o.Slug, Argv: []string{"crew", "peer-sync"}, Env: map[string]string{}, Stdin: &stdin})
		if err != nil {
			return nil, err
		}
		if len(req) <= maxRequest {
			return req, nil
		}
		if len(p.Rows) < 2 {
			return nil, errors.New("request over the remote-exec cap")
		}
		p.Rows = p.Rows[:len(p.Rows)/2]
		p.Cursor = p.Rows[len(p.Rows)-1].Seq
	}
}

// pin pins id for host on first contact (TOFU) and refuses a different one.
func pin(st *State, host, id string) error {
	if st.ID == "" {
		st.ID = id
		return nil
	}
	if st.ID != id {
		return fmt.Errorf("identity: peer %s id changed (remove peers/%s.state to re-pin)", host, host)
	}
	return nil
}

// takeEpoch records host's log epoch theirs in st. A change means host's log
// was reset, so the old seqs mean nothing: HWM and Scan restart before this
// exchange's rows, which the peer sent from 0 on seeing our stale epoch, are
// admitted, and the reset is marked for the stream to report.
func takeEpoch(store Store, host string, st *State, theirs string, now time.Time) error {
	if changed(st.Epoch, theirs) {
		st.HWM, st.Scan = 0, 0
		if err := store.markReset(host, now); err != nil {
			return err
		}
	}
	if theirs != "" {
		st.Epoch = theirs
	}
	return nil
}

// takeRoster admits the peer's snapshot raw, counts its drops into dropped and
// replaces host's roster with what passed. A peer that predates rosters sends
// none, which leaves the stored roster alone.
func takeRoster(store Store, host string, raw *Snapshot, dropped map[string]int, now time.Time) error {
	if raw == nil {
		return nil
	}
	rows, d := AdmitSnapshot(raw, now)
	for r, n := range d {
		dropped[r] += n
	}
	return store.writeRoster(host, rows, now)
}

// admitRows is the rows past hwm and through cursor that pass Admit, the drop
// counts of the rest, and the highest seq it processed. Seqs must rise, so a
// repeated seq is never stored twice.
func admitRows(es []Entry, hwm, cursor int64, self string, now time.Time) ([]Entry, map[string]int, int64) {
	var kept []Entry
	dropped := map[string]int{}
	last := hwm
	for _, e := range es {
		if e.Seq <= last || e.Seq > cursor {
			continue
		}
		last = e.Seq
		row, reason := Admit(e, self, now)
		if reason != "" {
			dropped[reason]++
			continue
		}
		kept = append(kept, Entry{Seq: e.Seq, Row: row})
	}
	return kept, dropped, last
}

// wire is a peer-sync message: its version and its sender's bus id.
type wire interface{ header() (v int, id string) }

func (r *Request) header() (int, string)  { return r.V, r.ID }
func (r *Response) header() (int, string) { return r.V, r.ID }

// decodeOne decodes data as exactly one JSON object of version 1 carrying a
// bus id.
func decodeOne(data []byte, msg wire) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(msg); err != nil {
		// The decoder's text can embed a whole literal from the peer.
		return errors.New("not a valid peer-sync object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("must be exactly one JSON object")
	}
	v, id := msg.header()
	if v != 1 {
		return fmt.Errorf("unsupported version %d", v)
	}
	if !busIDRe.MatchString(id) {
		return errors.New("id is not a bus id")
	}
	return nil
}

func selfName(self string) (string, error) {
	if self != "" {
		return self, nil
	}
	return SelfName()
}

func nowFunc(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

// SSHTransport runs `ssh <dest> crew remote-exec` with request on stdin.
func SSHTransport(ctx context.Context, dest string, request []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", remote.SSHArgv(dest)...)
	cmd.Stdin = bytes.NewReader(request)
	out := &cappedBuffer{max: MaxResponse}
	errb := &headBuffer{max: stderrKeep}
	cmd.Stdout = out
	cmd.Stderr = errb
	err := cmd.Run()
	if out.over {
		return nil, errors.New("ssh: response over 1 MiB")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ssh: %w", ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil, SSHExit{Code: ee.ExitCode(), Stderr: errb.buf.Bytes()}
	}
	if err != nil {
		return nil, fmt.Errorf("ssh: %w", err)
	}
	return out.buf.Bytes(), nil
}

// SSHExit is a failed ssh run. Its Stderr is peer text, so Error omits it: the
// error reaches the stream's error line.
type SSHExit struct {
	Code   int
	Stderr []byte
}

func (e SSHExit) Error() string { return fmt.Sprintf("ssh: exit %d", e.Code) }

// stderrKeep is the most of ssh's stderr kept.
const stderrKeep = 4096

// headBuffer keeps the first max bytes and discards the rest without failing,
// so a chatty stderr neither fills memory nor fails the call.
type headBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// cappedBuffer fails the write that would take it past max, which ends ssh's
// stdout copy, so a runaway peer cannot fill memory. The buffer is a field,
// not embedded: an embedded bytes.Buffer's ReadFrom would let io.Copy bypass
// Write.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New("over the cap")
	}
	return b.buf.Write(p)
}
