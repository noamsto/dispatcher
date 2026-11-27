package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/lock"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

// Files inside the state directory.
const (
	SockFile    = "ctl.sock"
	LockFile    = "session.lock"
	SessionFile = "session.json"
)

// Session is persisted at start and pause so a TUI or a resume can find the
// conversation and launch the same binary, model and extensions as the child.
type Session struct {
	Engine     string `json:"engine"`
	SessionID  string `json:"session_id"`
	Work       string `json:"work"`
	Bin        string `json:"bin,omitempty"`
	Model      string `json:"model,omitempty"`
	SessionDir string `json:"session_dir,omitempty"` // pi
	Gate       string `json:"gate,omitempty"`        // pi
}

// ReadSession reads session.json only when it is a regular file (not a
// symlink) owned by this user and not writable by group or others.
func ReadSession(state string) (Session, error) {
	var s Session
	path := filepath.Join(state, SessionFile)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return s, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return s, err
	}
	if !fi.Mode().IsRegular() {
		return s, fmt.Errorf("%s: not a regular file", path)
	}
	if err := ownedByMe(path, fi); err != nil {
		return s, err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return s, fmt.Errorf("%s: mode %v is writable by group or others", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// CtlRequest is one line of JSON on the control socket. Cmd is one of
// pause, resume, send (Arg is the text), status, stop.
type CtlRequest struct {
	Cmd string `json:"cmd"`
	Arg string `json:"arg,omitempty"`
}

// CtlResponse is the single JSON line answering a CtlRequest. Text carries the
// assistant text of a send.
type CtlResponse struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Text      string `json:"text,omitempty"`
	State     string `json:"state,omitempty"` // running | paused
	Turn      Status `json:"turn,omitempty"`
	Engine    string `json:"engine,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Work      string `json:"work,omitempty"`
}

// Call sends one request to the server listening in state and returns its
// response; a refused command comes back as an error carrying the server's message.
func Call(ctx context.Context, state string, req CtlRequest) (CtlResponse, error) {
	var resp CtlResponse
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", filepath.Join(state, SockFile))
	if err != nil {
		return resp, err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return resp, fmt.Errorf("ctl %s: %w", req.Cmd, err)
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// Config configures a Server. Options supplies the engine settings; the
// server sets Work, SessionID, Resume and (when nil) Transcript itself.
type Config struct {
	Engine      string
	Work        string
	State       string
	Factory     Factory
	Options     event.Options
	SessionDir  string                              // recorded in session.json for engines that store sessions under Work
	Policy      func(*event.Request) event.Decision // nil denies
	Transcript  *transcript.Writer
	Silence     time.Duration // liveness silence limit; 0 disables
	TurnTimeout time.Duration // default 3m
	Grace       time.Duration // shutdown grace before SIGKILL; default 15s
}

// Server owns one engine child at a time and exposes pause/resume/send/status
// on a unix socket in the state directory.
type Server struct {
	cfg Config

	cmdMu   sync.Mutex    // serialises pause/resume/teardown
	sendSem chan struct{} // one send at a time; pause interrupts a send in flight

	mu     sync.Mutex // guards the fields below
	run    *Run
	lk     *lock.Lock
	state  string
	sid    string // last persisted session id
	ctx    context.Context
	stop   chan struct{}
	stopMu sync.Once
}

func NewServer(cfg Config) *Server {
	if cfg.TurnTimeout == 0 {
		cfg.TurnTimeout = 3 * time.Minute
	}
	if cfg.Grace == 0 {
		cfg.Grace = 15 * time.Second
	}
	if cfg.Policy == nil {
		cfg.Policy = func(*event.Request) event.Decision {
			return event.Decision{Verdict: "deny", Reason: "hsup serve: no policy configured"}
		}
	}
	return &Server{cfg: cfg, state: "paused", stop: make(chan struct{}), sendSem: make(chan struct{}, 1)}
}

// Serve takes the session lock, launches the engine, and serves the socket
// until ctx ends or a stop command arrives.
func (s *Server) Serve(ctx context.Context) error {
	if err := PrepareState(s.cfg.State); err != nil {
		return err
	}
	s.ctx = ctx
	lk, err := lock.Acquire(filepath.Join(s.cfg.State, LockFile))
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	s.lk = lk
	id := s.cfg.Options.SessionID
	if id == "" {
		id = newSessionID()
	}
	if err := s.launch(id, s.cfg.Options.Resume, lk); err != nil {
		s.lk = nil
		return errors.Join(err, lk.Release())
	}

	sock := filepath.Join(s.cfg.State, SockFile)
	_ = os.Remove(sock) // our own stale socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return errors.Join(err, s.teardown())
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return errors.Join(err, s.teardown())
	}
	go s.accept(ln)

	select {
	case <-ctx.Done():
	case <-s.stop:
	}
	_ = ln.Close()
	_ = os.Remove(sock)
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	return s.teardown()
}

func (s *Server) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var req CtlRequest
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(CtlResponse{Error: "bad request: " + err.Error()})
		return
	}
	resp := s.handle(req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (s *Server) handle(req CtlRequest) CtlResponse {
	if req.Cmd == "status" {
		return s.status()
	}
	if req.Cmd == "stop" {
		s.stopMu.Do(func() { close(s.stop) })
		return CtlResponse{OK: true}
	}
	var err error
	var text string
	switch req.Cmd {
	case "pause":
		s.cmdMu.Lock()
		text, err = s.pause()
		s.cmdMu.Unlock()
	case "resume":
		s.cmdMu.Lock()
		err = s.resume()
		s.cmdMu.Unlock()
	case "send":
		text, err = s.send(req.Arg)
	default:
		err = fmt.Errorf("unknown command %q", req.Cmd)
	}
	resp := s.status()
	resp.Text = text
	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
	}
	return resp
}

func (s *Server) status() CtlResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := CtlResponse{OK: true, State: s.state, Engine: s.cfg.Engine, Work: s.cfg.Work}
	if s.run != nil {
		resp.SessionID = s.run.Engine.SessionID()
		if s.state == "running" {
			resp.Turn = s.run.Mon.Status()
		}
	}
	return resp
}

// PrepareState creates dir 0700, or accepts an existing one only when
// CheckState does: the socket, lock and session.json inside it steer what a
// TUI later launches.
func PrepareState(dir string) error {
	err := CheckState(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o700)
	}
	return err
}

// CheckState accepts dir only when it is a real directory owned by this user
// and closed to group and others. It never creates anything.
func CheckState(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("state %s: not a directory (or a symlink)", dir)
	}
	if err := ownedByMe("state "+dir, fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state %s: mode %v is open to group or others; want 0700", dir, fi.Mode().Perm())
	}
	return nil
}

func ownedByMe(name string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s: not owned by uid %d", name, os.Getuid())
	}
	return nil
}

// launch persists session.json, then builds and starts an engine on id. The
// child inherits lk's file so the flock outlives hsup while the child runs.
func (s *Server) launch(id string, resume bool, lk *lock.Lock) error {
	if err := s.writeSession(id); err != nil {
		return err
	}
	o := s.cfg.Options
	o.Work, o.SessionID, o.Resume = s.cfg.Work, id, resume
	o.Inherit = append(slices.Clip(o.Inherit), lk.File())
	if o.Transcript == nil {
		o.Transcript = s.cfg.Transcript
	}
	eng, err := s.cfg.Factory(o)
	if err != nil {
		return err
	}
	run := &Run{
		Engine: eng, T: s.cfg.Transcript, Policy: s.cfg.Policy,
		Mon: &Monitor{Silence: s.cfg.Silence},
	}
	if err := run.Start(s.ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.run, s.state = run, "running"
	s.mu.Unlock()
	return nil
}

// writeSession persists id; an empty id (a child that never reported one)
// keeps the last persisted id.
func (s *Server) writeSession(id string) error {
	s.mu.Lock()
	if id == "" {
		id = s.sid
	}
	s.sid = id
	s.mu.Unlock()
	o := s.cfg.Options
	b, err := json.Marshal(Session{
		Engine: s.cfg.Engine, SessionID: id, Work: s.cfg.Work,
		Bin: o.Bin, Model: o.Model, SessionDir: s.cfg.SessionDir, Gate: o.GatePath,
	})
	if err != nil {
		return err
	}
	// A fresh temp file renamed over session.json never follows a planted symlink.
	f, err := os.CreateTemp(s.cfg.State, SessionFile+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.cfg.State, SessionFile))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func (s *Server) current() (*Run, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run, s.state
}

// pause returns a note when the child exited with an error but was reaped, so
// the pause still went through.
func (s *Server) pause() (string, error) {
	run, state := s.current()
	if state != "running" {
		return "", errors.New("pause: already paused")
	}
	if st := run.Mon.Status(); st == StatusActive || st == StatusStale {
		run.Note("pause: interrupting open turn")
		_ = run.Engine.Interrupt()
	}
	// Holding the send slot keeps new sends out; an in-flight send ends on the
	// interrupt's TurnEnd, and is cut off by the shutdown if it never does.
	acquired := false
	select {
	case s.sendSem <- struct{}{}:
		acquired = true
	case <-time.After(s.cfg.Grace):
	}
	err := run.Shutdown(s.cfg.Grace)
	if !acquired {
		s.sendSem <- struct{}{}
	}
	defer func() { <-s.sendSem }()
	var note string
	if err != nil {
		select {
		case <-run.Done():
			// The child was reaped; a non-zero exit status is no reason to stay running.
			note = "pause: child exited with error: " + err.Error()
			run.Note(note)
		default:
			// The child may still be writing the session: keep the lock so no TUI opens it.
			run.Note("pause: shutdown failed, still running: " + err.Error())
			return "", fmt.Errorf("pause: %w", err)
		}
	}
	id := run.Engine.SessionID()
	err = s.writeSession(id)
	s.mu.Lock()
	s.state = "paused"
	lk := s.lk
	s.lk = nil
	s.mu.Unlock()
	if lk != nil {
		err = errors.Join(err, lk.Release())
	}
	return note, err
}

func (s *Server) resume() error {
	if _, state := s.current(); state != "paused" {
		return errors.New("resume: not paused")
	}
	sess, err := ReadSession(s.cfg.State)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	path := filepath.Join(s.cfg.State, LockFile)
	lk, err := lock.Acquire(path)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return fmt.Errorf("resume: %w (%s): a TUI owns this session; quit it first", err, path)
		}
		return fmt.Errorf("resume: %w", err)
	}
	if err := s.launch(sess.SessionID, true, lk); err != nil {
		_ = lk.Release()
		return fmt.Errorf("resume: %w", err)
	}
	s.mu.Lock()
	s.lk = lk
	s.mu.Unlock()
	return nil
}

func (s *Server) send(text string) (string, error) {
	s.sendSem <- struct{}{}
	defer func() { <-s.sendSem }()
	run, state := s.current()
	if state != "running" {
		return "", errors.New("send: session is paused")
	}
	mark := run.Mark()
	end, err := run.Turn(text, s.cfg.TurnTimeout)
	if err != nil {
		return run.TextSince(mark), fmt.Errorf("send: %w", err)
	}
	reply := run.TextSince(mark)
	if end.IsError {
		return reply, fmt.Errorf("send: turn ended in error: %s", end.Text)
	}
	return reply, nil
}

// teardown stops the engine if running and releases the lock; callers hold cmdMu.
func (s *Server) teardown() error {
	run, state := s.current()
	var err error
	if state == "running" {
		err = run.Shutdown(s.cfg.Grace)
		_ = s.writeSession(run.Engine.SessionID())
	}
	s.mu.Lock()
	lk := s.lk
	s.lk, s.state = nil, "paused"
	s.mu.Unlock()
	if lk != nil {
		err = errors.Join(err, lk.Release())
	}
	return err
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
