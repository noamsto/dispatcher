package marks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

// dir returns a temp crew dir: Path hangs await/ under it, as $dir/await does.
func dir(t *testing.T) string {
	t.Helper()
	d := t.TempDir() + "/crew"
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// write puts a marks file at the path under test, so a test that only reads or
// only merges does not have to trust Record to have produced it.
func write(t *testing.T, d, crew, me, body string) {
	t.Helper()
	p := Path(d, crew, me)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lines is the arm's `jq -c` output: one event per line, in order.
func lines(t *testing.T, body string) []jsonv.Value {
	t.Helper()
	if body == "" {
		return nil
	}
	vs, err := jsonv.DecodeStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse msgs: %v", err)
	}
	return vs
}

func readRaw(t *testing.T, d, crew, me string) string {
	t.Helper()
	b, err := os.ReadFile(Path(d, crew, me))
	if err != nil {
		t.Fatalf("marks file: %v", err)
	}
	return string(b)
}

// TestPath pins the file name against `printf '%s' "$key" | tr -c
// 'A-Za-z0-9._-' '_'` and `... | cksum | cut -d' ' -f1` run in the shell: the
// key carries every character the sanitizer maps, including the `#`, `:` and
// `/` of a real session id, and the two-byte UTF-8 rune that becomes two
// underscores.
func TestPath(t *testing.T) {
	for _, tc := range []struct{ crew, me, want string }{
		{"c1", "worker:feat/x#s1-1", "c1-worker_feat_x_s1-1.3454895548"},
		{"c1", "dispatcher:c1", "c1-dispatcher_c1.3148762201"},
		{"c1", "worker:über/x#s1-1", "c1-worker___ber_x_s1-1.1956298388"},
		{"", "worker:x", "-worker_x.2520424549"},
		{"c1", "", "c1-.3013663994"},
	} {
		if got := Path("/x/crew", tc.crew, tc.me); got != "/x/crew/await/"+tc.want {
			t.Errorf("Path(%q, %q) = %s", tc.crew, tc.me, got)
		}
	}
}

// TestPathMatchesShell cross-checks the same formula against the real tr and
// cksum, the two tools _await_state runs.
func TestPathMatchesShell(t *testing.T) {
	for _, bin := range []string{"tr", "cksum"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	for _, tc := range []struct{ crew, me string }{
		{"1791464376-1014098", "worker:feat/876-crew-go-port-inbox-subcommand#s1791518238-511329"},
		{"c/1", "role:feat/x:reviewer"},
		{"é", "worker:é#s1-1"},
		{"", ""},
	} {
		key := tc.crew + "-" + tc.me
		out, err := exec.Command("sh", "-c",
			`printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_'; printf .; printf '%s' "$1" | cksum | cut -d" " -f1`,
			"_await_state", key).Output()
		if err != nil {
			t.Skipf("tr/cksum pipeline: %v", err)
		}
		if got, want := filepath.Base(Path("/x/crew", tc.crew, tc.me)), strings.TrimSpace(string(out)); got != want {
			t.Errorf("Path key %q = %s, shell says %s", key, got, want)
		}
	}
}

func TestRead(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"empty file", "", `{}`},
		{"one object", `{"dispatcher:c1":1785951264000}` + "\n", `{"dispatcher:c1":1785951264000}`},
		{"no trailing newline", `{"a":1}`, `{"a":1}`},
		{"two objects merge, last wins", "{\"a\":1}\n{\"a\":2,\"b\":3}\n", `{"a":2,"b":3}`},
		// jq's `+` on objects is shallow: a nested value is replaced, not merged.
		{"a later object replaces a nested one", "{\"a\":{\"x\":1}}\n{\"a\":{\"y\":2}}\n", `{"a":{"y":2}}`},
		{"non-objects dropped", "5\n\"s\"\nnull\n[]\ntrue\n{\"a\":1}\n", `{"a":1}`},
		{"objects only, none left", "5\n\"s\"\n", `{}`},
		{"torn tail reads as empty", "{\"a\":1}\n{\"b\":2\n", `{}`},
		{"garbage reads as empty", "not json\n", `{}`},
		{"whitespace only", "  \n\t\n", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dir(t)
			write(t, d, "c1", "worker:feat/x#s1-1", tc.body)
			got := Read(d, "c1", "worker:feat/x#s1-1")
			if testjson.Compact(got) != tc.want {
				t.Errorf("Read = %s, want %s", testjson.Compact(got), tc.want)
			}
		})
	}

	t.Run("no file at all", func(t *testing.T) {
		d := dir(t)
		if got := Read(d, "c1", "worker:feat/x#s1-1"); testjson.Compact(got) != "{}" {
			t.Errorf("Read = %s, want {}", testjson.Compact(got))
		}
	})

	t.Run("marks path is a directory", func(t *testing.T) {
		d := dir(t)
		if err := os.MkdirAll(Path(d, "c1", "me"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := Read(d, "c1", "me"); testjson.Compact(got) != "{}" {
			t.Errorf("Read = %s, want {}", testjson.Compact(got))
		}
	})
}

func TestRecord(t *testing.T) {
	const me = "worker:feat/x#s1-1"

	for _, tc := range []struct {
		name        string
		old         string // existing marks file body, "" for none
		msgs        string
		want        string // "" means the file must not change
		wantWritten bool
	}{
		{
			name:        "first write",
			msgs:        `{"from":"dispatcher:c1","ts":100}` + "\n",
			want:        `{"dispatcher:c1":100}`,
			wantWritten: true,
		},
		{
			name:        "two senders, one each",
			msgs:        "{\"from\":\"dispatcher:c1\",\"ts\":100}\n{\"from\":\"role:feat/x:reviewer\",\"ts\":90}\n",
			want:        `{"dispatcher:c1":100,"role:feat/x:reviewer":90}`,
			wantWritten: true,
		},
		{
			name:        "same sender takes the newest, order aside",
			msgs:        "{\"from\":\"a\",\"ts\":300}\n{\"from\":\"a\",\"ts\":100}\n",
			want:        `{"a":300}`,
			wantWritten: true,
		},
		{
			name:        "an older msg never lowers a mark",
			old:         `{"a":500}` + "\n",
			msgs:        "{\"from\":\"a\",\"ts\":100}\n",
			want:        `{"a":500}`,
			wantWritten: true,
		},
		{
			name:        "a newer msg raises it, the other sender stays",
			old:         `{"a":500,"b":7}` + "\n",
			msgs:        "{\"from\":\"a\",\"ts\":900}\n",
			want:        `{"a":900,"b":7}`,
			wantWritten: true,
		},
		{
			// `max` is jq's total order: a string outranks every number, so a
			// hand-edited mark is never replaced by a real ts.
			name:        "a string mark beats a number ts",
			old:         `{"a":"50"}` + "\n",
			msgs:        "{\"from\":\"a\",\"ts\":900}\n",
			want:        `{"a":"50"}`,
			wantWritten: true,
		},
		{
			name:        "a torn marks file is dropped, not merged",
			old:         `{"a":50` + "\n",
			msgs:        "{\"from\":\"a\",\"ts\":900}\n",
			want:        `{"a":900}`,
			wantWritten: true,
		},
		{
			name:        "no msgs rewrite the marks unchanged",
			old:         `{"a":1}` + "\n",
			want:        `{"a":1}`,
			wantWritten: true,
		},
		// A `from` jq cannot index with fails the reduce as a whole: the helper
		// keeps the file it had, and so does the port.
		{
			name: "a null from loses the write",
			old:  `{"a":1}` + "\n",
			msgs: "{\"from\":\"b\",\"ts\":9}\n{\"ts\":10}\n",
		},
		{
			name: "a numeric from loses the write",
			old:  `{"a":1}` + "\n",
			msgs: "{\"from\":3,\"ts\":9}\n",
		},
		{
			name: "a missing from loses the write",
			old:  `{"a":1}` + "\n",
			msgs: "{\"ts\":10}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dir(t)
			if tc.old != "" {
				write(t, d, "c1", me, tc.old)
			}
			if got := Record(d, "c1", me, lines(t, tc.msgs)); got != tc.wantWritten {
				t.Fatalf("Record = %v, want %v", got, tc.wantWritten)
			}
			if tc.want == "" {
				if got := readRaw(t, d, "c1", me); got != tc.old {
					t.Fatalf("marks file changed: %q, want %q", got, tc.old)
				}
			} else if got := readRaw(t, d, "c1", me); testjson.Compact(testjson.MustParse(t, got)) != tc.want {
				t.Errorf("marks = %s, want %s", got, tc.want)
			}
			if _, err := os.Stat(Path(d, "c1", me)); tc.want != "" && err != nil {
				t.Errorf("marks file: %v", err)
			}
			// mktemp's staging file is renamed away, never left beside it.
			left, err := filepath.Glob(d + "/await/.st.*")
			if err != nil || len(left) != 0 {
				t.Errorf("staging files left: %v (%v)", left, err)
			}
		})
	}
}

// TestRecordModeIsPrivate pins the mode mktemp gives the file: the marks hold
// nothing secret, but a port that switched to os.WriteFile would silently
// widen it.
func TestRecordModeIsPrivate(t *testing.T) {
	d := dir(t)
	if !Record(d, "c1", "me", lines(t, `{"from":"a","ts":1}`+"\n")) {
		t.Fatal("Record failed")
	}
	st, err := os.Stat(Path(d, "c1", "me"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("marks mode = %v, want -rw-------", st.Mode().Perm())
	}
}

// TestRecordMatchesJq is the oracle: run the helper's own reduce through the
// jq it runs, over the same msgs and the same marks file, and compare values
// (key order is engine-internal, docs/crew-go-port.md).
func TestRecordMatchesJq(t *testing.T) {
	bin, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	for _, tc := range []struct{ old, msgs string }{
		{"", `{"from":"dispatcher:c1","ts":100}`},
		{`{"a":500,"b":7}`, "{\"from\":\"a\",\"ts\":100}\n{\"from\":\"c\",\"ts\":1}\n"},
		{`{"a":"50"}`, `{"from":"a","ts":900}`},
		{`{"a":{"x":1},"b":1}`, "{\"from\":\"b\",\"ts\":900}"},
		{`{"a":1}`, "{\"from\":\"a\",\"ts\":9.5}\n{\"from\":\"b\",\"ts\":1e3}\n"},
	} {
		d := dir(t)
		if tc.old != "" {
			write(t, d, "c1", "me", tc.old+"\n")
		}
		msgs := lines(t, tc.msgs+"\n")
		if !Record(d, "c1", "me", msgs) {
			t.Fatal("Record failed")
		}
		got := testjson.Compact(testjson.MustParse(t, readRaw(t, d, "c1", "me")))

		cmd := exec.Command(bin, "-sc", "--argjson", "old", mustOld(t, d), strings.TrimSpace(recordProgram), "-")
		cmd.Stdin = strings.NewReader(tc.msgs + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("jq: %v", err)
		}
		if want := testjson.Compact(testjson.MustParse(t, string(out))); got != want {
			t.Errorf("old %s msgs %s\n got %s\nwant %s", tc.old, tc.msgs, got, want)
		}
	}
}

// mustOld is what the helper passes as --argjson old: _await_marks' own output.
func mustOld(t *testing.T, d string) string {
	t.Helper()
	return testjson.Compact(Read(d, "c1", "me"))
}

// TestReadMatchesJq pins the read program the same way, over files no single
// Go reader would agree about.
func TestReadMatchesJq(t *testing.T) {
	bin, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	for _, body := range []string{
		"",
		`{"a":1}`,
		"{\"a\":{\"x\":1}}\n{\"a\":{\"y\":2},\"b\":[1,2]}\n",
		"5\n\"s\"\nnull\n[]\n",
		"{}\n",
		"{\"a\":1e3}\n{\"a\":1000.0}\n",
	} {
		d := dir(t)
		write(t, d, "c1", "me", body)
		got := testjson.Compact(Read(d, "c1", "me"))

		cmd := exec.Command(bin, "-cs", strings.TrimSpace(readProgram))
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("jq on %q: %v", body, err)
		}
		if want := testjson.Compact(testjson.MustParse(t, string(out))); got != want {
			t.Errorf("Read(%q) = %s, jq says %s", body, got, want)
		}
	}
}
