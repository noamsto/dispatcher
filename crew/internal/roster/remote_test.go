package roster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

func busEvents(t *testing.T, lines ...string) []bus.Event {
	t.Helper()
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := bus.ReadEvents(log)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func remoteDispatch(branch, name, host string) string {
	return `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"` + branch + `","name":"` + name + `","host":"` + host + `","remote":true}`
}

const localRows = `[{"branch":"feat/near","state":"working"}]`

// capture is a Remote whose host answers from a table.
type captureCall struct {
	host string
	argv []string
}

func TestMergeRemoteWithoutRemoteRowsChangesNothing(t *testing.T) {
	events := busEvents(t, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/near","name":"nova","host":"h1"}`)
	local := testjson.MustParse(t, localRows)
	got := MergeRemote(context.Background(), local, events, "c1", func(context.Context, string, []string, io.Writer, io.Writer) int {
		t.Fatal("asked a host with no remote rows")
		return 0
	})
	if testjson.Compact(got) != testjson.Compact(testjson.MustParse(t, localRows)) {
		t.Errorf("got %s", testjson.Compact(got))
	}
}

func TestMergeRemoteTagsAndKeepsOnlyThisHostsBranches(t *testing.T) {
	events := busEvents(t,
		remoteDispatch("feat/far", "iris", "mini"),
		remoteDispatch("feat/also", "sage", "mini"),
		remoteDispatch("feat/big", "otto", "big"),
	)
	answers := map[string]string{
		"mini": `[{"branch":"feat/far","state":"working","live":true},{"branch":"feat/stranger","state":"working","live":true},{"branch":"feat/also","state":"done","live":false}]`,
		"big":  `[{"branch":"feat/big","state":"blocked","live":true}]`,
	}
	var calls []captureCall
	got := MergeRemote(context.Background(), testjson.MustParse(t, localRows), events, "c1",
		func(ctx context.Context, host string, argv []string, stdout, _ io.Writer) int {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("a remote roster has no deadline")
			}
			calls = append(calls, captureCall{host, argv})
			_, _ = io.WriteString(stdout, answers[host])
			return 0
		})
	wantCalls := []captureCall{
		{"big", []string{"crew", "roster", "c1", "--live"}},
		{"mini", []string{"crew", "roster", "c1", "--live"}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("calls = %v, want %v", calls, wantCalls)
	}
	want := `[{"branch":"feat/near","state":"working"},` +
		`{"branch":"feat/big","state":"blocked","live":true,"host":"big"},` +
		`{"branch":"feat/far","state":"working","live":true,"host":"mini"},` +
		`{"branch":"feat/also","state":"done","live":false,"host":"mini"}]`
	if testjson.Compact(got) != testjson.Compact(testjson.MustParse(t, want)) {
		t.Errorf("got  %s\nwant %s", testjson.Compact(got), testjson.Compact(testjson.MustParse(t, want)))
	}
	// Local rows come first and keep their key order and bytes.
	if s := string(jsonv.Append(nil, got.Elems()[0], jsonv.Options{})); s != `{"branch":"feat/near","state":"working"}` {
		t.Errorf("local row changed: %s", s)
	}
}

func TestMergeRemoteUnreachableHostGetsPlaceholders(t *testing.T) {
	events := busEvents(t,
		remoteDispatch("feat/far", "iris", "mini"),
		remoteDispatch("feat/also", "sage", "mini"),
	)
	for name, tc := range map[string]struct {
		rc      int
		stdout  string
		stderr  string
		wantErr string
	}{
		"ssh failed":     {rc: 255, stderr: "ssh: connect to host mini: Connection refused\ncrew: remote-run: ssh to host 'mini' failed (exit 255)\n", wantErr: "ssh: connect to host mini: Connection refused"},
		"refused":        {rc: 2, stderr: "crew: remote-run: unknown host 'mini' (no hosts.mini in settings)\n", wantErr: "crew: remote-run: unknown host 'mini' (no hosts.mini in settings)"},
		"silent failure": {rc: 1, wantErr: "exit status 1"},
		"bad json":       {rc: 0, stdout: "not json", wantErr: ""},
		"not an array":   {rc: 0, stdout: `{"a":1}`, wantErr: "remote roster is not an array"},
	} {
		t.Run(name, func(t *testing.T) {
			got := MergeRemote(context.Background(), testjson.MustParse(t, localRows), events, "c1",
				func(_ context.Context, _ string, _ []string, stdout, stderr io.Writer) int {
					_, _ = io.WriteString(stdout, tc.stdout)
					_, _ = io.WriteString(stderr, tc.stderr)
					return tc.rc
				})
			rows := got.Elems()
			if len(rows) != 3 {
				t.Fatalf("rows = %s, want the local row and one placeholder per remote branch", testjson.Compact(got))
			}
			for i, w := range []struct{ branch, name string }{{"feat/also", "sage"}, {"feat/far", "iris"}} {
				row := rows[i+1]
				errv, _ := row.Get("host_error")
				msg, _ := errv.AsString()
				if tc.wantErr != "" && msg != tc.wantErr || msg == "" {
					t.Errorf("host_error = %q, want %q", msg, tc.wantErr)
				}
				if s := string(jsonv.Append(nil, jsonv.Object(nonErr(row.Members())...), jsonv.Options{})); s !=
					`{"branch":"`+w.branch+`","name":"`+w.name+`","host":"mini","state":null,"live":null}` {
					t.Errorf("placeholder = %s", s)
				}
			}
		})
	}
}

func nonErr(ms []jsonv.Member) []jsonv.Member {
	var out []jsonv.Member
	for _, m := range ms {
		if m.Key != "host_error" {
			out = append(out, m)
		}
	}
	return out
}

func TestMergeRemoteBoundsEachHostBy30Seconds(t *testing.T) {
	events := busEvents(t, remoteDispatch("feat/far", "iris", "mini"))
	MergeRemote(context.Background(), testjson.MustParse(t, localRows), events, "c1",
		func(ctx context.Context, _ string, _ []string, _, _ io.Writer) int {
			deadline, ok := ctx.Deadline()
			if left := time.Until(deadline); !ok || left > 30*time.Second || left < 29*time.Second {
				t.Errorf("deadline in %v, want about 30s", left)
			}
			return 0
		})
}
