package sessions

import (
	"errors"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// now is jq's `now`, fixed. Every row's want was produced by running the
// `_sessions` program from adapters/core/crew.sh with `now*1000` replaced by
// 1700000000000; wantErr rows are the inputs on which that jq program exits 5.
const now = 1700000000

type testCase struct {
	name    string
	events  string
	branch  string
	crew    string
	want    string
	wantErr bool
}

var cases = []testCase{
	{name: "folds each session separately, oldest first", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999002000,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999001000,"terminal":true,"age_s":999},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999002000,"terminal":false,"age_s":998}]`},
	{name: "pr_open is not terminal", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"pr_open","pr_url":"https://example.com/pr/1"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"pr_open","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "a branch with a # folds on the last #", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/a#b#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/a#b", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/a#b#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "legacy branch-keyed events fold in as a null session", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
`, branch: "feat/x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x","state":"done","ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "a dispatched session with no status yet has a null state", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s9-9"}
`, branch: "feat/x", crew: "", want: `[{"session":"s9-9","worker_id":"worker:feat/x#s9-9","state":null,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "a watchdog's session-less row folds into the live session", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"blocked","detail":"prompt: interactive prompt in pane %9","source":"watchdog"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"blocked","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "a session-less heartbeat after a newer dispatch folds into that session", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999001000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s2-2"}
{"ts":1699999002000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999000000,"terminal":true,"age_s":1000},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999002000,"terminal":false,"age_s":998}]`},
	{name: "a session-less row on a # branch keys on the full branch", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/a#b#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/a#b","to":"dispatcher:c1","kind":"status","body":{"state":"blocked"}}
`, branch: "feat/a#b", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/a#b#s1-1","state":"blocked","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "a session-less row after a terminal session does not revive it", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999000000,"terminal":true,"age_s":1000},{"session":null,"worker_id":"worker:feat/x","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "a session-less row in the same millisecond as a terminal session does not revive it", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x","state":"working","ts":1699999000000,"terminal":false,"age_s":1000},{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "a resume row starts its session", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999002000,"crew_id":"c1","kind":"resume","branch":"feat/x","session":"s2-2"}
{"ts":1699999003000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"blocked"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999001000,"terminal":false,"age_s":999},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"blocked","ts":1699999003000,"terminal":false,"age_s":997}]`},
	{name: "--crew scopes the fold", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c2","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "c2", want: `[{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "no --crew spans every crew", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c2","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "--crew c1 drops the other crew", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c2","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999002000,"crew_id":"c3","kind":"dispatch","branch":"feat/x","session":"s3-3"}
`, branch: "feat/x", crew: "c1", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "an unknown branch is an empty array", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/nope", crew: "", want: `[]`},
	{name: "an empty log is an empty array", events: "", branch: "feat/x", crew: "", want: `[]`},
	{name: "dispatch for another branch is ignored", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/y","session":"s1-1"}
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/y#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[]`},
	{name: "dispatch with null session is a null-session row", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":null}
`, branch: "feat/x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x","state":null,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "dispatch with false session is a null-session row", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":false}
`, branch: "feat/x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x","state":null,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "dispatch without a session key", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x"}
`, branch: "feat/x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x","state":null,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "same-ts statuses: the later in file order wins", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"blocked"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"blocked","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "equal starts: a session-less row adopts the later session", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s2-2"}
{"ts":1699999000001,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":null,"ts":1699999000000,"terminal":false,"age_s":1000},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999000001,"terminal":false,"age_s":999}]`},
	{name: "session-less row before any start stays null", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
`, branch: "feat/x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x","state":"working","ts":1699999000000,"terminal":false,"age_s":1000},{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":null,"ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "status ts false falls back to the dispatch ts", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
{"ts":false,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "status state false is null", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":false}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":null,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "status state null and missing body", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":null}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x#s2-2","kind":"status"}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":null,"ts":1699999000000,"terminal":false,"age_s":1000},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":null,"ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "body null is state null", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":null}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":null,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "a trailing newline keeps the suffix in the branch", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1\n","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x#s1-1\n", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1\n#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "a trailing newline: the plain branch does not match", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1\n","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[]`},
	{name: "a suffix that is not a session id stays in the branch", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x#s1-1x", crew: "", want: `[{"session":null,"worker_id":"worker:feat/x#s1-1x","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "a from without the worker: prefix is ignored", events: `
{"ts":1699999000000,"crew_id":"c1","from":"dispatcher:c1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[]`},
	{name: "a from of false or null is ignored", events: `
{"ts":1699999000000,"crew_id":"c1","from":false,"to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000000,"crew_id":"c1","from":null,"to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[]`},
	{name: "a non-status event with a non-string from is ignored", events: `
{"ts":1699999000000,"crew_id":"c1","from":5,"kind":"msg"}
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "null events are skipped", events: `
null
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "null events are skipped under a crew filter", events: `
null
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "c1", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "events without crew_id drop under a crew filter", events: `
{"kind":"status","from":"worker:feat/x#s1-1","ts":1699999000000,"body":{"state":"x"}}
`, branch: "feat/x", crew: "c1", want: `[]`},
	{name: "state \"done\"", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state \"failed\"", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"failed"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"failed","ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state \"exited\"", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"exited"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"exited","ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state \"working\"", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state \"\"", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":""}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state [\"done\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["done"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["done"],"ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state [\"done\",\"failed\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["done","failed"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["done","failed"],"ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state [\"failed\",\"exited\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["failed","exited"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["failed","exited"],"ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state [\"done\",\"exited\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["done","exited"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["done","exited"],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state [\"exited\",\"done\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["exited","done"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["exited","done"],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state [\"failed\",\"done\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["failed","done"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["failed","done"],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state []", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":[]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":[],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state [null]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":[null]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":[null],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state [[\"done\"]]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":[["done"]]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":[["done"]],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state [\"done\",\"failed\",\"exited\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["done","failed","exited"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["done","failed","exited"],"ts":1699999000000,"terminal":true,"age_s":1000}]`},
	{name: "state [\"done\",\"failed\",\"exited\",\"x\"]", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["done","failed","exited","x"]}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["done","failed","exited","x"],"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state 5", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":5}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":5,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state true", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":true}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":true,"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state {\"a\":1}", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":{"a":1}}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":{"a":1},"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "state {\"done\":1}", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":{"done":1}}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":{"done":1},"ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "an array terminal state blocks a session-less row", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["done"]}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":["done"],"ts":1699999000000,"terminal":true,"age_s":1000},{"session":null,"worker_id":"worker:feat/x","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "a non-terminal array state lets a session-less row in", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":["failed","done"]}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "an empty array state lets a session-less row in", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":[]}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "a numeric state lets a session-less row in", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":5}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "big literal ts reorder exactly", events: `
{"ts":12345678901234567891,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":12345678901234567890,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":12345678901234567890,"terminal":false,"age_s":-12345677201234568},{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":12345678901234567891,"terminal":false,"age_s":-12345677201234568}]`},
	{name: "big literal ts tie keeps file order", events: `
{"ts":12345678901234567890,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":12345678901234567890,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":12345678901234567890,"terminal":false,"age_s":-12345677201234568},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":12345678901234567890,"terminal":false,"age_s":-12345677201234568}]`},
	{name: "literal ts text is canonicalised", events: `
{"ts":1.0e3,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000000.50,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1.7E12,"crew_id":"c1","from":"worker:feat/x#s3-3","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1.0E+3,"terminal":false,"age_s":1699999999},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999000000.50,"terminal":false,"age_s":999},{"session":"s3-3","worker_id":"worker:feat/x#s3-3","state":"working","ts":1.7E+12,"terminal":false,"age_s":0}]`},
	{name: "1.0 and 1 tie in file order", events: `
{"ts":1.0,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1.0,"terminal":false,"age_s":1699999999},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1,"terminal":false,"age_s":1699999999}]`},
	{name: "1 and 1.0 tie in file order", events: `
{"ts":1,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1.0,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1,"terminal":false,"age_s":1699999999},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1.0,"terminal":false,"age_s":1699999999}]`},
	{name: "fractional and negative ts", events: `
{"ts":1699999000000.5,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":-5,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1.7000000001e12,"crew_id":"c1","from":"worker:feat/x#s3-3","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":-5,"terminal":false,"age_s":1700000000},{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000.5,"terminal":false,"age_s":999},{"session":"s3-3","worker_id":"worker:feat/x#s3-3","state":"working","ts":1.7000000001E+12,"terminal":false,"age_s":-1}]`},
	{name: "huge ts", events: `
{"ts":1e1000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1e18,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1E+18,"terminal":false,"age_s":-999998300000000},{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1E+1000,"terminal":false,"age_s":-1.7976931348623157e+308}]`},
	{name: "a future ts gives a negative age", events: `
{"ts":1700004000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1700004000000,"terminal":false,"age_s":-4000}]`},
	{name: "age floors toward minus infinity at the boundary", events: `
{"ts":1700000000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1700000000001,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999999999,"crew_id":"c1","from":"worker:feat/x#s3-3","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s3-3","worker_id":"worker:feat/x#s3-3","state":"working","ts":1699999999999,"terminal":false,"age_s":0},{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1700000000000,"terminal":false,"age_s":0},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1700000000001,"terminal":false,"age_s":-1}]`},
	{name: "a ts of zero", events: `
{"ts":0,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":-0,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":0,"terminal":false,"age_s":1700000000},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":-0,"terminal":false,"age_s":1700000000}]`},
	{name: "three sessions, mixed order, resume, legacy row", events: `
{"ts":1699999003000,"crew_id":"c1","from":"worker:feat/x#s3-3","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
{"ts":1699999000500,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999001000,"crew_id":"c1","kind":"resume","branch":"feat/x","session":"s2-2"}
{"ts":1699999001500,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"blocked"}}
{"ts":1699999002000,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"exited"}}
{"ts":1699999002500,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000100,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999000500,"terminal":true,"age_s":999},{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"exited","ts":1699999002000,"terminal":true,"age_s":998},{"session":null,"worker_id":"worker:feat/x","state":"working","ts":1699999002500,"terminal":false,"age_s":997},{"session":"s3-3","worker_id":"worker:feat/x#s3-3","state":"working","ts":1699999003000,"terminal":false,"age_s":997}]`},
	{name: "two crews, same branch, session-less rows cross crews", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c2","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"blocked"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"blocked","ts":1699999001000,"terminal":false,"age_s":999}]`},
	{name: "two crews, session-less row filtered away by crew", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c2","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"blocked"}}
`, branch: "feat/x", crew: "c1", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "two crews with their own sessions", events: `
{"ts":1699999000000,"crew_id":"a","kind":"dispatch","branch":"feat/x","session":"s1-1"}
{"ts":1699999000001,"crew_id":"a","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999000002,"crew_id":"b","kind":"dispatch","branch":"feat/x","session":"s2-2"}
{"ts":1699999000003,"crew_id":"b","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000004,"crew_id":"b","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "b", want: `[{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999000004,"terminal":false,"age_s":999}]`},
	{name: "ERR number event, no crew", events: `
5
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR number event, crew", events: `
5
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "c1", wantErr: true},
	{name: "ERR string event", events: `
"x"
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR array event", events: `
[1]
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR array event, crew", events: `
[1]
`, branch: "feat/x", crew: "c1", wantErr: true},
	{name: "ERR false event", events: `
false
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR true event, crew", events: `
true
`, branch: "feat/x", crew: "c1", wantErr: true},
	{name: "ERR a number event after valid ones, crew", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
0
`, branch: "feat/x", crew: "c1", wantErr: true},
	{name: "ERR number from on a status", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"status","from":5,"body":{"state":"x"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR true from on a status", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"status","from":true,"body":{"state":"x"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR array from on a status", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"status","from":["worker:feat/x"],"body":{"state":"x"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR object from on a status", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"status","from":{},"body":{"state":"x"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "a number from on a status in another crew is not evaluated", events: `
{"ts":1699999000000,"crew_id":"c2","kind":"status","from":5,"body":{"state":"x"}}
`, branch: "feat/x", crew: "c1", want: `[]`},
	{name: "ERR string body on the matching branch", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":"x"}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR number body", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":5}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR false body", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":false}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR array body", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":[]}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "a bad body on another branch is not evaluated", events: `
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/y#s1-1","to":"dispatcher:c1","kind":"status","body":"x"}
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
	{name: "a bad body on a non-worker from is not evaluated", events: `
{"ts":1699999000000,"crew_id":"c1","from":"dispatcher:c1","to":"dispatcher:c1","kind":"status","body":"x"}
`, branch: "feat/x", crew: "", want: `[]`},
	{name: "ERR number session on dispatch", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":1}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR true session on dispatch", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":true}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR array session on dispatch", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"resume","branch":"feat/x","session":["s1-1"]}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR object session on dispatch", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":{}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "a number session on another branch is not evaluated", events: `
{"ts":1699999000000,"crew_id":"c1","kind":"dispatch","branch":"feat/y","session":1}
`, branch: "feat/x", crew: "", want: `[]`},
	{name: "ERR string ts on a status", events: `
{"ts":"x","crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR null ts on a status", events: `
{"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"x"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR array ts on a dispatch", events: `
{"ts":[1],"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR bool ts", events: `
{"ts":true,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR null ts on a dispatch", events: `
{"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1"}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "ERR false ts on a sole status", events: `
{"ts":false,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", wantErr: true},
	{name: "a bad ts on another branch is not evaluated", events: `
{"ts":"x","crew_id":"c1","from":"worker:feat/y#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`, branch: "feat/x", crew: "", want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"working","ts":1699999000000,"terminal":false,"age_s":1000}]`},
}

func TestFold(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := jsonv.DecodeStream(strings.NewReader(tc.events))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Fold(events, tc.branch, tc.crew, now)
			if tc.wantErr {
				var typeErr *jsonv.TypeError
				if !errors.As(err, &typeErr) {
					t.Fatalf("got %s, %v; want a *jsonv.TypeError", jsonv.Append(nil, got, jsonv.Options{}), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s := string(jsonv.Append(nil, got, jsonv.Options{})); s != tc.want {
				t.Errorf("got  %s\nwant %s", s, tc.want)
			}
		})
	}
}
