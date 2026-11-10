package rolewatch

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

const (
	detailPullUnacked   = "assignment not picked up: the role pulls its assignments with crew await and has not acked this one"
	detailPullUnstarted = "assignment not picked up: the role has not announced itself (still booting, or never started)"
	detailTypedDeferred = "assignment not delivered: the pane is not at an idle input box (a permission dialog, prompt, live turn or unrecognised frame), or its engine has no recognised idle frame"
	detailRejected      = "assignment not delivered: its body contains a control character (newline/tab/…); re-send it on one line, e.g. compact JSON via jq -c"
	detailUnknownFrame  = "could not confirm the submit: the pane matches no recognised frame"

	unsubmittedPrefix = "assignment typed but not submitted: "
	unsubmittedSuffix = "; left in place, not re-pasted — capture the pane and, only if it is still an idle input box holding the text, submit it with Enter — or clear the box (C-u) before re-sending"
)

func detailStillInBox(retries int) string {
	return fmt.Sprintf("still in the input box after %d Enter retries", retries)
}

// Each body lists its fields in the arm's jq key order and keeps empty values,
// as jq does.
type (
	dropBody struct {
		Role   string `json:"role"`
		Event  string `json:"event"`
		Pane   string `json:"pane"`
		From   string `json:"from"`
		To     string `json:"to"`
		Detail string `json:"detail"`
	}
	deferredBody struct {
		Role     string `json:"role"`
		Event    string `json:"event"`
		Pane     string `json:"pane"`
		Engine   string `json:"engine"`
		Delivery string `json:"delivery"`
		Detail   string `json:"detail"`
	}
	engineBody struct {
		Role   string `json:"role"`
		Event  string `json:"event"`
		Pane   string `json:"pane"`
		Engine string `json:"engine"`
		Detail string `json:"detail"`
	}
	orphanedBody struct {
		Role   string `json:"role"`
		Event  string `json:"event"`
		Pane   string `json:"pane"`
		Detail string `json:"detail"`
	}
)

// encodeNotice is v as one compact line with <>& unescaped, like jq -c.
func encodeNotice(v any) string {
	var b bytes.Buffer
	_ = jsonval.Encode(&b, v, false)
	return strings.TrimSuffix(b.String(), "\n")
}

func dropNotice(role, pane, from, to string) string {
	return encodeNotice(dropBody{role, "role_watch_drop", pane, from, to, "role-watch: dropped msg from " + from})
}

func deferredNotice(role, pane, engine, delivery, detail string) string {
	return encodeNotice(deferredBody{role, "assignment_deferred", pane, engine, delivery, detail})
}

func unsubmittedNotice(role, pane, engine, detail string) string {
	return encodeNotice(engineBody{role, "assignment_unsubmitted", pane, engine, unsubmittedPrefix + detail + unsubmittedSuffix})
}

func orphanedNotice(role, pane, branch string) string {
	return encodeNotice(orphanedBody{role, "assignment_orphaned", pane,
		"watcher exited: pane " + pane + " is no longer stamped as " + role + " on " + branch + "; re-send after re-arming (dispatch resume --rearm-roles)"})
}

func rejectedNotice(role, pane, engine string) string {
	return encodeNotice(engineBody{role, "assignment_rejected", pane, engine, detailRejected})
}
