// Package overseer is the overseer session's verbs: `crew overseer msg`, and
// the `status` and `deliver` verbs a federated overseer runs on another host.
package overseer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/status"
)

// MsgUsage is the line every `crew overseer msg` grammar refusal prints.
const MsgUsage = "usage: crew overseer msg [--host <h>] <repo> <crew> '<json>'"

// StatusUsage and DeliverUsage are the grammar refusals of the far verbs.
const (
	StatusUsage  = "usage: crew overseer status --origin <host> [--since <ms>]"
	DeliverUsage = "usage: crew overseer deliver --origin <host> -- <repo> <crew> '<json>'"
)

const (
	exitFailure = 1
	// farAnswerMax bounds what a far deliver may print, on either stream.
	farAnswerMax = 4 << 10
)

var (
	kinds      = []string{"ask", "request", "capacity", "notice"}
	errNoSelf  = errors.New("no host-name probe")
	errNoHosts = errors.New("no settings-hosts probe")
)

// MsgRequest is `crew overseer msg [--host <h>] <repo> <crew> '<json>'`.
type MsgRequest struct{ Repo, Crew, Body, Host string }

// Options carry every outside read, so tests use temp dirs and stubs.
type Options struct {
	Getenv   func(string) string // repos.Discover's env (registry, CREW_SWEEP_ROOTS, HOME); nil: os.Getenv
	Registry string              // repos.Options.Registry
	Git      git.Client          // names a discovered repo from its origin
	Clock    clock.Clock
	Sender   sender.Probes
	// RunHost runs a verb on another host, the client side of --host; nil
	// refuses --host.
	RunHost roster.Capture
	// Status is the far `status` verb's fleet status options; ServeFar sets
	// served mode itself.
	Status fleet.StatusOptions
}

// RunMsg posts the overseer's JSON message to the dispatcher of <crew> on the
// bus of <repo>: a repo named as crew fleet names it, or its git common dir.
// With Host another host's name, the far host's `crew overseer deliver` posts
// it there instead, and RunMsg prints the far row's ts. The sender and body
// checks run first, so no repo is read and no host asked for a message that
// may not be posted. Every refusal prints one line and writes nothing.
func RunMsg(ctx context.Context, req MsgRequest, stdout, stderr io.Writer, o Options) int {
	self, err := hostName(o.Sender)
	if err != nil {
		return refuse(stderr, "this host's name cannot be read (%v)", err)
	}
	// The raw crew goes to the check on purpose: no overseer rule reads it.
	from := "overseer:" + self
	if reason := sender.Check(ctx, from, req.Crew, o.Sender); reason != "" {
		return refuse(stderr, "refusing sender '%s' — %s", from, reason)
	}
	if reason := bodyRefusal(req.Body); reason != "" {
		return refuse(stderr, "%s", reason)
	}
	if req.Host != "" && req.Host != self {
		return relay(ctx, req, self, stdout, stderr, o)
	}
	paths, reason := busFor(ctx, req.Repo, req.Crew, "msg", o, stderr)
	if reason != "" {
		return refuse(stderr, "%s", reason)
	}
	return status.RunOverseerMsg(ctx, status.OverseerMsgRequest{Self: self, Crew: req.Crew, Body: req.Body}, paths, stderr, status.Options{Clock: o.Clock, Sender: o.Sender})
}

// relay asks req.Host's `crew overseer deliver` to post the message from
// overseer:<self>, and prints {"host","ts"} from its answer. The far host's
// output is untrusted: both streams are capped, a failure shows only its first
// stderr line stripped of hidden characters, and stdout must be exactly
// {"ts":<whole ms>}.
func relay(ctx context.Context, req MsgRequest, self string, stdout, stderr io.Writer, o Options) int {
	if reason := settingsHost("host", req.Host, o.Sender); reason != "" {
		return refuse(stderr, "%s", reason)
	}
	if o.RunHost == nil {
		return refuse(stderr, "--host %s needs a remote runner, and none is wired", req.Host)
	}
	out, errOut := &capped{max: farAnswerMax}, &capped{max: farAnswerMax}
	argv := []string{"crew", "overseer", "deliver", "--origin", self, "--", req.Repo, req.Crew, req.Body}
	if rc := o.RunHost(ctx, req.Host, argv, out, errOut); rc != 0 {
		return refuse(stderr, "%s: %s", req.Host, farFailure(errOut.buf.String(), rc))
	}
	ts, ok := farTS(out)
	if !ok {
		return refuse(stderr, "%s: malformed far answer", req.Host)
	}
	return emit(stdout, stderr, "msg", struct {
		Host string `json:"host"`
		TS   int64  `json:"ts"`
	}{req.Host, ts})
}

// farFailure is why a far deliver failed: the first line of its stderr with
// every control and invisible character dropped, else its status.
func farFailure(stderr string, rc int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	line = strings.TrimSpace(strings.Map(func(r rune) rune {
		if control(r) {
			return -1
		}
		return r
	}, invisible.Drop(line)))
	if line == "" {
		return fmt.Sprintf("exit status %d", rc)
	}
	return line
}

// farTS is the ts of a far deliver's answer, which must be one JSON object
// holding only a whole, non-negative ms.
func farTS(out *capped) (int64, bool) {
	if out.over {
		return 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(out.buf.Bytes()))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || len(obj) != 1 {
		return 0, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, false
	}
	n, ok := obj["ts"].(json.Number)
	if !ok {
		return 0, false
	}
	ts, err := strconv.ParseInt(n.String(), 10, 64)
	return ts, err == nil && ts >= 0
}

// capped keeps the first max bytes written to it and notes that more came. It
// never fails a write, so the far command is not cut off mid-answer.
type capped struct {
	max  int
	buf  bytes.Buffer
	over bool
}

func (w *capped) Write(p []byte) (int, error) {
	keep := p
	if room := w.max - w.buf.Len(); len(p) > room {
		w.over = true
		keep = p[:max(room, 0)]
	}
	w.buf.Write(keep)
	return len(p), nil
}

// emit prints v as one JSON line.
func emit(stdout, stderr io.Writer, verb string, v any) int {
	var buf bytes.Buffer
	if err := jsonval.Encode(&buf, v, false); err != nil {
		return fail(stderr, verb, "%v", err)
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		return fail(stderr, verb, "%v", err)
	}
	return 0
}

// settingsHost is why name, as role, is not a settings host, "" when it is.
func settingsHost(role, name string, p sender.Probes) string {
	if reason := badName(role, name); reason != "" {
		return reason
	}
	if p.Hosts == nil {
		return "the settings hosts cannot be read (" + errNoHosts.Error() + ")"
	}
	hosts, err := p.Hosts()
	if err != nil {
		return "the settings hosts cannot be read (" + err.Error() + ")"
	}
	if _, ok := hosts[name]; !ok {
		return role + " " + name + " is not in settings hosts"
	}
	return ""
}

// badName is why name, as role, is not a host name, "" when it is.
func badName(role, name string) string {
	if remote.HostNameRe.MatchString(name) {
		return ""
	}
	return fmt.Sprintf("%s %q is not a valid host name", role, name)
}

func refuse(stderr io.Writer, format string, args ...any) int {
	return fail(stderr, "msg", format, args...)
}

// fail prints one `crew: overseer <verb>:` refusal line.
func fail(stderr io.Writer, verb, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "crew: overseer "+verb+": "+format+"\n", args...)
	return exitFailure
}

func hostName(p sender.Probes) (string, error) {
	if p.Self == nil {
		return "", errNoSelf
	}
	return p.Self()
}

const reasonControl = "body has a control or invisible character (escaped ones too); send compact one-line JSON, e.g. jq -c"

// bodyRefusal is why body is not a message the overseer may post, "" when it
// is: one JSON object of an allowed kind, as one line of visible text.
func bodyRefusal(body string) string {
	if len(body) > peer.MaxBody {
		return fmt.Sprintf("body is %d bytes, over the %d-byte limit", len(body), peer.MaxBody)
	}
	if !utf8.ValidString(body) {
		return "body is not valid UTF-8"
	}
	if hidden(body) {
		return reasonControl
	}
	v, err := jqrun.FromJSON(body)
	if err != nil {
		return "body is not valid JSON"
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return "body must be a JSON object"
	}
	// Decoding unescapes, so \u001b and \n only show up here.
	if hiddenIn(obj) {
		return reasonControl
	}
	kind, isString := obj["kind"].(string)
	if slices.Contains(kinds, kind) {
		return ""
	}
	want := "body kind must be one of " + strings.Join(kinds, "|")
	if isString {
		return fmt.Sprintf("%s, got %q", want, kind)
	}
	return want
}

// hidden is the peer channel's test for text a terminal hides or a model
// misreads: C0, DEL and C1 controls, plus the invisible format characters.
func hidden(s string) bool {
	return invisible.Has(s) || strings.IndexFunc(s, control) >= 0
}

func control(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

func hiddenIn(v any) bool {
	switch x := v.(type) {
	case string:
		return hidden(x)
	case map[string]any:
		for k, item := range x {
			if hidden(k) || hiddenIn(item) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(x, hiddenIn)
	}
	return false
}

// busFor is the bus of repo that crew is on, else the refusal reason.
func busFor(ctx context.Context, repo, crew, verb string, o Options, stderr io.Writer) (bus.Paths, string) {
	common, reason := findBus(ctx, repo, verb, o, stderr)
	if reason != "" {
		return bus.Paths{}, reason
	}
	paths := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	if !bus.ValidCrewID(crew) || !slices.Contains(crews.IDs(paths), crew) {
		return bus.Paths{}, fmt.Sprintf("crew %s is not on %s's bus", crew, common)
	}
	return paths, ""
}

// findBus resolves repo to the git common dir of a discovered repo with a bus,
// else returns the refusal reason. An exact common dir wins; otherwise repo is
// matched as the name crew fleet gives it, which must pick one repo.
func findBus(ctx context.Context, repo, verb string, o Options, stderr io.Writer) (string, string) {
	found := repos.Discover(repos.Options{Registry: o.Registry, Getenv: o.Getenv, Stderr: stderr, Prefix: "crew: overseer " + verb})
	for _, r := range found {
		if r.Common != repo {
			continue
		}
		if r.Status != repos.StatusOK {
			return "", fmt.Sprintf("repo %s is not a usable crew bus (%s)", repo, r.Status)
		}
		return r.Common, ""
	}
	var named []string
	for _, r := range found {
		if r.Status != repos.StatusOK {
			continue
		}
		origin, _ := git.Origin(ctx, o.Git, r.Common)
		if repos.Label(r.Common, origin) == repo {
			named = append(named, r.Common)
		}
	}
	switch len(named) {
	case 0:
		return "", "repo " + repo + " is not a discovered crew bus; pass a repo as crew fleet names it, or its git common dir"
	case 1:
		return named[0], ""
	}
	return "", fmt.Sprintf("repo %s is ambiguous (%s); pass the git common dir", repo, strings.Join(named, ", "))
}
