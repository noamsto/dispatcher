package overseer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	tsText = `1700000000500`
	seed   = `{"ts":1,"crew_id":"c1","from":"worker:feat/x#s1","kind":"status","body":{"state":"working"}}`
	notice = `{"kind":"notice","detail":"x"}`
	prefix = "crew: overseer msg: "
)

// fixture is a machine of fake repos: each is a temp dir whose .git/crew holds
// a log, found through a registry file, with the sweep root pointed at an
// empty dir so nothing walks $HOME.
type fixture struct {
	t        *testing.T
	root     string
	sweep    string
	registry string
	env      map[string]string
	self     func() (string, error)
	origins  map[string]string
	commons  []string
	git      *gittest.Fake
	hosts    map[string]remote.Host // settings hosts; nil: no hosts probe
	runHost  roster.Capture
	tmux     *tmuxtest.Fake
	served   string // the far verbs' overseer root; "" serves the registry alone
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:        t,
		root:     root,
		sweep:    root + "/sweep",
		registry: root + "/registry",
		env:      map[string]string{"CREW_OVERSEER": "h1"},
		self:     func() (string, error) { return "h1", nil },
		origins:  map[string]string{},
		tmux:     &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": ""}},
	}
	if err := os.Mkdir(f.sweep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/loadavg", []byte("0.50 0 0 1/1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// host makes f the machine named self whose settings hosts are self and
// partners.
func (f *fixture) host(self string, partners ...string) {
	f.self = func() (string, error) { return self, nil }
	f.hosts = map[string]remote.Host{self: {}}
	for _, p := range partners {
		f.hosts[p] = remote.Host{}
	}
}

// repo adds <root>/<name> with a log holding a c1 row and returns its common dir.
func (f *fixture) repo(name, origin string) string {
	f.t.Helper()
	common := f.root + "/" + name + "/.git"
	if err := os.MkdirAll(common+"/crew", 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(common+"/crew/events.jsonl", []byte(seed+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if origin != "" {
		f.origins[common] = origin
	}
	f.register(common)
	return common
}

// noBus adds a registered .git dir that has no crew log.
func (f *fixture) noBus(name string) string {
	f.t.Helper()
	common := f.root + "/" + name + "/.git"
	if err := os.MkdirAll(common, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.register(common)
	return common
}

func (f *fixture) register(common string) {
	f.t.Helper()
	f.commons = append(f.commons, common)
	if err := os.WriteFile(f.registry, []byte(strings.Join(f.commons, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) opts() Options {
	answers := map[string]string{}
	for common, url := range f.origins {
		answers["--git-dir="+common+" config --get remote.origin.url"] = url
	}
	f.git = &gittest.Fake{Out: answers}
	var hosts func() (map[string]remote.Host, error)
	if f.hosts != nil {
		hosts = func() (map[string]remote.Host, error) { return f.hosts, nil }
	}
	clk := clock.Clock{Now: func() time.Time { return time.UnixMilli(1700000000500) }}
	probes := sender.Probes{
		Getenv: func(k string) string { return f.env[k] },
		Tmux:   f.tmux,
		Proc:   proctest.New(nil, map[int]int{os.Getpid(): 1}),
		Self:   f.self,
		Hosts:  hosts,
	}
	return Options{
		Getenv: func(k string) string {
			if k == "CREW_SWEEP_ROOTS" {
				return f.sweep
			}
			return ""
		},
		Registry: f.registry,
		Git:      f.git,
		Clock:    clk,
		Sender:   probes,
		RunHost:  f.runHost,
		Status:   f.statusOpts(clk, probes),
		ServedRoot: func(context.Context) (string, error) {
			return f.served, nil
		},
	}
}

// unregistered adds <parent>/<name> with a log holding a c1 row, outside the
// registry, and returns its common dir.
func (f *fixture) unregistered(parent, name string) string {
	f.t.Helper()
	common := parent + "/" + name + "/.git"
	if err := os.MkdirAll(common+"/crew", 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(common+"/crew/events.jsonl", []byte(seed+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return common
}

// statusOpts are the fleet status reads of this machine: its registry, a
// state dir under root, no live windows and a fixed machine document.
func (f *fixture) statusOpts(clk clock.Clock, probes sender.Probes) fleet.StatusOptions {
	env := map[string]string{
		"HOME":                  f.sweep,
		"CREW_SWEEP_ROOTS":      f.sweep,
		"XDG_STATE_HOME":        f.root + "/state",
		"DISPATCH_LOADAVG_FILE": f.root + "/loadavg",
	}
	maps.Copy(env, f.env)
	return fleet.StatusOptions{
		Options: fleet.Options{
			Getenv:   func(k string) string { return env[k] },
			Registry: f.registry,
			Tmux:     &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": ""}},
			Git: &gittest.Fake{Func: func(_ string, args []string) (string, error) {
				if args[len(args)-1] == "--porcelain" {
					return "", nil
				}
				if url := f.origins[strings.TrimPrefix(args[0], "--git-dir=")]; url != "" && args[len(args)-1] == "remote.origin.url" {
					return url, nil
				}
				return "", &git.ExitError{Code: 1}
			}},
			Clock: clk,
		},
		Proc:   proctest.New(nil, nil),
		Host:   f.self,
		Budget: func(context.Context) (int, error) { return 8, nil },
		Cmd: func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "nproc" {
				return "4", nil
			}
			return "", errors.New("no such command")
		},
		BudgetFile: f.root + "/engine-budget.json",
		Sender:     probes,
	}
}

// run is one local `crew overseer msg`, which prints nothing on stdout.
func (f *fixture) run(repo, crew, body string) (int, string) {
	f.t.Helper()
	code, stdout, stderr := f.msg(MsgRequest{Repo: repo, Crew: crew, Body: body})
	if stdout != "" {
		f.t.Errorf("the local path printed %q", stdout)
	}
	return code, stderr
}

func (f *fixture) msg(req MsgRequest) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunMsg(f.t.Context(), req, &stdout, &stderr, f.opts())
	return code, stdout.String(), stderr.String()
}

// logs is every registered repo's log text, by common dir.
func (f *fixture) logs() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, common := range f.commons {
		data, err := os.ReadFile(common + "/crew/events.jsonl")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			f.t.Fatal(err)
		}
		out[common] = string(data)
	}
	return out
}

// refused runs one request that must be refused with want and leave every log
// byte-identical.
func (f *fixture) refused(repo, crew, body, want string) {
	f.t.Helper()
	before := f.logs()
	code, stderr := f.run(repo, crew, body)
	if code != 1 || stderr != want+"\n" {
		f.t.Errorf("got %d %q, want 1 %q", code, stderr, want+"\n")
	}
	if after := f.logs(); !maps.Equal(after, before) {
		f.t.Errorf("a refused message changed a log:\nbefore %q\nafter  %q", before, after)
	}
}

// posted runs one request that must succeed and returns the last line of the
// log at common.
func (f *fixture) posted(repo, crew, body, common string) string {
	f.t.Helper()
	if code, stderr := f.run(repo, crew, body); code != 0 || stderr != "" {
		f.t.Fatalf("got %d %q, want 0 with no stderr", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(f.logs()[common], "\n"), "\n")
	return lines[len(lines)-1]
}

func rowFor(crew, body string) string { return rowFrom("h1", crew, body) }

// rowFrom is the row overseer:<host> posts to dispatcher:<crew>.
func rowFrom(host, crew, body string) string {
	return `{"ts":` + tsText + `,"crew_id":"` + crew + `","from":"overseer:` + host + `","to":"dispatcher:` + crew + `","kind":"msg","body":` + jsonString(body) + `}`
}

// jsonString is body as the row encodes it: quotes and backslashes escaped.
func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func TestMsgWritesTheRowByRepoNameAndByCommonDir(t *testing.T) {
	f := newFixture(t)
	alpha := f.repo("alpha", "git@github.com:owner/alpha.git")
	beta := f.repo("beta", "git@github.com:owner/beta.git")
	betaBefore := f.logs()[beta]

	if got, want := f.posted("owner/alpha", "c1", notice, alpha), rowFor("c1", notice); got != want {
		t.Errorf("by name: last line %q, want %q", got, want)
	}
	if got, want := f.posted(alpha, "c1", notice, alpha), rowFor("c1", notice); got != want {
		t.Errorf("by common dir: last line %q, want %q", got, want)
	}
	if n := strings.Count(f.logs()[alpha], "\n"); n != 3 {
		t.Errorf("alpha log has %d rows, want the seed and two posts", n)
	}
	if f.logs()[beta] != betaBefore {
		t.Error("a message to alpha changed beta's log")
	}
}

func TestMsgAddressesARepoByTheLabelItShows(t *testing.T) {
	f := newFixture(t)
	spaced := f.repo("spaced", "git@github.com:owner/my repo+x.git")

	if got, want := f.posted("owner/my?repo?x", "c1", notice, spaced), rowFor("c1", notice); got != want {
		t.Errorf("by label: last line %q, want %q", got, want)
	}
}

func TestMsgNamesARepoWithoutAnOriginByItsDirectory(t *testing.T) {
	f := newFixture(t)
	plain := f.repo("plain", "")
	if got, want := f.posted("plain", "c1", notice, plain), rowFor("c1", notice); got != want {
		t.Errorf("last line %q, want %q", got, want)
	}
}

func TestMsgPostsToACrewThatOnlyHasADirectory(t *testing.T) {
	f := newFixture(t)
	alpha := f.repo("alpha", "")
	if err := os.MkdirAll(alpha+"/crew/crews/c9", 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := f.posted("alpha", "c9", notice, alpha), rowFor("c9", notice); got != want {
		t.Errorf("last line %q, want %q", got, want)
	}
}

func TestMsgAcceptsEveryAllowedKind(t *testing.T) {
	for _, kind := range []string{"ask", "request", "capacity", "notice"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			alpha := f.repo("alpha", "")
			body := `{"kind":"` + kind + `","detail":"x"}`
			if got, want := f.posted("alpha", "c1", body, alpha), rowFor("c1", body); got != want {
				t.Errorf("last line %q, want %q", got, want)
			}
		})
	}
}

func TestMsgAcceptsABodyAtTheByteLimit(t *testing.T) {
	f := newFixture(t)
	alpha := f.repo("alpha", "")
	const head, tail = `{"kind":"notice","detail":"`, `"}`
	body := head + strings.Repeat("x", peer.MaxBody-len(head)-len(tail)) + tail
	if len(body) != peer.MaxBody {
		t.Fatalf("fixture body is %d bytes, want %d", len(body), peer.MaxBody)
	}
	if got, want := f.posted("alpha", "c1", body, alpha), rowFor("c1", body); got != want {
		t.Errorf("last line is %d bytes, want the exact row (%d bytes)", len(got), len(want))
	}
}

func TestMsgRefusesAnUnmarkedOrMismatchedSession(t *testing.T) {
	cases := []struct {
		name string
		mark func(f *fixture)
	}{
		{"no CREW_OVERSEER", func(f *fixture) { delete(f.env, "CREW_OVERSEER") }},
		{"CREW_OVERSEER names another host", func(f *fixture) { f.env["CREW_OVERSEER"] = "h2" }},
		{"a worker id beside the marker", func(f *fixture) { f.env["CREW_WORKER_ID"] = "worker:feat/x" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.repo("alpha", "")
			c.mark(f)
			reason := sender.Check(t.Context(), "overseer:h1", "c1", f.opts().Sender)
			if reason == "" {
				t.Fatal("fixture does not trip the sender check")
			}
			f.refused("alpha", "c1", notice, prefix+"refusing sender 'overseer:h1' — "+reason)
		})
	}
}

func TestMsgRefusesWhenTheHostNameCannotBeRead(t *testing.T) {
	t.Run("the probe fails", func(t *testing.T) {
		f := newFixture(t)
		f.repo("alpha", "")
		f.self = func() (string, error) { return "", errors.New("no hostname") }
		f.refused("alpha", "c1", notice, prefix+"this host's name cannot be read (no hostname)")
	})
	t.Run("there is no probe", func(t *testing.T) {
		f := newFixture(t)
		f.repo("alpha", "")
		f.self = nil
		f.refused("alpha", "c1", notice, prefix+"this host's name cannot be read (no host-name probe)")
	})
}

func TestMsgChecksTheSenderBeforeReadingAnyRepo(t *testing.T) {
	f := newFixture(t)
	f.repo("alpha", "")
	delete(f.env, "CREW_OVERSEER")
	reason := sender.Check(t.Context(), "overseer:h1", "c1", f.opts().Sender)
	f.refused("nowhere/unknown", "c1", notice, prefix+"refusing sender 'overseer:h1' — "+reason)
	if calls := f.git.Calls(); len(calls) != 0 {
		t.Errorf("a refused sender still read repos: %q", calls)
	}
}

func TestMsgRefusesARepoItCannotUse(t *testing.T) {
	f := newFixture(t)
	f.repo("alpha", "git@github.com:owner/alpha.git")
	bare := f.noBus("bare")
	gone := f.root + "/gone/.git"
	f.register(gone)

	unknown := func(repo string) string {
		return prefix + "repo " + repo + " is not a discovered crew bus; pass a repo as crew fleet names it, or its git common dir"
	}
	f.refused("owner/unknown", "c1", notice, unknown("owner/unknown"))
	f.refused("bare", "c1", notice, unknown("bare"))
	f.refused(bare, "c1", notice, prefix+"repo "+bare+" is not a usable crew bus (no bus)")
	f.refused(gone, "c1", notice, prefix+"repo "+gone+" is not a usable crew bus (gone)")
	f.refused(f.root+"/alpha", "c1", notice, unknown(f.root+"/alpha"))
}

func TestMsgRefusesAnAmbiguousRepoName(t *testing.T) {
	f := newFixture(t)
	one := f.repo("one", "git@github.com:owner/same.git")
	two := f.repo("two", "https://github.com/owner/same")
	f.repo("other", "git@github.com:owner/other.git")

	f.refused("owner/same", "c1", notice, prefix+"repo owner/same is ambiguous ("+one+", "+two+"); pass the git common dir")

	if got, want := f.posted(one, "c1", notice, one), rowFor("c1", notice); got != want {
		t.Errorf("by first common dir: last line %q, want %q", got, want)
	}
	if got, want := f.posted(two, "c1", notice, two), rowFor("c1", notice); got != want {
		t.Errorf("by second common dir: last line %q, want %q", got, want)
	}
}

func TestMsgRefusesACrewNotOnTheBus(t *testing.T) {
	f := newFixture(t)
	alpha := f.repo("alpha", "")
	f.repo("beta", "")
	if err := os.WriteFile(f.commons[1]+"/crew/events.jsonl", []byte(strings.Replace(seed, "c1", "c2", 1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, crew := range []string{"c2", "c3", "../x", "-c1"} {
		f.refused("alpha", crew, notice, prefix+"crew "+crew+" is not on "+alpha+"'s bus")
	}
}

func TestMsgRefusesABodyItWillNotPost(t *testing.T) {
	const noControl = "body has a control or invisible character (escaped ones too); send compact one-line JSON, e.g. jq -c"
	const badKind = "body kind must be one of ask|request|capacity|notice"
	long := `{"kind":"notice","detail":"` + strings.Repeat("x", peer.MaxBody) + `"}`

	cases := []struct{ name, body, want string }{
		{"not JSON", `{"kind":`, "body is not valid JSON"},
		{"two JSON texts", notice + notice, "body is not valid JSON"},
		{"a JSON array", `[` + notice + `]`, "body must be a JSON object"},
		{"a JSON string", `"notice"`, "body must be a JSON object"},
		{"a JSON null", `null`, "body must be a JSON object"},
		{"no kind", `{"detail":"x"}`, badKind},
		{"a numeric kind", `{"kind":1}`, badKind},
		{"a null kind", `{"kind":null}`, badKind},
		{"an object kind", `{"kind":{"a":"b"}}`, badKind},
		{"a kind outside the set", `{"kind":"directive"}`, badKind + `, got "directive"`},
		{"a kind in another case", `{"kind":"Notice"}`, badKind + `, got "Notice"`},
		{"over the byte limit", long, fmt.Sprintf("body is %d bytes, over the %d-byte limit", len(long), peer.MaxBody)},
		{"invalid UTF-8", "{\"kind\":\"notice\",\"detail\":\"\xff\"}", "body is not valid UTF-8"},
		{"a raw newline between tokens", "{\"kind\":\"notice\",\n\"detail\":\"x\"}", noControl},
		{"a raw tab between tokens", "{\"kind\":\"notice\",\t\"detail\":\"x\"}", noControl},
		{"a raw DEL", "{\"kind\":\"notice\",\"detail\":\"a\x7fb\"}", noControl},
		{"a raw C1 control", "{\"kind\":\"notice\",\"detail\":\"a\u0085b\"}", noControl},
		{"a raw zero-width space", "{\"kind\":\"notice\",\"detail\":\"a\u200bb\"}", noControl},
		{"a raw byte order mark", "\ufeff" + notice, noControl},
		{"an escaped ESC in a value", `{"kind":"notice","detail":"a\u001bb"}`, noControl},
		{"an escaped newline in a value", `{"kind":"notice","detail":"a\nb"}`, noControl},
		{"an escaped DEL in a value", `{"kind":"notice","detail":"a\u007fb"}`, noControl},
		{"an escaped C1 control in a value", `{"kind":"notice","detail":"a\u0085b"}`, noControl},
		{"an escaped zero-width space in a value", `{"kind":"notice","detail":"a\u200bb"}`, noControl},
		{"an escaped control in a key", `{"kind":"notice","a\u001b":"x"}`, noControl},
		{"an escaped control in a nested key", `{"kind":"notice","a":{"b\u001b":"x"}}`, noControl},
		{"an escaped control in a nested array", `{"kind":"notice","a":[1,["x",{"b":"\u001b"}]]}`, noControl},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.repo("alpha", "")
			f.refused("alpha", "c1", c.body, prefix+c.want)
		})
	}
}

func TestMsgChecksTheBodyBeforeReadingAnyRepo(t *testing.T) {
	f := newFixture(t)
	f.repo("alpha", "")
	f.refused("nowhere/unknown", "c1", `{"kind":"directive"}`, prefix+`body kind must be one of ask|request|capacity|notice, got "directive"`)
	if calls := f.git.Calls(); len(calls) != 0 {
		t.Errorf("a refused body still named repos: %q", calls)
	}
}
