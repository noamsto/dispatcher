// Package release is the C2 claim-release rule crew migrate's marks feed. It
// is its own leaf so reap, which migrate imports, can read it too.
package release

import "github.com/noamsto/dispatcher/crew/internal/bus"

// Release is a branch's active migrate mark: the host it is released to and
// the snapshot that travelled.
type Release struct {
	ToHost, Mode, Tip, Tree, Patch string
}

// Active is the C2 rule: the newest worker's newest status row that is
// neither an `exited` SessionEnd backstop nor a watchdog row, when that row is
// a `migrated` mark from crew migrate. Killing the window fires the SessionEnd
// hook and an orphaned stall-watch can still post; neither is the worker's word.
func Active(rows []bus.Event, branch string) (Release, bool) {
	worker := NewestWorker(rows, branch)
	if worker == "" {
		return Release{}, false
	}
	var last map[string]any
	for _, e := range rows {
		if e.Kind != bus.KindStatus || e.From != worker {
			continue
		}
		body, _ := e.Doc.(map[string]any)["body"].(map[string]any)
		if field(body, "state") == string(bus.StateExited) || field(body, "source") == "watchdog" {
			continue
		}
		last = body
	}
	if field(last, "state") != string(bus.StateMigrated) || field(last, "source") != "migrate" || field(last, "to_host") == "" {
		return Release{}, false
	}
	return Release{
		ToHost: field(last, "to_host"), Mode: field(last, "mode"),
		Tip: field(last, "tip"), Tree: field(last, "tree"), Patch: field(last, "patch"),
	}, true
}

// Released reports whether branch is released to host (Active).
func Released(rows []bus.Event, branch, host string) bool {
	rel, ok := Active(rows, branch)
	return ok && rel.ToHost == host
}

// NewestWorker is `_branch_finished`'s worker: the newest local dispatch row's
// worker_id, followed through the resume rows chained by prev_worker_id. A
// remote:true row names a worker whose status rows live on its own host; once
// a migration's remote resume lands it is the newest dispatch row, and it must
// not hide the local worker's mark.
func NewestWorker(rows []bus.Event, branch string) string {
	worker := ""
	for _, e := range rows {
		if e.Kind != bus.KindDispatch || e.Branch != branch {
			continue
		}
		if doc := e.Doc.(map[string]any); doc["remote"] != true {
			worker = field(doc, "worker_id")
		}
	}
	if worker == "" {
		return ""
	}
	for _, e := range rows {
		if e.Kind != bus.KindResume || e.Branch != branch {
			continue
		}
		doc := e.Doc.(map[string]any)
		if next := field(doc, "worker_id"); next != "" && field(doc, "prev_worker_id") == worker {
			worker = next
		}
	}
	return worker
}

// field is obj[key] as a string, "" when absent or not a string.
func field(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return s
}
