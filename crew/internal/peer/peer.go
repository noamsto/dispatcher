// Package peer links two dispatchers on the same repo on different hosts: each
// side exports its own dispatcher rows (claims, dispatches, reaps, peer
// messages) and stores the other side's in a separate read-only store under
// `<common>/crew/peers/`, which the claim gate and the stream read.
//
// This is trust-boundary code. A peer row is data from another host: it is
// admitted field by field against a per-kind allowlist, every size is capped,
// control characters drop it, and nothing in it ever selects a host, path,
// command or recipient. Provenance is the store's wrapper (`peer`, from the
// file it lands in), never a field of the row. Nothing here writes to
// `events.jsonl`.
package peer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// MaxRow and MaxBody are below the spec's 8 KiB / 4000 so a stored line, the
// row inside its `{"peer","seq","row"}` wrapper, stays within bus.LineMax: the
// cap every bus line has and the size one O_APPEND write keeps whole.
const (
	MaxRow      = 3900
	MaxBody     = 3500
	MaxRows     = 200
	MaxBatch    = 384 << 10
	MaxPayload  = 1 << 20
	MaxResponse = 1 << 20
	ClaimTTL    = 15 * time.Minute
	SyncEvery   = 30 * time.Second
	SyncTimeout = 60 * time.Second
	LockWait    = 5 * time.Second
)

// Entry is one exported row and its seq: the byte offset just past the row's
// line in its writer's events.jsonl.
type Entry struct {
	Seq int64           `json:"seq"`
	Row json.RawMessage `json:"row"`
}

// Request is the payload `crew peer-sync` reads on stdin. Expect is the id the
// requester has pinned for the server ("" before first contact), so the server
// can refuse an id change before it stores anything.
type Request struct {
	V      int     `json:"v"`
	Host   string  `json:"host"`
	ID     string  `json:"id"`
	Expect string  `json:"expect"`
	Since  int64   `json:"since"`
	Cursor int64   `json:"cursor"`
	Rows   []Entry `json:"rows"`
}

// Response is the object `crew peer-sync` prints.
type Response struct {
	V       int            `json:"v"`
	Host    string         `json:"host"`
	ID      string         `json:"id"`
	Ack     int64          `json:"ack"`
	Cursor  int64          `json:"cursor"`
	More    bool           `json:"more"`
	Rows    []Entry        `json:"rows"`
	Dropped map[string]int `json:"dropped"`
}

// State is `<host>.state`: the peer's pinned bus id, the peer's scan cursor we
// have stored through (HWM), and our scan cursor the peer last acknowledged.
type State struct {
	ID  string `json:"id"`
	HWM int64  `json:"hwm"`
	Ack int64  `json:"ack"`
}

// Store is the peer store directory, `<common>/crew/peers`. Every host passed
// to its methods is a settings host name, which remote.Load has already held to
// remote.HostNameRe, so it is a safe file name.
type Store struct {
	Dir string
}

// StoreFor is the peer store of the bus at p.
func StoreFor(p bus.Paths) Store {
	return Store{Dir: filepath.Join(p.Dir, "peers")}
}

func (s Store) path(host, ext string) string {
	return filepath.Join(s.Dir, host+ext)
}

// lockWait is LockWait, shortened by tests.
var lockWait = LockWait

const lockPoll = 20 * time.Millisecond

// mutexes holds one in-process mutex per lock path. lock.Acquire is re-entrant
// for one owner within a process, so two goroutines (the stream's sync and wake
// loops) would both win it; the mutex is what excludes them.
var (
	mutexesMu sync.Mutex
	mutexes   = map[string]*sync.Mutex{}
)

func pathMutex(path string) *sync.Mutex {
	mutexesMu.Lock()
	defer mutexesMu.Unlock()
	m, ok := mutexes[path]
	if !ok {
		m = &sync.Mutex{}
		mutexes[path] = m
	}
	return m
}

// Lock takes host's `<host>.lock.d`, polling the in-process mutex and then the
// cross-process lock under one deadline of lockWait. ok is false when either
// stayed busy (or the store dir could not be made); unlock releases both.
func (s Store) Lock(host string) (unlock func(), ok bool) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, false
	}
	path := s.path(host, ".lock.d")
	deadline := time.Now().Add(lockWait)
	m := pathMutex(path)
	if !poll(deadline, m.TryLock) {
		return nil, false
	}
	owner := strconv.Itoa(os.Getpid())
	if !poll(deadline, func() bool { return lock.Acquire(path, owner) }) {
		m.Unlock()
		return nil, false
	}
	return func() {
		lock.Release(path)
		m.Unlock()
	}, true
}

func poll(deadline time.Time, try func() bool) bool {
	for {
		if try() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(lockPoll)
	}
}

// State reads host's state; a missing file is the zero State.
func (s Store) State(host string) (State, error) {
	path := s.path(host, ".state")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// WriteState replaces host's state whole.
func (s Store) WriteState(host string, st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.replace(s.path(host, ".state"), append(data, '\n'))
}

// Seen is the offset into `<host>.jsonl` through which peer messages have been
// announced; missing or unreadable is 0, so they are announced (again).
func (s Store) Seen(host string) int64 {
	data, err := os.ReadFile(s.path(host, ".seen"))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// WriteSeen replaces host's seen offset.
func (s Store) WriteSeen(host string, off int64) error {
	return s.replace(s.path(host, ".seen"), []byte(strconv.FormatInt(off, 10)+"\n"))
}

func (s Store) replace(path string, data []byte) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}

type storedLine struct {
	Peer string          `json:"peer"`
	Seq  int64           `json:"seq"`
	Row  json.RawMessage `json:"row"`
}

// Append writes each entry to `<host>.jsonl` as a `{"peer","seq","row"}` line.
func (s Store) Append(host string, es []Entry) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	path := s.path(host, ".jsonl")
	for _, e := range es {
		line, err := encode(storedLine{Peer: host, Seq: e.Seq, Row: e.Row})
		if err != nil {
			return err
		}
		if err := bus.Append(path, string(line)); err != nil {
			return err
		}
	}
	return nil
}

var busIDRe = regexp.MustCompile(`\A[0-9a-f]{32}\z`)

// BusID is this clone's bus id, `self.id`, created once. The id is written to
// a temp file and hard-linked into place, an exclusive create that never shows
// a concurrent reader a half-written id.
func (s Store) BusID() (string, error) {
	path := filepath.Join(s.Dir, "self.id")
	id, err := readBusID(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id = hex.EncodeToString(b[:])
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.Dir, "self.id.tmp.*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readBusID(path)
		}
		return "", err
	}
	return id, nil
}

func readBusID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimRight(string(data), "\n")
	if !busIDRe.MatchString(id) {
		return "", fmt.Errorf("%s: not a bus id", path)
	}
	return id, nil
}

// Hostname is the source of SelfName.
var Hostname = os.Hostname

// SelfName is this host's short name, `uname -n` up to the first dot: the name
// a peer's settings must list this host under.
func SelfName() (string, error) {
	h, err := Hostname()
	if err != nil {
		return "", err
	}
	name, _, _ := strings.Cut(h, ".")
	if !remote.HostNameRe.MatchString(name) {
		return "", fmt.Errorf("self name %q is not a valid host name, so this host cannot peer", name)
	}
	return name, nil
}

// encode is compact JSON without HTML escaping and without a trailing newline.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
