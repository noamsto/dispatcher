package roster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
	var (
		mu    sync.Mutex
		calls []captureCall
	)
	got := MergeRemote(context.Background(), testjson.MustParse(t, localRows), events, "c1",
		func(ctx context.Context, host string, argv []string, stdout, _ io.Writer) int {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("a remote roster has no deadline")
			}
			mu.Lock()
			calls = append(calls, captureCall{host, argv})
			mu.Unlock()
			_, _ = io.WriteString(stdout, answers[host])
			return 0
		})
	slices.SortFunc(calls, func(a, b captureCall) int { return strings.Compare(a.host, b.host) })
	wantCalls := []captureCall{
		{"big", []string{"crew", "roster", "--live", "--", "c1"}},
		{"mini", []string{"crew", "roster", "--live", "--", "c1"}},
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
					`{"branch":"`+w.branch+`","name":"`+w.name+`","host":"mini","state":null,"live":null,"ts":1}` {
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

// A placeholder carries the ts of the branch's newest dispatch row, so a reader
// computing its age does not see the epoch.
func TestMergeRemotePlaceholderCarriesTheDispatchTS(t *testing.T) {
	events := busEvents(t,
		`{"ts":1700000000000,"crew_id":"c1","kind":"dispatch","branch":"feat/far","name":"iris","host":"mini","remote":true}`,
		`{"ts":1700000005000,"crew_id":"c1","kind":"dispatch","branch":"feat/far","name":"iris","host":"mini","remote":true}`,
	)
	got := MergeRemote(context.Background(), testjson.MustParse(t, `[]`), events, "c1",
		func(context.Context, string, []string, io.Writer, io.Writer) int { return 255 })
	ts, _ := got.Elems()[0].Get("ts")
	if s := string(jsonv.Append(nil, ts, jsonv.Options{})); s != "1700000005000" {
		t.Errorf("ts = %s, want the newest dispatch row's 1700000005000", s)
	}
}

// Hosts are asked at once, so one slow host costs its own bound, not the sum;
// the rows still come back in host order.
func TestMergeRemoteAsksHostsConcurrently(t *testing.T) {
	events := busEvents(t,
		remoteDispatch("feat/far", "iris", "mini"),
		remoteDispatch("feat/big", "otto", "big"),
	)
	var arrived sync.WaitGroup
	arrived.Add(2)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	got := MergeRemote(context.Background(), testjson.MustParse(t, `[]`), events, "c1",
		func(_ context.Context, host string, _ []string, stdout, stderr io.Writer) int {
			arrived.Done()
			select {
			case <-all:
			case <-time.After(5 * time.Second):
				_, _ = io.WriteString(stderr, "asked one host at a time")
				return 1
			}
			_, _ = io.WriteString(stdout, `[{"branch":"feat/`+map[string]string{"mini": "far", "big": "big"}[host]+`","state":"working"}]`)
			return 0
		})
	want := `[{"branch":"feat/big","state":"working","host":"big"},{"branch":"feat/far","state":"working","host":"mini"}]`
	if testjson.Compact(got) != testjson.Compact(testjson.MustParse(t, want)) {
		t.Errorf("got  %s\nwant %s", testjson.Compact(got), want)
	}
}
