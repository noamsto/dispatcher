package stream

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSegLogAppendRead(t *testing.T) {
	dir := t.TempDir()
	w, err := openSegLog(dir, defaultSegBytes, defaultKeepSegs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	data := "one\ntwo\nthree\n"
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	chunk, next, oldest, end, err := readLog(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != data || next != int64(len(data)) || oldest != 0 || end != int64(len(data)) {
		t.Fatalf("got %q next=%d oldest=%d end=%d", chunk, next, oldest, end)
	}
}

func TestReadLogPartialTrailingLine(t *testing.T) {
	dir := t.TempDir()
	w, err := openSegLog(dir, defaultSegBytes, defaultKeepSegs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.Write([]byte("a\n")); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(dir, fmt.Sprintf("%020d.jsonl", 0))
	f, err := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("par"); err != nil {
		t.Fatal(err)
	}
	chunk, next, _, _, _ := readLog(dir, 0)
	if string(chunk) != "a\n" || next != 2 {
		t.Fatalf("partial leaked: %q next=%d", chunk, next)
	}
	if _, err := f.WriteString("tial\n"); err != nil {
		t.Fatal(err)
	}
	chunk, next, _, _, _ = readLog(dir, next)
	if string(chunk) != "partial\n" || next != 10 {
		t.Fatalf("got %q next=%d", chunk, next)
	}
}

func TestSegLogRotationRetention(t *testing.T) {
	dir := t.TempDir()
	w, err := openSegLog(dir, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	var lines []string
	for i := range 20 {
		l := fmt.Sprintf("line-%02d-xxxxxxxxxx\n", i)
		lines = append(lines, l)
		if _, err := w.Write([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(ents) != 2 {
		t.Fatalf("want 2 segments, got %d", len(ents))
	}
	_, _, oldest, end, _ := readLog(dir, 0)
	if end != w.end() || oldest == 0 {
		t.Fatalf("oldest=%d end=%d writer end=%d", oldest, end, w.end())
	}
	// below oldest: reads from oldest
	chunk, _, o2, _, _ := readLog(dir, 0)
	if o2 <= 0 || len(chunk) == 0 {
		t.Fatalf("expected clamp, oldest=%d", o2)
	}
	// loop chunk by chunk with a restart carrying only next, from oldest
	var got strings.Builder
	off := oldest
	for {
		c, next, _, _, err := readLog(dir, off)
		if err != nil {
			t.Fatal(err)
		}
		if len(c) == 0 {
			break
		}
		got.Write(c)
		off = next
	}
	want := strings.Join(lines, "")
	if !strings.HasSuffix(want, got.String()) || int64(got.Len()) != end-oldest {
		t.Fatalf("got %q", got.String())
	}
	// reader at a retained mid offset sees exactly the remainder
	mid := oldest + int64(len(lines[0]))
	c, next, _, _, _ := readLog(dir, mid)
	if string(c) != want[int64(len(want))-(end-mid):] || next != end {
		t.Fatalf("mid read mismatch next=%d end=%d", next, end)
	}
	if c, next, _, _, _ = readLog(dir, end+5); len(c) != 0 || next != end+5 {
		t.Fatalf("beyond end: %q %d", c, next)
	}
}

func TestSegLogReopenContinues(t *testing.T) {
	dir := t.TempDir()
	w, err := openSegLog(dir, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if _, err := fmt.Fprintf(w, "row-%d-xxxxxxxxxxxxxxxx\n", i); err != nil {
			t.Fatal(err)
		}
	}
	before := w.end()
	_ = w.Close()
	w2, err := openSegLog(dir, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Close() }()
	if w2.end() != before {
		t.Fatalf("end %d != %d", w2.end(), before)
	}
	if _, err := w2.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	if w2.end() != before+5 {
		t.Fatalf("end %d", w2.end())
	}
	chunk, next, _, end, _ := readLog(dir, before)
	if string(chunk) != "more\n" || next != end {
		t.Fatalf("got %q", chunk)
	}
}

func TestDaemonInfoRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), daemonInfoName)
	if _, ok := readDaemonInfo(p); ok {
		t.Fatal("missing should be !ok")
	}
	in := daemonInfo{PID: 42, StartedMS: 1000, StartOffset: 7}
	if err := writeDaemonInfo(p, in); err != nil {
		t.Fatal(err)
	}
	if out, ok := readDaemonInfo(p); !ok || out != in {
		t.Fatalf("got %+v %v", out, ok)
	}
	if err := os.WriteFile(p, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDaemonInfo(p); ok {
		t.Fatal("corrupt should be !ok")
	}
}

func TestFollowStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), followStateName)
	if _, ok := readFollowState(p); ok {
		t.Fatal("missing should be !ok")
	}
	in := followState{Offset: 99, StartedMS: 5}
	if err := writeFollowState(p, in); err != nil {
		t.Fatal(err)
	}
	if out, ok := readFollowState(p); !ok || out != in {
		t.Fatalf("got %+v %v", out, ok)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readFollowState(p); ok {
		t.Fatal("corrupt should be !ok")
	}
}

func TestSegLogRotatesBeforeTheNextWrite(t *testing.T) {
	dir := t.TempDir()
	w, err := openSegLog(dir, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("12345678\n")); err != nil {
		t.Fatal(err)
	}
	if segs, _ := listSegments(dir); len(segs) != 1 {
		t.Fatalf("rotated on the filling write: %v", segs)
	}
	_ = w.Close()
	w, err = openSegLog(dir, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.Write([]byte("b\n")); err != nil {
		t.Fatal(err)
	}
	segs, _ := listSegments(dir)
	if len(segs) != 2 || segs[1] != (segment{9, 2}) {
		t.Fatalf("a reopened full segment did not rotate first: %v", segs)
	}
}

func TestSegLogRotateErrorWritesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	w, err := openSegLog(dir, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.Write([]byte("full\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if n, err := w.Write([]byte("lost\n")); n != 0 || err == nil {
		t.Fatalf("n=%d err=%v, want 0 and an error", n, err)
	}
	if chunk, _, _, end, _ := readLog(dir, 0); string(chunk) != "full\n" || end != 5 {
		t.Fatalf("bytes landed despite the rotate error: %q end=%d", chunk, end)
	}
}

func TestSegLogPruneErrorKeepsRotating(t *testing.T) {
	dir := t.TempDir()
	w, err := openSegLog(dir, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.Write([]byte("a\n")); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory under a segment name makes its prune fail.
	stuck := filepath.Join(dir, segName(0))
	if err := os.Remove(stuck); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(stuck, "x"))
	if _, err := w.Write([]byte("b\n")); err != nil {
		t.Fatalf("a failed prune failed the write: %v", err)
	}
	if w.end() != 4 {
		t.Fatalf("end %d", w.end())
	}
}
