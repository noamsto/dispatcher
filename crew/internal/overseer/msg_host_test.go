package overseer

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

// farCall is one RunHost call the fake saw.
type farCall struct {
	host string
	argv []string
}

// far makes the fake RunHost answer every call with stdout, stderr and rc,
// and returns the calls it saw.
func (f *fixture) far(stdout, stderr string, rc int) *[]farCall {
	calls := &[]farCall{}
	f.runHost = func(_ context.Context, host string, argv []string, out, errw io.Writer) int {
		*calls = append(*calls, farCall{host, slices.Clone(argv)})
		_, _ = io.WriteString(out, stdout)
		_, _ = io.WriteString(errw, stderr)
		return rc
	}
	return calls
}

// haloFixture is the overseer session of halo, whose settings hosts are halo
// and mbp, with one repo alpha.
func haloFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.host("halo", "mbp")
	f.env["CREW_OVERSEER"] = "halo"
	f.repo("alpha", "")
	return f
}

// hostRefused runs a --host request that must be refused with want, call no
// far host and leave every local log byte-identical.
func (f *fixture) hostRefused(req MsgRequest, calls *[]farCall, want string) {
	f.t.Helper()
	before := f.logs()
	code, stdout, stderr := f.msg(req)
	if code != 1 || stdout != "" || stderr != want+"\n" {
		f.t.Errorf("got %d %q %q, want 1, no stdout, %q", code, stdout, stderr, want+"\n")
	}
	if calls != nil && len(*calls) != 0 {
		f.t.Errorf("a refused request still ran on the far host: %v", *calls)
	}
	if after := f.logs(); !maps.Equal(after, before) {
		f.t.Errorf("a --host request changed a local log:\nbefore %q\nafter  %q", before, after)
	}
}

func TestMsgHostSendsTheFixedArgvAndPrintsTheFarTS(t *testing.T) {
	f := haloFixture(t)
	calls := f.far(`{"ts":1700000000777}`+"\n", "", 0)
	before := f.logs()

	code, stdout, stderr := f.msg(MsgRequest{Repo: "owner/alpha", Crew: "c1", Body: notice, Host: "mbp"})
	if code != 0 || stderr != "" || stdout != `{"host":"mbp","ts":1700000000777}`+"\n" {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
	want := []farCall{{"mbp", []string{"crew", "overseer", "deliver", "--origin", "halo", "--", "owner/alpha", "c1", notice}}}
	if !slices.EqualFunc(*calls, want, func(a, b farCall) bool { return a.host == b.host && slices.Equal(a.argv, b.argv) }) {
		t.Errorf("calls = %q, want %q", *calls, want)
	}
	if !maps.Equal(f.logs(), before) {
		t.Error("a --host message changed a local log")
	}
}

func TestMsgHostSelfOrEmptyIsTheLocalPath(t *testing.T) {
	for _, host := range []string{"", "halo"} {
		t.Run("host="+host, func(t *testing.T) {
			f := haloFixture(t)
			calls := f.far(`{"ts":1}`, "", 0)
			code, stdout, stderr := f.msg(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: host})
			if code != 0 || stdout != "" || stderr != "" {
				t.Fatalf("got %d %q %q", code, stdout, stderr)
			}
			lines := strings.Split(strings.TrimSuffix(f.logs()[f.commons[0]], "\n"), "\n")
			if got, want := lines[len(lines)-1], rowFrom("halo", "c1", notice); got != want {
				t.Errorf("last line %q, want %q", got, want)
			}
			if len(*calls) != 0 {
				t.Errorf("the local path ran on a far host: %v", *calls)
			}
		})
	}
}

func TestMsgHostRefusesABodyBeforeAnyFarCall(t *testing.T) {
	long := `{"kind":"notice","detail":"` + strings.Repeat("x", peer.MaxBody) + `"}`
	cases := []struct{ name, body, want string }{
		{"over the byte limit", long, fmt.Sprintf("body is %d bytes, over the %d-byte limit", len(long), peer.MaxBody)},
		{"a raw control", "{\"kind\":\"notice\",\"detail\":\"a\x1bb\"}", reasonControl},
		{"a raw zero-width space", "{\"kind\":\"notice\",\"detail\":\"a\u200bb\"}", reasonControl},
		{"an escaped ESC", `{"kind":"notice","detail":"a\u001bb"}`, reasonControl},
		{"an escaped zero-width space", `{"kind":"notice","detail":"a\u200bb"}`, reasonControl},
		{"a kind outside the set", `{"kind":"directive"}`, `body kind must be one of ask|request|capacity|notice, got "directive"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := haloFixture(t)
			calls := f.far(`{"ts":1}`, "", 0)
			f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: c.body, Host: "mbp"}, calls, prefix+c.want)
		})
	}
}

func TestMsgHostRefusesANonOverseerSession(t *testing.T) {
	f := haloFixture(t)
	delete(f.env, "CREW_OVERSEER")
	calls := f.far(`{"ts":1}`, "", 0)
	reason := sender.Check(t.Context(), "overseer:halo", "c1", f.opts().Sender)
	if reason == "" {
		t.Fatal("fixture does not trip the sender check")
	}
	f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "mbp"}, calls, prefix+"refusing sender 'overseer:halo' — "+reason)
}

func TestMsgHostRefusesAHostOutsideSettings(t *testing.T) {
	t.Run("not a settings host", func(t *testing.T) {
		f := haloFixture(t)
		calls := f.far(`{"ts":1}`, "", 0)
		f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "evil"}, calls, prefix+"host evil is not in settings hosts")
	})
	t.Run("not a host name", func(t *testing.T) {
		f := haloFixture(t)
		calls := f.far(`{"ts":1}`, "", 0)
		f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "-mbp"}, calls, prefix+`host "-mbp" is not a valid host name`)
	})
	t.Run("no hosts probe", func(t *testing.T) {
		f := haloFixture(t)
		f.hosts = nil
		calls := f.far(`{"ts":1}`, "", 0)
		f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "mbp"}, calls, prefix+"the settings hosts cannot be read (no settings-hosts probe)")
	})
	t.Run("no remote runner", func(t *testing.T) {
		f := haloFixture(t)
		f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "mbp"}, nil, prefix+"--host mbp needs a remote runner, and none is wired")
	})
}

func TestMsgHostMapsAFarFailure(t *testing.T) {
	cases := []struct {
		name, stderr string
		rc           int
		want         string
	}{
		{"its first stderr line", "\ncrew: overseer deliver: refusing relay\nsecond line\n", 1, "crew: overseer deliver: refusing relay"},
		{"control and invisible chars stripped", "bad\x1b[31mthing\u200b\r\n", 1, "bad[31mthing"},
		{"no stderr", "", 3, "exit status 3"},
		{"only hidden chars", "\x1b\u200b\n", 255, "exit status 255"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := haloFixture(t)
			f.far(`{"ts":1}`, c.stderr, c.rc)
			f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "mbp"}, nil, prefix+"mbp: "+c.want)
		})
	}
}

func TestMsgHostRejectsAMalformedFarAnswer(t *testing.T) {
	for _, out := range []string{
		"",
		"ok",
		`{"ts":1}{"ts":2}`,
		`{"ts":1} x`,
		`[{"ts":1}]`,
		`null`,
		`{}`,
		`{"ts":"1"}`,
		`{"ts":1.5}`,
		`{"ts":1e3}`,
		`{"ts":-1}`,
		`{"ts":1,"x":2}`,
		`{"ts":1}` + strings.Repeat(" ", 4096),
	} {
		t.Run(fmt.Sprintf("%.20q", out), func(t *testing.T) {
			f := haloFixture(t)
			f.far(out, "", 0)
			f.hostRefused(MsgRequest{Repo: "alpha", Crew: "c1", Body: notice, Host: "mbp"}, nil, prefix+"mbp: malformed far answer")
		})
	}
}
