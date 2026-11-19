package view

import (
	"fmt"
	"regexp"
	"strconv"
)

// AllKinds lists every Kind; a test requires clearsText to cover each.
var AllKinds = []Kind{
	KindBlocked, KindPrompt, KindStall, KindWatchdog, KindDead, KindFailed, KindStale,
	KindRoleFailed, KindMergeReady, KindLaneDead, KindLaneStale, KindHoldMatured,
	KindRelayLost, KindPeerStale, KindDispatcherGone, KindDispatcherWaiting, KindRemoteWatchDead,
}

// clearsCtx is what clearsText may name. Keep is crew reap's reason for
// keeping the branch.
type clearsCtx struct {
	PR                   *int
	Branch, CrewID, Role string
	Keep                 string
	Inspect, Remove      string
}

var branchIssueRe = regexp.MustCompile(`^[a-z]+/(\d+)-`)

func clearsText(kind Kind, c clearsCtx) string {
	switch kind {
	case KindBlocked:
		return "a `crew reply` to its question"
	case KindPrompt:
		return "answer the prompt in pane (crew where " + c.Branch + ")"
	case KindStall:
		return "clears on next output; dead after 30 min more"
	case KindWatchdog:
		return "clears when the worker emits output or `crew nudge` wakes it"
	case KindDead, KindFailed:
		return reapClears(c)
	case KindStale:
		if c.Inspect == "" || c.Remove == "" {
			return "run `crew reap --dry-run` on the worker's host for the inspect and remove commands"
		}
		return "inspect: " + c.Inspect + "; remove: " + c.Remove
	case KindRoleFailed:
		return "restart the role pane or `crew nudge` the worker"
	case KindMergeReady:
		if c.PR != nil {
			return "merge PR #" + strconv.Itoa(*c.PR)
		}
		return "merge its PR"
	case KindLaneDead, KindLaneStale:
		return "re-arm `crew stream --crew " + c.CrewID + "`"
	case KindHoldMatured:
		return "release or drop the hold"
	case KindRelayLost:
		return "restore the relay link to the host"
	case KindPeerStale:
		return "clears when the peer's next sync succeeds"
	case KindDispatcherGone:
		return "relaunch the dispatcher or finish its workers"
	case KindDispatcherWaiting:
		return "answer the dispatcher in its pane"
	case KindRemoteWatchDead:
		return "re-arm the remote watch for the host"
	}
	return ""
}

func reapClears(c clearsCtx) string {
	switch m := branchIssueRe.FindStringSubmatch(c.Branch); {
	case c.Keep != "":
		return "kept by reap: " + c.Keep
	case c.PR != nil:
		return fmt.Sprintf("reaped once PR #%d merges/closes", *c.PR)
	case m != nil:
		return fmt.Sprintf("reaped once issue #%s closes, or re-dispatch", m[1])
	}
	return "reaped once its PR merges/closes or its issue closes (crew reap), or re-dispatch"
}
