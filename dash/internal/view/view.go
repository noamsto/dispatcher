// Package view holds one plain struct per dash tab. The TUI renderers and
// `crew dash --json --view` both read these structs, so a field exists once.
// Builders are pure: no I/O, no styling.
package view

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

const SchemaVersion = 1

// Names is every view `--view` accepts, in tab order.
var Names = []string{"overview", "roster", "budget", "runs", "settings", "map"}

// Loaded says which sources have delivered at least once.
type Loaded struct {
	Settings, Budget, Retro, Ratings, Roster, Bus bool
}

// Inputs is everything a builder reads.
type Inputs struct {
	Snap   data.Snapshot
	Bus    data.BusDigest
	Loaded Loaded
	NowMS  int64
	Crew   []string
}

func (in Inputs) nowSec() int64 {
	if in.NowMS != 0 {
		return in.NowMS / 1000
	}
	return in.Snap.Now
}

// Build returns the named view as a JSON-marshalable struct.
func Build(name string, in Inputs) (any, error) {
	switch name {
	case "overview":
		return BuildOverview(in), nil
	case "roster":
		return BuildRoster(in), nil
	case "budget":
		return BuildBudget(in), nil
	case "runs":
		return BuildRuns(in), nil
	case "settings":
		return BuildSettings(in), nil
	case "map":
		return BuildMap(in), nil
	}
	return nil, fmt.Errorf("unknown view %q (valid: %s)", name, strings.Join(Names, ", "))
}

// Origin says where a row's worker runs and so what may be done to it.
type Origin string

const (
	OriginLocal  Origin = "local"
	OriginRemote Origin = "remote_worker"
	OriginPeer   Origin = "peer"
)

// OriginOf classifies a roster row: peer rows come from another host's
// snapshot, remote rows from a --host worker, the rest are local.
func OriginOf(row map[string]any) Origin {
	if p, _ := row["peer"].(bool); p {
		return OriginPeer
	}
	if h, _ := row["host"].(string); h != "" {
		return OriginRemote
	}
	return OriginLocal
}

// Action is one operator command: Command is the shell rendering of Argv, and
// the TUI executes Argv, so the two cannot drift. Argv is tagged raw: the TUI
// cleans every other string for display but must execute these bytes as built.
type Action struct {
	Key        string   `json:"key"`
	Verb       string   `json:"verb"`
	Label      string   `json:"label"`
	Command    string   `json:"command"`
	Argv       []string `json:"argv" dash:"raw"`
	NeedsInput bool     `json:"needs_input"`
	PR         *int     `json:"pr"`
}

func crewArgv(crew []string, sub, crewID string, target ...string) []string {
	argv := append(slices.Clone(crew), sub, "--crew", crewID, "--")
	return append(argv, target...)
}

// NudgeTarget is the codename, else the branch.
func NudgeTarget(name, branch string) string {
	if name != "" {
		return name
	}
	return branch
}

func PeekArgv(crew []string, crewID, branch string) []string {
	return crewArgv(crew, "where", crewID, branch)
}

func NudgeArgv(crew []string, crewID, name, branch string) []string {
	return crewArgv(crew, "nudge", crewID, NudgeTarget(name, branch))
}

func ReplyArgv(crew []string, crewID, branch, text string) []string {
	return crewArgv(crew, "reply", crewID, "worker:"+branch, text)
}

func MergeArgv(crew []string, crewID string, pr int) []string {
	return append(slices.Clone(crew), "merge", strconv.Itoa(pr), "--crew", crewID)
}

// shellSafe is a token sh and fish both read literally. % is never bare (fish
// expands the whole token %self to its PID), and = @ : , + only follow a safe
// first character, since a word's first character is where shells give
// punctuation meaning (zsh's =cmd); quoting those costs nothing.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./-][A-Za-z0-9_./:@,+=-]*$`)

// shellEscaper closes the quote around each ' and \: fish treats \' and \\ as
// escapes inside single quotes, POSIX sh does not, and this form reads the
// same in both.
var shellEscaper = strings.NewReplacer(`\`, `'\\'`, `'`, `'\''`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + shellEscaper.Replace(s) + "'"
}

func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func newAction(key, verb, label string, needsInput bool, argv []string) Action {
	return Action{Key: key, Verb: verb, Label: label, Command: shellJoin(argv), Argv: argv, NeedsInput: needsInput}
}

// WorkerActions is the operator's menu for one worker. Peer workers belong to
// another host's dispatcher and get none; merge needs a known PR.
func WorkerActions(origin Origin, crew []string, crewID, branch, name string, pr *int) []Action {
	if origin == OriginPeer {
		return []Action{}
	}
	acts := []Action{
		newAction("p", "peek", "peek", false, PeekArgv(crew, crewID, branch)),
		newAction("n", "nudge", "nudge", false, NudgeArgv(crew, crewID, name, branch)),
		newAction("r", "reply", "reply", true, ReplyArgv(crew, crewID, branch, "<text>")),
	}
	if pr != nil {
		merge := newAction("m", "merge", "squash-merge", false, MergeArgv(crew, crewID, *pr))
		merge.PR = pr
		acts = append(acts, merge)
	}
	return acts
}

var prNumberRe = regexp.MustCompile(`^https?://[^/\s]+/[^/\s]+/[^/\s]+/pull/(\d+)(?:[/?#]\S*)?$`)

// PRNumber is the PR number of a GitHub pull URL, nil for anything else.
func PRNumber(url string) *int {
	m := prNumberRe.FindStringSubmatch(url)
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return &n
}
