// Package policy is the repo owner's standing per-repo policy: words written
// ahead of time from the owner's own terminal, which a dispatcher reads with
// their provenance when no human is present.
//
// The write path is a tripwire, not authentication: every session runs as the
// same uid. The record lives under the user's state dir, outside the git common
// dir, the worktrees and every --add-dir root, so no worker grant reaches it,
// and a read refuses any record that the write path would not have produced.
package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

// Record is one standing policy file.
type Record struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Repo string `json:"repo"`
	By   string `json:"by"`
	Host string `json:"host"`
	UID  int    `json:"uid"`
	TTY  string `json:"tty"`
	At   string `json:"at"`
}

// Options are the probes the policy reads; DefaultOptions is the live set.
type Options struct {
	Getenv   func(string) string
	IsTTY    func() bool
	TTYName  func() string
	User     func() (string, error)
	Hostname func() (string, error)
	Now      func() time.Time
	UID      func() int
	Sender   sender.Probes
}

// DefaultOptions reads the live process; Sender is left zero for the caller to
// fill with the probes merge and nudge get.
func DefaultOptions() Options {
	return Options{
		Getenv:  os.Getenv,
		IsTTY:   func() bool { return bus.IsTerminal(0) },
		TTYName: ttyName,
		User: func() (string, error) {
			u, err := user.Current()
			if err != nil {
				return "", err
			}
			return u.Username, nil
		},
		Hostname: os.Hostname,
		Now:      func() time.Time { return time.Now().UTC() },
		UID:      os.Getuid,
	}
}

func ttyName() string {
	if runtime.GOOS != "linux" {
		return "tty"
	}
	t, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		return "tty"
	}
	return t
}

const (
	// Usage is what a bare or unknown action prints.
	Usage = "crew: policy set|clear|show|list"
	// SetUsage and its siblings are the action's line for a grammar error.
	SetUsage   = "crew: policy set <name> <text>"
	ClearUsage = "crew: policy clear <name>"
	ShowUsage  = "crew: policy show <name> [--json]"
	ListUsage  = "crew: policy list [--json | --brief]"
)

// ErrMissing is wrapped by Load and Clear when the policy does not exist.
var ErrMissing = errors.New("no such standing policy")

// Dir is the policy dir of the repo whose git common dir is common, and that
// common dir's realpath (a record's Repo).
func Dir(common string, getenv func(string) string) (dir, real string, err error) {
	if real, err = filepath.EvalSymlinks(common); err != nil {
		return "", "", err
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", "", err
	}
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		home := getenv("HOME")
		if home == "" {
			return "", "", errors.New("neither XDG_STATE_HOME nor HOME is set")
		}
		state = filepath.Join(home, ".local/state")
	}
	sum := sha256.Sum256([]byte(real))
	return filepath.Join(state, "crew", "policy", hex.EncodeToString(sum[:])), real, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName returns "" or why name is not a policy name.
func ValidName(name string) string {
	if !nameRE.MatchString(name) {
		return "policy name " + strconv.Quote(name) + " must match " + nameRE.String()
	}
	return ""
}

const maxText = 2000

// ValidText returns "" or why text cannot be a policy's text: it reaches a
// dispatcher's first prompt, so it must be one plain line.
func ValidText(text string) string {
	switch {
	case text == "":
		return "the text is empty"
	case !utf8.ValidString(text):
		return "the text is not valid UTF-8"
	case utf8.RuneCountInString(text) > maxText:
		return fmt.Sprintf("the text is %d characters; the limit is %d", utf8.RuneCountInString(text), maxText)
	case strings.ContainsFunc(text, unicode.IsControl):
		return "the text has a control character (a newline counts); it must be one plain line"
	case invisible.Has(text):
		return "the text has a hidden code point"
	}
	return ""
}

// locate is the policy dir, its file for name, and the repo realpath.
func locate(common, name string, o Options) (dir, file, real string, err error) {
	if reason := ValidName(name); reason != "" {
		return "", "", "", errors.New(reason)
	}
	if dir, real, err = Dir(common, o.Getenv); err != nil {
		return "", "", "", err
	}
	return dir, filepath.Join(dir, name+".json"), real, nil
}

// checkDir refuses a policy dir that is a symlink or not a dir; a missing one
// wraps fs.ErrNotExist.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return errors.New("the policy dir " + dir + " is a symlink")
	}
	if !fi.IsDir() {
		return errors.New("the policy dir " + dir + " is not a directory")
	}
	return nil
}

// Load reads the policy name of common's repo when it is in force. A missing
// policy wraps ErrMissing; any other error is the reason it is not in force.
func Load(common, name string, o Options) (Record, error) {
	dir, file, real, err := locate(common, name, o)
	if err != nil {
		return Record{}, err
	}
	err = checkDir(dir)
	var fi fs.FileInfo
	if err == nil {
		fi, err = os.Lstat(file)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("%w %s in this repo", ErrMissing, name)
	}
	if err != nil {
		return Record{}, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return Record{}, errors.New("the file " + file + " is a symlink")
	}
	if !fi.Mode().IsRegular() {
		return Record{}, errors.New("the file " + file + " is not a regular file")
	}
	owner := int(fi.Sys().(*syscall.Stat_t).Uid)
	if uid := o.UID(); owner != uid {
		return Record{}, fmt.Errorf("the file %s is owned by uid %d, not this uid %d", file, owner, uid)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return Record{}, fmt.Errorf("the file %s has mode %#o; it must have no group or other permission bits", file, perm)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Record{}, err
	}
	var r Record
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("the file %s is not a policy record: %v", file, err)
	}
	for _, f := range []struct{ field, v string }{
		{"name", r.Name}, {"text", r.Text}, {"repo", r.Repo}, {"by", r.By}, {"host", r.Host}, {"tty", r.TTY}, {"at", r.At},
	} {
		if f.v == "" {
			return Record{}, errors.New("the record has no " + f.field)
		}
	}
	switch {
	case r.Name != name:
		return Record{}, fmt.Errorf("the record's name %q is not its file name %q", r.Name, name)
	case r.Repo != real:
		return Record{}, fmt.Errorf("the record's repo %q is not this repo %q", r.Repo, real)
	case r.UID != owner:
		return Record{}, fmt.Errorf("the record's uid %d is not the file owner %d", r.UID, owner)
	}
	if reason := ValidText(r.Text); reason != "" {
		return Record{}, errors.New(reason)
	}
	if _, err := time.Parse(time.RFC3339, r.At); err != nil {
		return Record{}, fmt.Errorf("the record's at %q is not an RFC 3339 time", r.At)
	}
	return r, nil
}

// List is every policy of common's repo by name: the in-force records, and a
// "standing policy <name> is not in force — <reason>" line per refused one.
func List(common string, o Options) (in []Record, refused []string, err error) {
	dir, _, err := Dir(common, o.Getenv)
	if err != nil {
		return nil, nil, err
	}
	if err := checkDir(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		r, err := Load(common, name, o)
		if err != nil {
			refused = append(refused, notInForce(name, err))
			continue
		}
		in = append(in, r)
	}
	return in, refused, nil
}

func notInForce(name string, err error) string {
	return "standing policy " + oneLine(name) + " is not in force — " + oneLine(err.Error())
}

// Write stores rec as its name's policy of common's repo, replacing any, and
// reports a record that does not read back in force as an error.
func Write(common string, rec Record, o Options) error {
	if reason := ValidText(rec.Text); reason != "" {
		return errors.New(reason)
	}
	dir, file, _, err := locate(common, rec.Name, o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkDir(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+rec.Name+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename moved it
	if err := writeTemp(tmp, rec); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	if _, err := Load(common, rec.Name, o); err != nil {
		return fmt.Errorf("wrote %s, but it is not in force: %w", file, err)
	}
	return nil
}

func writeTemp(tmp *os.File, rec Record) error {
	err := tmp.Chmod(0o600)
	if err == nil {
		err = json.NewEncoder(tmp).Encode(rec)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	return err
}

// Clear removes the policy name of common's repo; a missing one wraps
// ErrMissing.
func Clear(common, name string, o Options) error {
	dir, file, _, err := locate(common, name, o)
	if err != nil {
		return err
	}
	if err = checkDir(dir); err == nil {
		_, err = os.Lstat(file)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w %s", ErrMissing, name)
	}
	if err != nil {
		return err
	}
	return os.Remove(file)
}

var ownerMarkers = []struct{ name, kind string }{
	{"CREW_WORKER_ID", "a worker"},
	{"CREW_ROLE_ID", "a grid role"},
	{"CREW_OVERSEER", "the overseer"},
	{"CREW_ID", "a dispatcher"},
	{"DISPATCHER_LAUNCHER_PID", "a dispatcher"},
	{"DISPATCHER_HANDOFF_DIR", "a dispatcher"},
}

// OwnerRefusal returns "" when this caller may write a standing policy, else
// the reason: only the owner writes one, from their own terminal, outside every
// crew pane.
func OwnerRefusal(ctx context.Context, o Options) string {
	for _, m := range ownerMarkers {
		if o.Getenv(m.name) != "" {
			return m.name + " is set, so this is " + m.kind + " session, and only the repo owner writes a standing policy; unset it if your own shell exported it"
		}
	}
	if !o.IsTTY() {
		return "stdin is not a terminal: writing a standing policy needs your own terminal (not an agent's shell tool or Claude Code's ! prefix)"
	}
	if desc := sender.CrewPane(ctx, o.Sender); desc != "" {
		return "this is a crew pane (" + desc + "), and only the repo owner writes a standing policy, from their own terminal"
	}
	return ""
}

// Request is one `crew policy` invocation.
type Request struct {
	Action string // set, clear, show or list
	Name   string
	Text   string
	JSON   bool
	Brief  bool
}

// Run carries out req against common's repo and returns the exit code.
func Run(ctx context.Context, req Request, common string, stdout, stderr io.Writer, o Options) int {
	fail := func(msg string) int {
		say(stderr, "crew: policy: %s\n", msg)
		return 1
	}
	switch req.Action {
	case "set":
		if reason := OwnerRefusal(ctx, o); reason != "" {
			return fail(reason)
		}
		rec, err := stamp(common, req.Name, req.Text, o)
		if err == nil {
			err = Write(common, rec, o)
		}
		if err != nil {
			return fail("cannot set standing policy " + oneLine(req.Name) + " — " + oneLine(err.Error()))
		}
		printRecord(stdout, rec)
		return 0
	case "clear":
		if reason := OwnerRefusal(ctx, o); reason != "" {
			return fail(reason)
		}
		err := Clear(common, req.Name, o)
		if errors.Is(err, ErrMissing) {
			return fail("no standing policy " + oneLine(req.Name))
		}
		if err != nil {
			return fail("cannot clear standing policy " + oneLine(req.Name) + " — " + oneLine(err.Error()))
		}
		say(stdout, "cleared standing policy %s\n", req.Name)
		return 0
	case "show":
		rec, err := Load(common, req.Name, o)
		if errors.Is(err, ErrMissing) {
			return fail("no standing policy " + oneLine(req.Name) + " in this repo")
		}
		if err != nil {
			return fail(notInForce(req.Name, err))
		}
		if req.JSON {
			return encode(stdout, stderr, rec)
		}
		printRecord(stdout, rec)
		return 0
	case "list":
		in, refused, err := List(common, o)
		if err != nil {
			return fail(oneLine(err.Error()))
		}
		for _, line := range refused {
			say(stderr, "crew: policy: %s\n", line)
		}
		switch {
		case req.JSON:
			if in == nil {
				in = []Record{}
			}
			return encode(stdout, stderr, in)
		case req.Brief:
			for _, r := range in {
				say(stdout, "'%s' set by %s@%s from %s at %s: \"%s\"\n",
					oneLine(r.Name), oneLine(r.By), oneLine(r.Host), oneLine(r.TTY), oneLine(r.At), oneLine(r.Text))
			}
		case len(in) == 0:
			say(stdout, "no standing policy in this repo\n")
		default:
			for _, r := range in {
				printRecord(stdout, r)
			}
		}
		return 0
	}
	return fail("unknown action " + oneLine(req.Action))
}

// stamp is the record the owner's set writes: their words with provenance.
func stamp(common, name, text string, o Options) (Record, error) {
	_, real, err := Dir(common, o.Getenv)
	if err != nil {
		return Record{}, err
	}
	by, err := o.User()
	if err != nil {
		return Record{}, err
	}
	host, err := o.Hostname()
	if err != nil {
		return Record{}, err
	}
	return Record{
		Name: name, Text: text, Repo: real, By: by, Host: host,
		UID: o.UID(), TTY: o.TTYName(), At: o.Now().UTC().Format(time.RFC3339),
	}, nil
}

func printRecord(w io.Writer, r Record) {
	say(w, "policy %s — set by %s@%s from %s at %s, verbatim:\n  %s\n",
		oneLine(r.Name), oneLine(r.By), oneLine(r.Host), oneLine(r.TTY), oneLine(r.At), oneLine(r.Text))
}

func encode(stdout, stderr io.Writer, v any) int {
	if err := json.NewEncoder(stdout).Encode(v); err != nil {
		say(stderr, "crew: policy: %s\n", err)
		return 1
	}
	return 0
}

// oneLine keeps untrusted record fields to one visible line.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, invisible.Escape(s))
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
