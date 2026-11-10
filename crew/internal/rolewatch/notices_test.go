package rolewatch

import (
	"reflect"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

// The programs are the dispatch.sh arms' `jq -nc` programs, verbatim.
const (
	jqDrop = `{role:$r,event:"role_watch_drop",pane:$p,from:$f,to:$t,detail:("role-watch: dropped msg from " + $f)}`

	jqDeferredPull = `{role:$r,event:"assignment_deferred",pane:$p,engine:$e,delivery:"pull",detail:$d}`

	jqDeferredTyped = `{role:$r,event:"assignment_deferred",pane:$p,engine:$e,delivery:"typed",detail:"assignment not delivered: the pane is not at an idle input box (a permission dialog, prompt, live turn or unrecognised frame), or its engine has no recognised idle frame"}`

	jqUnsubmitted = `{role:$r,event:"assignment_unsubmitted",pane:$p,engine:$e,detail:("assignment typed but not submitted: " + $d + "; left in place, not re-pasted — capture the pane and, only if it is still an idle input box holding the text, submit it with Enter — or clear the box (C-u) before re-sending")}`

	jqOrphaned = `{role:$r,event:"assignment_orphaned",pane:$p,detail:("watcher exited: pane " + $p + " is no longer stamped as " + $r + " on " + $b + "; re-send after re-arming (dispatch resume --rearm-roles)")}`

	jqRejected = `{role:$r,event:"assignment_rejected",pane:$p,engine:$e,detail:"assignment not delivered: its body contains a control character (newline/tab/…); re-send it on one line, e.g. compact JSON via jq -c"}`
)

func TestNotices(t *testing.T) {
	const role, pane = "reviewer", "%7"
	tests := []struct {
		name string
		prog string
		vars map[string]any
		got  string
	}{
		{"drop", jqDrop,
			map[string]any{"r": role, "p": pane, "f": "worker:feat/8-other#s1-1", "t": "role:feat/9-x:reviewer"},
			dropNotice(role, pane, "worker:feat/8-other#s1-1", "role:feat/9-x:reviewer")},
		{"drop escapes", jqDrop,
			map[string]any{"r": role, "p": pane, "f": `a<b>&"c"`, "t": ""},
			dropNotice(role, pane, `a<b>&"c"`, "")},
		{"deferred pull unacked", jqDeferredPull,
			map[string]any{"r": role, "p": pane, "e": "pi", "d": detailPullUnacked},
			deferredNotice(role, pane, "pi", "pull", detailPullUnacked)},
		{"deferred pull unstarted", jqDeferredPull,
			map[string]any{"r": role, "p": pane, "e": "pi", "d": detailPullUnstarted},
			deferredNotice(role, pane, "pi", "pull", detailPullUnstarted)},
		{"deferred typed", jqDeferredTyped,
			map[string]any{"r": role, "p": pane, "e": "claude"},
			deferredNotice(role, pane, "claude", "typed", detailTypedDeferred)},
		{"unsubmitted retries", jqUnsubmitted,
			map[string]any{"r": role, "p": pane, "e": "codex", "d": detailStillInBox(3)},
			unsubmittedNotice(role, pane, "codex", detailStillInBox(3))},
		{"unsubmitted unknown frame", jqUnsubmitted,
			map[string]any{"r": role, "p": pane, "e": "cursor", "d": detailUnknownFrame},
			unsubmittedNotice(role, pane, "cursor", detailUnknownFrame)},
		{"orphaned", jqOrphaned,
			map[string]any{"r": role, "p": pane, "b": "feat/9-x"},
			orphanedNotice(role, pane, "feat/9-x")},
		{"rejected", jqRejected,
			map[string]any{"r": role, "p": pane, "e": "claude"},
			rejectedNotice(role, pane, "claude")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want, err := jqrun.RunAny(tc.prog, nil, 0, tc.vars)
			if err != nil {
				t.Fatal(err)
			}
			got, err := jqrun.FromJSON(tc.got)
			if err != nil {
				t.Fatalf("notice is not JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("notice = %s\nwant jq value %v", tc.got, want)
			}
		})
	}
}

func TestNoticeBytes(t *testing.T) {
	got := rejectedNotice("r", "%1", "pi")
	want := `{"role":"r","event":"assignment_rejected","pane":"%1","engine":"pi","detail":"` + detailRejected + `"}`
	if got != want {
		t.Errorf("rejectedNotice = %s, want %s", got, want)
	}
}
