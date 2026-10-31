package peer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// newBus is a bus with an empty crew/events.jsonl; no git is needed below Sync.
func newBus(t *testing.T) bus.Paths {
	t.Helper()
	common := t.TempDir()
	p := bus.Paths{Common: common, Dir: filepath.Join(common, "crew"), Log: filepath.Join(common, "crew", "events.jsonl")}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// appendRaw appends text to path as is, so a test can leave a torn line.
func appendRaw(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func shortLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := lockWait
	lockWait = d
	t.Cleanup(func() { lockWait = old })
}

func TestBusIDCreatedOnce(t *testing.T) {
	s := StoreFor(newBus(t))
	id, err := s.BusID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\A[0-9a-f]{32}\z`).MatchString(id) {
		t.Fatalf("BusID = %q, want 32 lowercase hex", id)
	}
	again, err := s.BusID()
	if err != nil || again != id {
		t.Fatalf("second BusID = %q, %v, want %q", again, err, id)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "self.id"))
	if err != nil || string(data) != id+"\n" {
		t.Fatalf("self.id = %q, %v", data, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(s.Dir, "self.id.tmp.*")); len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestBusIDRefusesACorruptFile(t *testing.T) {
	s := StoreFor(newBus(t))
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "self.id"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id, err := s.BusID(); err == nil {
		t.Fatalf("BusID = %q, want an error", id)
	}
}

func TestStateMissingIsZero(t *testing.T) {
	st, err := StoreFor(newBus(t)).State("halo")
	if err != nil || st != (State{}) {
		t.Fatalf("State = %+v, %v, want zero", st, err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	s := StoreFor(newBus(t))
	want := State{ID: strings.Repeat("ab", 16), HWM: 120, Scan: 130, Ack: 7}
	if err := s.WriteState("halo", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.State("halo")
	if err != nil || got != want {
		t.Fatalf("State = %+v, %v, want %+v", got, err, want)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, "halo.state"))
	if string(data) != `{"id":"`+want.ID+`","hwm":120,"scan":130,"ack":7}`+"\n" {
		t.Errorf("halo.state = %q", data)
	}
}

func TestSeenRoundTrip(t *testing.T) {
	s := StoreFor(newBus(t))
	if got := s.Seen("halo"); got != 0 {
		t.Fatalf("Seen of a missing file = %d, want 0", got)
	}
	if err := s.WriteSeen("halo", 812); err != nil {
		t.Fatal(err)
	}
	if got := s.Seen("halo"); got != 812 {
		t.Fatalf("Seen = %d, want 812", got)
	}
}

func TestAppendWrapsRows(t *testing.T) {
	s := StoreFor(newBus(t))
	err := s.Append("halo", []Entry{
		{Seq: 40, Row: []byte(`{"ts":1,"kind":"reap","branch":"feat/a<b"}`)},
		{Seq: 95, Row: []byte(`{"ts":2,"kind":"reap","branch":"feat/c"}`)},
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "halo.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"peer":"halo","seq":40,"row":{"ts":1,"kind":"reap","branch":"feat/a<b"}}` + "\n" +
		`{"peer":"halo","seq":95,"row":{"ts":2,"kind":"reap","branch":"feat/c"}}` + "\n"
	if string(data) != want {
		t.Fatalf("halo.jsonl =\n%s\nwant\n%s", data, want)
	}
}

// storeLines writes each row to host's store file in stored form and returns
// the offset past each line.
func storeLines(t *testing.T, s Store, host string, rows ...string) []int64 {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = fmt.Sprintf(`{"peer":%q,"seq":%d,"row":%s}`, host, i+1, r)
	}
	return writeLines(t, filepath.Join(s.Dir, host+".jsonl"), lines...)
}

func shortMaxStore(t *testing.T, n int64) {
	t.Helper()
	old := maxStore
	maxStore = n
	t.Cleanup(func() { maxStore = old })
}

func peerMsg(body string, age time.Duration) string {
	return fmt.Sprintf(`{"ts":%d,"kind":"msg","from":"a","body":%q}`, ms(age), body)
}

const stale = keepFor + time.Hour

func TestAppendCompactsStaleRowsOverCap(t *testing.T) {
	s := StoreFor(newBus(t))
	ends := storeLines(t, s, "halo", peerMsg("old1", stale), peerMsg("live1", 0), peerMsg("old2", stale), peerMsg("live2", 0))
	shortMaxStore(t, ends[3])
	err := s.Append("halo", []Entry{{Seq: 9, Row: []byte(peerMsg("new", 0))}}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "halo.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"old1", "old2"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("halo.jsonl still holds %s:\n%s", gone, data)
		}
	}
	msgs, _, err := Messages(s, "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Body != "live1" || msgs[1].Body != "live2" || msgs[2].Body != "new" {
		t.Errorf("msgs = %+v, want live1, live2, new", msgs)
	}
}

func TestAppendCompactsFarFutureRowsOverCap(t *testing.T) {
	s := StoreFor(newBus(t))
	future := `{"ts":9223372036854775807,"kind":"msg","from":"a","body":"future"}`
	ends := storeLines(t, s, "halo", peerMsg("live1", 0), future, peerMsg("live2", 0))
	shortMaxStore(t, ends[2])
	err := s.Append("halo", []Entry{{Seq: 9, Row: []byte(peerMsg("new", 0))}}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _, err := Messages(s, "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Body != "live1" || msgs[1].Body != "live2" || msgs[2].Body != "new" {
		t.Errorf("msgs = %+v, want live1, live2, new", msgs)
	}
}

func TestAppendRefusesWhenLiveRowsFillCap(t *testing.T) {
	s := StoreFor(newBus(t))
	ends := storeLines(t, s, "halo", peerMsg("live1", 0), peerMsg("live2", 0))
	shortMaxStore(t, ends[1])
	path := filepath.Join(s.Dir, "halo.jsonl")
	before, _ := os.ReadFile(path)
	err := s.Append("halo", []Entry{{Seq: 9, Row: []byte(peerMsg("new", 0))}}, testNow)
	if err == nil {
		t.Fatal("Append over a cap of live rows succeeded")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("halo.jsonl changed:\n%s\nwant\n%s", after, before)
	}
}

func TestCompactRemapsSeen(t *testing.T) {
	s := StoreFor(newBus(t))
	ends := storeLines(t, s, "halo",
		peerMsg("old1", stale), peerMsg("live1", 0), peerMsg("old2", stale), peerMsg("live2", 0), peerMsg("live3", 0))
	if err := s.WriteSeen("halo", ends[3]); err != nil {
		t.Fatal(err)
	}
	shortMaxStore(t, ends[4])
	err := s.Append("halo", []Entry{{Seq: 9, Row: []byte(peerMsg("new", 0))}}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _, err := Messages(s, "halo", s.Seen("halo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Body != "live3" || msgs[1].Body != "new" {
		t.Errorf("unannounced msgs = %+v, want live3, new", msgs)
	}
}

func TestLockExcludesGoroutines(t *testing.T) {
	shortLockWait(t, 150*time.Millisecond)
	s := StoreFor(newBus(t))
	unlock, ok := s.Lock("halo")
	if !ok {
		t.Fatal("first Lock refused")
	}

	done := make(chan bool)
	start := time.Now()
	go func() {
		_, ok := s.Lock("halo")
		done <- ok
	}()
	if <-done {
		t.Fatal("second goroutine took a held lock")
	}
	if waited := time.Since(start); waited < lockWait {
		t.Errorf("second Lock gave up after %v, want at least %v", waited, lockWait)
	}

	shortLockWait(t, 5*time.Second)
	go func() {
		unlock2, ok := s.Lock("halo")
		if ok {
			unlock2()
		}
		done <- ok
	}()
	time.Sleep(100 * time.Millisecond)
	unlock()
	if !<-done {
		t.Fatal("waiting goroutine did not get the lock after the holder released it")
	}
}

// Another process holding the lock is internal/lock's own flock contract; in
// one process lock.Acquire refuses every owner but the holder, which stands in
// for it here.
func TestLockRefusesAnotherOwner(t *testing.T) {
	shortLockWait(t, 100*time.Millisecond)
	s := StoreFor(newBus(t))
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "halo.lock.d")
	if !lock.Acquire(path, "other") {
		t.Fatal("helper acquire refused")
	}
	if _, ok := s.Lock("halo"); ok {
		t.Fatal("Lock taken while another owner holds it")
	}
	lock.Release(path)
	unlock, ok := s.Lock("halo")
	if !ok {
		t.Fatal("Lock refused after the other owner released")
	}
	unlock()
}

func TestLockSharesOneDeadline(t *testing.T) {
	shortLockWait(t, 400*time.Millisecond)
	s := StoreFor(newBus(t))
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "halo.lock.d")
	m := pathMutex(path)
	m.Lock()
	if !lock.Acquire(path, "other") {
		t.Fatal("helper acquire refused")
	}
	t.Cleanup(func() { lock.Release(path) })
	go func() {
		time.Sleep(300 * time.Millisecond)
		m.Unlock()
	}()

	start := time.Now()
	if _, ok := s.Lock("halo"); ok {
		t.Fatal("Lock taken while another owner holds the cross-process lock")
	}
	if waited := time.Since(start); waited >= 650*time.Millisecond {
		t.Errorf("Lock waited %v: the cross-process poll got a fresh deadline", waited)
	}
	if !m.TryLock() {
		t.Fatal("a failed Lock left the in-process mutex held")
	}
	m.Unlock()
}

func TestSelfName(t *testing.T) {
	old := Hostname
	t.Cleanup(func() { Hostname = old })
	for host, want := range map[string]string{
		"Noams-MacBook-Pro.local": "Noams-MacBook-Pro",
		"halo":                    "halo",
		"bad name":                "",
		".local":                  "",
	} {
		Hostname = func() (string, error) { return host, nil }
		got, err := SelfName()
		if want == "" {
			if err == nil {
				t.Errorf("SelfName(%q) = %q, want an error", host, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("SelfName(%q) = %q, %v, want %q", host, got, err, want)
		}
	}
	Hostname = func() (string, error) { return "", errors.New("boom") }
	if _, err := SelfName(); err == nil {
		t.Error("SelfName swallowed a Hostname error")
	}
}
