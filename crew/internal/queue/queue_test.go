package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

const crew = "c1"

// events reads rows back through bus.ReadEvents, so `ts` arrives as json.Number
// exactly as in production.
func events(t *testing.T, rows ...string) []bus.Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return mustRead(t, path)
}

func mustRead(t *testing.T, path string) []bus.Event {
	t.Helper()
	evs, err := bus.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func raw(v any) string {
	return encode(v)
}

func addRowText(ts int64, from, id, ref, branch string, after ...string) string {
	if after == nil {
		after = []string{}
	}
	return raw(row{
		TS: json.Number(strconv.FormatInt(ts, 10)), CrewID: crew, From: from, To: "queue:" + crew, Kind: "msg",
		Body: raw(Entry{ID: id, Task: Task{Ref: orNull(ref), Branch: orNull(branch), Title: "t " + id}, After: after}),
	})
}

func dropRowText(ts int64, from, id string) string {
	return raw(row{
		TS: json.Number(strconv.FormatInt(ts, 10)), CrewID: crew, From: from, To: "queue:" + crew, Kind: "msg",
		Body: raw(dropBody{ID: id, Dropped: true}),
	})
}

func dispatchRowText(ts int64, crewID, branch, alsoCloses string) string {
	extra := ""
	if alsoCloses != "" {
		extra = `,"also_closes":` + alsoCloses
	}
	return `{"ts":` + strconv.FormatInt(ts, 10) + `,"crew_id":"` + crewID + `","kind":"dispatch","branch":"` + branch + `"` + extra + `}`
}

func ids(es []Entry) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func TestOutstanding(t *testing.T) {
	d := "dispatcher:" + crew
	cases := []struct {
		name string
		rows []string
		want []string
	}{
		{"one add", []string{addRowText(10, d, "a", "#12", "feat/12-x", "feat/9-y")}, []string{"a"}},
		{"drop clears", []string{addRowText(10, d, "a", "#12", ""), dropRowText(11, d, "a")}, []string{}},
		{"drop of unknown id", []string{addRowText(10, d, "a", "#12", ""), dropRowText(11, d, "zz")}, []string{"a"}},
		{"dispatch of same branch", []string{addRowText(10, d, "a", "", "feat/12-x"), dispatchRowText(11, crew, "feat/12-x", "")}, []string{}},
		{"dispatch of branch with the ref's issue", []string{addRowText(10, d, "a", "#12", ""), dispatchRowText(11, crew, "fix/12-y", "")}, []string{}},
		{"also_closes number", []string{addRowText(10, d, "a", "13", ""), dispatchRowText(11, crew, "feat/12-x", "[13]")}, []string{}},
		{"also_closes string", []string{addRowText(10, d, "a", "13", ""), dispatchRowText(11, crew, "feat/12-x", `["13"]`)}, []string{}},
		{"also_closes other number", []string{addRowText(10, d, "a", "13", ""), dispatchRowText(11, crew, "feat/12-x", "[14]")}, []string{"a"}},
		{"dispatch older than the add", []string{addRowText(10, d, "a", "#12", ""), dispatchRowText(9, crew, "feat/12-x", "")}, []string{"a"}},
		{"dispatch at the add's ts", []string{addRowText(10, d, "a", "#12", ""), dispatchRowText(10, crew, "feat/12-x", "")}, []string{}},
		{"dispatch of another crew", []string{addRowText(10, d, "a", "#12", ""), dispatchRowText(11, "other", "feat/12-x", "")}, []string{"a"}},
		{"foreign add ignored", []string{addRowText(10, "worker:x", "a", "#12", "")}, []string{}},
		{"foreign drop ignored", []string{addRowText(10, d, "a", "#12", ""), dropRowText(11, "worker:x", "a")}, []string{"a"}},
		{"bad bodies skipped", []string{
			`{"ts":1,"crew_id":"c1","from":"dispatcher:c1","to":"queue:c1","kind":"msg","body":"not json"}`,
			`{"ts":2,"crew_id":"c1","from":"dispatcher:c1","to":"queue:c1","kind":"msg","body":"{\"id\":5,\"task\":{\"title\":\"x\"}}"}`,
			`{"ts":3,"crew_id":"c1","from":"dispatcher:c1","to":"queue:c1","kind":"msg","body":7}`,
			addRowText(10, d, "a", "#12", ""),
		}, []string{"a"}},
		{"branch-shaped ref", []string{addRowText(10, d, "a", "feat/9-z", ""), dispatchRowText(11, crew, "feat/9-z", "")}, []string{}},
		{"linear ref cleared by its branch", []string{addRowText(10, d, "a", "ENG-12", ""), dispatchRowText(11, crew, "eng-12-fix-it", "")}, []string{}},
		{"linear ref cleared by also_closes", []string{addRowText(10, d, "a", "ENG-12", ""), dispatchRowText(11, crew, "feat/3-x", `["ENG-12"]`)}, []string{}},
		{"linear ref not cleared by a longer id", []string{addRowText(10, d, "a", "ENG-12", ""), dispatchRowText(11, crew, "eng-120-x", "")}, []string{"a"}},
		{"issue is anchored", []string{addRowText(10, d, "a", "#4", ""), dispatchRowText(11, crew, "feat/grok-4-7-cursor-ladder", "")}, []string{"a"}},
		{"oldest add first", []string{addRowText(10, d, "a", "#1", ""), addRowText(11, d, "b", "#2", ""), addRowText(12, d, "c", "#3", "")}, []string{"a", "b", "c"}},
		{"other crew's sink", []string{strings.Replace(addRowText(10, d, "a", "#1", ""), "queue:c1", "queue:c2", 1)}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(Outstanding(events(t, tc.rows...), crew))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Outstanding = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOutstandingKeepsTheTask(t *testing.T) {
	got := Outstanding(events(t, addRowText(10, "dispatcher:"+crew, "a", "#12", "feat/12-x", "feat/9-y", "#7")), crew)
	want := []Entry{{
		ID:    "a",
		Task:  Task{Ref: orNull("#12"), Branch: orNull("feat/12-x"), Title: "t a"},
		After: []string{"feat/9-y", "#7"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Outstanding = %+v, want %+v", got, want)
	}
}

func TestSameTask(t *testing.T) {
	e := func(ref, branch string) Entry { return Entry{Task: Task{Ref: orNull(ref), Branch: orNull(branch)}} }
	cases := []struct {
		name string
		a, b Entry
		want bool
	}{
		{"equal branch", e("", "feat/5-x"), e("", "feat/5-x"), true},
		{"hash ref equals bare ref", e("#5", ""), e("5", ""), true},
		{"numeric ref vs branch", e("5", ""), e("", "feat/5-x"), true},
		{"branch vs numeric ref", e("", "feat/5-x"), e("#5", ""), true},
		{"different issues", e("", "feat/5-x"), e("", "feat/6-x"), false},
		{"different numeric refs", e("#5", ""), e("#6", ""), false},
		{"branch refs verbatim", e("feat/5-x", ""), e("feat/5-x", ""), true},
		{"different branch refs", e("feat/5-x", ""), e("feat/5-y", ""), false},
		{"empty branches never equal", e("", ""), e("", ""), false},
		{"branch ref equals branch", e("feat/12-x", ""), e("", "feat/12-x"), true},
		{"numeric ref equals branch ref", e("#12", ""), e("feat/12-x", ""), true},
		{"different branch issues", e("", "feat/12-x"), e("", "feat/13-x"), false},
		{"linear refs case-insensitive", e("ENG-5", ""), e("eng-5", ""), true},
		{"linear ref equals linear branch", e("ENG-5", ""), e("", "eng-5-slug"), true},
		{"lower linear ref equals linear branch", e("eng-5", ""), e("", "eng-5-slug"), true},
		{"linear ids differ", e("ENG-5", ""), e("ENG-50", ""), false},
		{"linear ref vs longer branch id", e("ENG-5", ""), e("", "eng-50-slug"), false},
		{"grok is not issue 4", e("#4", ""), e("", "feat/grok-4-7-x"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTask(tc.a, tc.b); got != tc.want {
				t.Errorf("sameTask = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	if got, want := sanitize("a\tb\nc\x1bd\x7fe\u0085f é日本"), "a b c d e f é日本"; got != want {
		t.Errorf("sanitize = %q, want %q", got, want)
	}
}

type rig struct{ paths bus.Paths }

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	return &rig{paths: bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}}
}

func (r *rig) run(o Options, req Request) (string, string, int) {
	if o.Clock.Now == nil {
		o.Clock = clock.Clock{Now: func() time.Time { return time.Unix(1700000000, 0) }}
	}
	var out, errb bytes.Buffer
	code := Run(context.Background(), req, r.paths, &out, &errb, o)
	return out.String(), errb.String(), code
}

func (r *rig) lines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.paths.Log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func addReq(ref, branch string, title ...string) Request {
	return Request{Action: "add", Crew: crew, Ref: ref, Branch: branch, Title: title}
}

var workerSender = Options{Sender: sender.Probes{Getenv: func(name string) string {
	if name == "CREW_WORKER_ID" {
		return "x"
	}
	return ""
}}}

func TestRunAddDedupes(t *testing.T) {
	r := newRig(t)
	first, _, code := r.run(Options{}, addReq("#12", "", "do", "it"))
	if code != 0 || strings.TrimSpace(first) == "" {
		t.Fatalf("first add: code %d, out %q", code, first)
	}
	again, _, code := r.run(Options{}, addReq("12", "", "other", "words"))
	if code != 0 || again != first {
		t.Errorf("second add = %q (code %d), want %q", again, code, first)
	}
	if n := len(r.lines(t)); n != 1 {
		t.Errorf("log has %d lines, want 1", n)
	}
}

func TestRunAddRefusesAnAlreadyDispatchedTask(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(r.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.paths.Log, []byte(dispatchRowText(1, crew, "feat/12-x", "")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errb, code := r.run(Options{}, addReq("#12", "", "late"))
	if code != 0 || out != "" || !strings.Contains(errb, "crew: queue add: already dispatched as feat/12-x; not queued") {
		t.Errorf("code %d, out %q, err %q", code, out, errb)
	}
	if n := len(r.lines(t)); n != 1 {
		t.Errorf("log has %d lines, want the dispatch row only", n)
	}
}

func TestRunAddDoesNotRefuseOnAnUnanchoredDigitRun(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(r.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.paths.Log, []byte(dispatchRowText(1, crew, "feat/grok-4-7-x", "")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errb, code := r.run(Options{}, addReq("#4", "", "real four"))
	if code != 0 || strings.TrimSpace(out) == "" || errb != "" {
		t.Errorf("code %d, out %q, err %q", code, out, errb)
	}
}

func TestRunAddSanitizesTheRow(t *testing.T) {
	r := newRig(t)
	if _, errb, code := r.run(Options{}, addReq("#1\t2", "", "fix\tthe\nthing\x1b[0m")); code != 0 {
		t.Fatalf("code %d: %s", code, errb)
	}
	got := Outstanding(mustRead(t, r.paths.Log), crew)
	if len(got) != 1 || got[0].Task.Title != "fix the thing [0m" || *got[0].Task.Ref != "#1 2" {
		t.Errorf("entries = %+v", got)
	}
}

func TestRunAddRefusals(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		req  Request
		want string
	}{
		{"no title", Options{}, addReq("#1", ""), "crew: queue add: a title is required"},
		{"no ref or branch", Options{}, addReq("", "", "t"), "crew: queue add: --ref or --branch is required"},
		{"worker sender", workerSender, addReq("#1", "", "t"), "crew: queue add: refusing sender 'dispatcher:c1'"},
		{"bad crew", Options{}, Request{Action: "add", Crew: "a/b", Ref: "#1", Title: []string{"t"}}, "crew: invalid crew id"},
		{"no crew", Options{CrewID: func() string { return "" }}, Request{Action: "add", Ref: "#1", Title: []string{"t"}}, "crew: CREW_ID unset and no WORKER_TASK.md crew_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			out, errb, code := r.run(tc.o, tc.req)
			if code != 1 || out != "" || !strings.Contains(errb, tc.want) {
				t.Errorf("code %d, out %q, err %q", code, out, errb)
			}
			if len(r.lines(t)) != 0 {
				t.Error("a refused add appended a row")
			}
		})
	}
}

func TestRunListAndDrop(t *testing.T) {
	r := newRig(t)
	id, _, _ := r.run(Options{}, Request{Action: "add", Crew: crew, Ref: "#12", After: []string{"feat/9-x", "#7"}, Title: []string{"fix", "it"}})
	id = strings.TrimSpace(id)

	out, _, code := r.run(Options{}, Request{Action: "list", Crew: crew})
	want := "id\tref\tbranch\tafter\ttitle\n" + id + "\t#12\t-\tfeat/9-x,#7\tfix it\n"
	if code != 0 || out != want {
		t.Errorf("list = %q (code %d), want %q", out, code, want)
	}
	out, _, _ = r.run(Options{}, Request{Action: "list", Crew: crew, JSON: true})
	wantJSON := `[{"id":"` + id + `","task":{"ref":"#12","branch":null,"title":"fix it"},"after":["feat/9-x","#7"]}]` + "\n"
	if out != wantJSON {
		t.Errorf("list --json = %q, want %q", out, wantJSON)
	}

	if out, errb, code := r.run(Options{}, Request{Action: "drop", Crew: crew, ID: id}); code != 0 || out != "" {
		t.Errorf("drop: code %d, out %q, err %q", code, out, errb)
	}
	out, _, _ = r.run(Options{}, Request{Action: "list", Crew: crew, JSON: true})
	if out != "[]\n" {
		t.Errorf("list --json after drop = %q", out)
	}
	out, _, _ = r.run(Options{}, Request{Action: "list", Crew: crew})
	if out != "id\tref\tbranch\tafter\ttitle\n" {
		t.Errorf("list after drop = %q", out)
	}
}

// A row appended by hand skips add's sanitizing, so the TSV list does its own.
func TestRunListSanitizesHandWrittenRows(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(r.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := raw(Entry{ID: "1-2", Task: Task{Ref: orNull("#1\t2"), Branch: orNull("b\x1b[0m"), Title: "a\tb\x1b[31mc"}, After: []string{"x\ty", "\x1bz"}})
	line := raw(row{TS: "5", CrewID: crew, From: "dispatcher:" + crew, To: "queue:" + crew, Kind: "msg", Body: body})
	if err := os.WriteFile(r.paths.Log, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code := r.run(Options{}, Request{Action: "list", Crew: crew})
	want := "id\tref\tbranch\tafter\ttitle\n1-2\t#1 2\tb [0m\tx y, z\ta b [31mc\n"
	if code != 0 || out != want {
		t.Errorf("list = %q (code %d), want %q", out, code, want)
	}
}

func TestRunDropAndActionErrors(t *testing.T) {
	r := newRig(t)
	if _, errb, code := r.run(Options{}, Request{Action: "drop", Crew: crew}); code != 1 || !strings.Contains(errb, DropUsage) {
		t.Errorf("drop without id: code %d, err %q", code, errb)
	}
	if _, errb, code := r.run(Options{}, Request{Action: "bogus", Crew: crew}); code != 1 || !strings.Contains(errb, Usage) {
		t.Errorf("unknown action: code %d, err %q", code, errb)
	}
	if _, errb, code := r.run(workerSender, Request{Action: "drop", Crew: crew, ID: "a"}); code != 1 || !strings.Contains(errb, "crew: queue drop: refusing sender") {
		t.Errorf("worker drop: code %d, err %q", code, errb)
	}
}
