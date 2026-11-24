package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

var (
	goldenTS  = regexp.MustCompile(`\b1[0-9]{9,12}\b`)
	goldenISO = regexp.MustCompile(`\b20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:]+Z\b`)
	goldenBy  = regexp.MustCompile(` by=.*`)
	goldenUID = regexp.MustCompile(`linux:[0-9a-f-]{36}:[0-9]+|lstart:[A-Za-z]{3} [A-Za-z]{3} [0-9]{1,2} [0-9:]{8} [0-9]{4}`)
)

// proc.OS.Start is a boot-id token on Linux and ps' lstart elsewhere; the
// golden must normalize both or it only passes on Linux.
func TestGoldenStartTokenNormalization(t *testing.T) {
	for _, tok := range []string{
		"linux:0a1b2c3d-0000-4000-8000-123456789abc:48213",
		"lstart:Tue Nov 24 03:50:07 2026",
		"lstart:Mon Jan  5 03:50:07 2026",
	} {
		tok = strings.Join(strings.Fields(tok), " ")
		if got := goldenUID.ReplaceAllString("4242 "+tok, "<START>"); got != "4242 <START>" {
			t.Errorf("%q normalized to %q", tok, got)
		}
	}
}

// TestLayout1Golden records the unswapped layout-1 file-level behaviour of one
// crew's lifecycle, so a state-root change can prove layout 1 stays
// byte-identical. Regenerate only with UPDATE_GOLDEN=1.
func TestLayout1Golden(t *testing.T) {
	const crew = "gold1"
	// The golden records layout-1 bytes.
	t.Setenv("CREW_LAYOUT", "1")
	// newCrewsCLI moves the cwd into the temp repo.
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "layout1-golden"))
	if err != nil {
		t.Fatal(err)
	}
	c := newCrewsCLI(t)
	home := t.TempDir()
	state := t.TempDir()
	clockFile := filepath.Join(t.TempDir(), "clock")
	if err := os.WriteFile(clockFile, []byte("1700000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("CREW_CLOCK", clockFile)
	pid := strconv.Itoa(os.Getppid())
	common := filepath.Join(c.repo, ".git")

	norm := func(s string) string {
		for old, repl := range map[string]string{c.repo: "<REPO>", home: "<HOME>", state: "<STATE>", clockFile: "<CLOCK>"} {
			s = strings.ReplaceAll(s, old, repl)
		}
		s = goldenBy.ReplaceAllString(s, " by=<BY>")
		s = goldenISO.ReplaceAllString(s, "<ISO>")
		s = goldenUID.ReplaceAllString(s, "<START>")
		s = goldenTS.ReplaceAllString(s, "<TS>")
		// Last, so a small pid cannot match inside a timestamp or start token.
		for _, p := range []string{pid, strconv.Itoa(os.Getpid())} {
			s = regexp.MustCompile(`\b`+p+`\b`).ReplaceAllString(s, "<PID>")
		}
		return s
	}

	// snapshot lists every file under root; content is shown for the files
	// that are small state, not for the bus log (rows are dumped separately).
	snapshot := func(root string, withContent func(rel string) bool) string {
		var rels []string
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rels = append(rels, rel)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		sort.Strings(rels)
		var b strings.Builder
		for _, rel := range rels {
			fmt.Fprintf(&b, "== %s\n", norm(rel))
			if withContent(rel) {
				data, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil {
					t.Fatal(err)
				}
				b.WriteString(norm(string(data)))
				if len(data) > 0 && data[len(data)-1] != '\n' {
					b.WriteString("\n<no trailing newline>\n")
				}
			}
		}
		return b.String()
	}
	busContent := func(rel string) bool {
		return rel != "events.jsonl" && !strings.HasSuffix(rel, string(filepath.Separator)+"events.jsonl")
	}
	rows := func() string {
		data, err := os.ReadFile(filepath.Join(common, "crew", "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("row %q: %v", line, err)
			}
			out, _ := json.Marshal(row) // sorted keys
			b.WriteString(norm(string(out)) + "\n")
		}
		return b.String()
	}
	full := func(label string) string {
		return "##### " + label + "\n" +
			"--- <common>/crew\n" + snapshot(filepath.Join(common, "crew"), busContent) +
			"--- $XDG_STATE_HOME\n" + snapshot(state, func(string) bool { return true }) +
			"--- events.jsonl rows\n" + rows()
	}

	const worker = "worker:feat/x#s1-1"
	c.as(crew)
	c.mustRun("register", pid)
	c.mustRun("status", worker, "working")
	c.mustRun("msg", worker, "dispatcher:"+crew, "hello")
	c.mustRun("hold", "add", "--engine", "claude", "--window", "5h", "--resets-at", "1700000100",
		"--agent", "codex", "--ref", "r1", "--branch", "b1", "--tier", "standard", "--model", "sonnet",
		"--effort", "medium", "--crew", crew, "gold hold")
	c.mustRun("msg", "dispatcher:"+crew, worker, "go")
	c.mustRun("inbox", worker, crew)
	c.mustRun("adopt", crew, pid)

	line, err := json.Marshal(map[string]any{
		"ts": 1700000001000, "crew_id": crew, "kind": "resume", "branch": "feat/x",
		"worker_id": worker, "session": "s1", "repo": c.repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Append(filepath.Join(common, "crew", "events.jsonl"), string(line)); err != nil {
		t.Fatal(err)
	}

	got := full("before deregister")
	c.mustRun("deregister")
	got += full("after deregister")

	for _, p := range []string{filepath.Join(state, "crew", "crews"), filepath.Join(state, "crew", "index")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists in layout 1 (err %v)", p, err)
		}
	}

	golden := filepath.Join(goldenDir, "lifecycle.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("layout-1 snapshot differs from %s:\n got:\n%s\nwant:\n%s", golden, got, want)
	}
}
