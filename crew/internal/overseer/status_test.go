package overseer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/fleet"
)

const statusPrefix = "crew: overseer status: "

// appendRows adds lines to the log at common.
func appendRows(t *testing.T, common string, lines ...string) {
	t.Helper()
	log, err := os.OpenFile(common+"/crew/events.jsonl", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if _, err := log.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// reply is a msg row on a bus, its body a JSON string the way `crew msg` writes it.
func reply(ts int64, from, to, body string) string {
	return `{"ts":` + strconv.FormatInt(ts, 10) + `,"kind":"msg","crew_id":"c1","from":"` + from + `","to":"` + to + `","body":` + jsonString(body) + `}`
}

func (f *fixture) status(req StatusRequest) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunStatus(f.t.Context(), req, &stdout, &stderr, f.opts())
	return code, stdout.String(), stderr.String()
}

func TestStatusRefusesAnOrigin(t *testing.T) {
	neg := int64(-1)
	cases := []struct {
		name string
		req  StatusRequest
		mark func(f *fixture)
		want string
	}{
		{"not a settings host", StatusRequest{Origin: "evil"}, nil, "origin evil is not in settings hosts"},
		{"this host", StatusRequest{Origin: "mbp"}, nil, "origin mbp is this host's own name, and only another host's overseer pulls"},
		{"not a host name", StatusRequest{Origin: "a b"}, nil, `origin "a b" is not a valid host name`},
		{"an empty name", StatusRequest{Origin: ""}, nil, `origin "" is not a valid host name`},
		{"settings hosts unreadable", StatusRequest{Origin: "halo"}, func(f *fixture) { f.hosts = nil }, "the settings hosts cannot be read (no settings-hosts probe)"},
		{"no host name", StatusRequest{Origin: "halo"}, func(f *fixture) { f.self = nil }, "this host's name cannot be read (no host-name probe)"},
		{"a negative since", StatusRequest{Origin: "halo", Since: &neg}, nil, "--since -1 is below 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := mbpFixture(t)
			if c.mark != nil {
				c.mark(f)
			}
			code, stdout, stderr := f.status(c.req)
			if code != 1 || stdout != "" || stderr != statusPrefix+c.want+"\n" {
				t.Errorf("got %d %q %q, want 1, no stdout, %q", code, stdout, stderr, statusPrefix+c.want+"\n")
			}
		})
	}
}

func TestStatusServesTheOriginsReplies(t *testing.T) {
	f := mbpFixture(t)
	f.origins[f.commons[0]] = "git@github.com:owner/alpha.git"
	const acked, misrouted = 1700000000100, 1700000000200
	appendRows(t, f.commons[0],
		reply(acked, "dispatcher:c1", "overseer:halo", `{"kind":"ack","re":7}`),
		reply(misrouted, "worker:feat/x#s1", "overseer:halo", `{"kind":"ack"}`),
	)

	code, stdout, stderr := f.status(StatusRequest{Origin: "halo"})
	if code != 0 || stderr != "" {
		t.Fatalf("got %d %q", code, stderr)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Errorf("stdout is not one JSON line: %q", stdout)
	}
	var ans fleet.FarAnswer
	if err := json.Unmarshal([]byte(stdout), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.V != 1 || ans.Self != "mbp" || ans.Cursor != misrouted {
		t.Errorf("v/self/cursor = %d/%q/%d", ans.V, ans.Self, ans.Cursor)
	}
	if len(ans.Inbox) != 1 || ans.Inbox[0].Repo != "owner/alpha" || len(ans.Inbox[0].Events) != 1 || ans.Inbox[0].Events[0]["from"] != "dispatcher:c1" {
		t.Errorf("inbox = %+v, want the dispatcher's reply only", ans.Inbox)
	}
	if want := []fleet.FarMisrouted{{Repo: "owner/alpha", Common: f.commons[0], CrewID: "c1", From: "worker:feat/x#s1", TS: misrouted}}; !slices.Equal(ans.Misrouted, want) {
		t.Errorf("misrouted = %+v, want %+v", ans.Misrouted, want)
	}
	if len(ans.Status.Repos) != 1 || len(ans.Status.Repos[0].Crews) != 1 {
		t.Fatalf("status repos = %+v", ans.Status.Repos)
	}
	inbox := ans.Status.Repos[0].Crews[0].OverseerInbox
	if len(inbox) != 1 || inbox[0].TS != acked || inbox[0].Re == nil || *inbox[0].Re != 7 {
		t.Errorf("c1 overseer_inbox = %+v, want the ack of 7", inbox)
	}

	since := int64(acked)
	code, stdout, stderr = f.status(StatusRequest{Origin: "halo", Since: &since})
	if code != 0 || stderr != "" {
		t.Fatalf("with --since: got %d %q", code, stderr)
	}
	ans = fleet.FarAnswer{}
	if err := json.Unmarshal([]byte(stdout), &ans); err != nil {
		t.Fatal(err)
	}
	if len(ans.Inbox) != 0 || len(ans.Misrouted) != 1 {
		t.Errorf("with --since at the ack: inbox %+v misrouted %+v, want only the misroute", ans.Inbox, ans.Misrouted)
	}
}

// A far answer that cannot advance is still one JSON answer, carrying the fixed
// refusal code on stdout so the requester can word it locally; the exit status
// stays non-zero and the human reason stays on stderr. The refusal here is the
// one the trim leaves in place: more rows at one ts than an answer may carry,
// never the oversized overseer_inbox the trim removes.
func TestStatusRefusalCodeOnStdout(t *testing.T) {
	f := mbpFixture(t)
	const ts = 1700000000100
	rows := make([]string, 0, fleet.FarMaxEvents+1)
	for range fleet.FarMaxEvents + 1 {
		rows = append(rows, reply(ts, "dispatcher:c1", "overseer:halo", `{"kind":"ack"}`))
	}
	appendRows(t, f.commons[0], rows...)

	code, stdout, stderr := f.status(StatusRequest{Origin: "halo"})
	if code == 0 {
		t.Fatalf("got exit 0 and %q, want a non-zero refusal", stdout)
	}
	if !strings.HasPrefix(stderr, statusPrefix) || !strings.Contains(stderr, "answer cannot advance") {
		t.Errorf("stderr = %q, want the human reason", stderr)
	}
	if strings.Contains(stderr, fleet.RefuseTSGroupOverCap) {
		t.Errorf("stderr = %q, want no code on stderr", stderr)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Errorf("stdout is not one JSON line: %q", stdout)
	}
	var ans struct {
		V      int    `json:"v"`
		Self   string `json:"self"`
		TS     *int64 `json:"ts"`
		Refuse string `json:"refuse"`
	}
	if err := json.Unmarshal([]byte(stdout), &ans); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, stdout)
	}
	if ans.Refuse != fleet.RefuseTSGroupOverCap {
		t.Errorf("refuse = %q, want %q", ans.Refuse, fleet.RefuseTSGroupOverCap)
	}
	if ans.V != 1 || ans.Self != "mbp" || ans.TS == nil {
		t.Errorf("v/self/ts = %d/%q/%v, want 1/mbp/a whole ms", ans.V, ans.Self, ans.TS)
	}
}

// TestRoundTrip is one ask from halo's overseer to mbp's dispatcher and the
// reply back: RunMsg --host relays through RunDeliver on mbp, and halo's fleet
// status pulls mbp's reply through RunStatus.
func TestRoundTrip(t *testing.T) {
	mbp := mbpFixture(t)
	mbp.origins[mbp.commons[0]] = "git@github.com:owner/alpha.git"
	halo := newFixture(t)
	halo.host("halo", "mbp")
	halo.env["CREW_OVERSEER"] = "halo"

	halo.runHost = func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		if host != "mbp" || len(argv) != 9 || !slices.Equal(argv[:4], []string{"crew", "overseer", "deliver", "--origin"}) || argv[5] != "--" {
			t.Errorf("run on %s: %q", host, argv)
			return 2
		}
		return RunDeliver(ctx, DeliverRequest{Origin: argv[4], Repo: argv[6], Crew: argv[7], Body: argv[8]}, stdout, stderr, mbp.opts())
	}
	ask := `{"kind":"ask","detail":"capacity?"}`
	code, stdout, stderr := halo.msg(MsgRequest{Repo: "owner/alpha", Crew: "c1", Body: ask, Host: "mbp"})
	if code != 0 || stderr != "" || stdout != `{"host":"mbp","ts":`+tsText+`}`+"\n" {
		t.Fatalf("msg --host: got %d %q %q", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(mbp.logs()[mbp.commons[0]], "\n"), "\n")
	if got, want := lines[len(lines)-1], rowFrom("halo", "c1", ask); got != want {
		t.Fatalf("mbp's last row %q, want %q", got, want)
	}

	re, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	appendRows(t, mbp.commons[0], reply(re+100, "dispatcher:c1", "overseer:halo", `{"re":`+tsText+`,"kind":"ack"}`))

	so := halo.opts().Status
	so.Federate = func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		if host != "mbp" || len(argv) != 7 || !slices.Equal(argv[:4], []string{"crew", "overseer", "status", "--origin"}) || argv[5] != "--since" {
			t.Errorf("pull from %s: %q", host, argv)
			return 2
		}
		since, err := strconv.ParseInt(argv[6], 10, 64)
		if err != nil {
			t.Errorf("--since %q: %v", argv[6], err)
			return 2
		}
		return RunStatus(ctx, StatusRequest{Origin: argv[4], Since: &since}, stdout, stderr, mbp.opts())
	}
	var collectErr bytes.Buffer
	doc := fleet.CollectStatus(t.Context(), fleet.StatusRequest{}, &collectErr, so)
	if len(doc.Hosts) != 1 || doc.Hosts[0].Host != "mbp" || doc.Hosts[0].State != "ok" {
		t.Fatalf("hosts = %+v (stderr %q)", doc.Hosts, collectErr.String())
	}
	h := doc.Hosts[0]
	var inbox []fleet.InboxMsg
	for _, r := range h.Repos {
		for _, c := range r.Crews {
			if c.CrewID == "c1" {
				inbox = c.OverseerInbox
			}
		}
	}
	if len(inbox) != 1 || inbox[0].Re == nil || *inbox[0].Re != re || inbox[0].TS != re+100 {
		t.Errorf("mbp c1 overseer_inbox = %+v, want the ack with re %d", inbox, re)
	}
	b := h.Batches()
	if len(b) != 1 || len(b[0].Events) != 1 || b[0].Events[0]["from"] != "dispatcher:c1" || b[0].Events[0]["to"] != "overseer:halo" {
		t.Errorf("batches = %+v, want mbp's reply", b)
	}
}
