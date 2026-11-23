package view

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityReady    Severity = "ready"
)

type Kind string

const (
	KindBlocked     Kind = "blocked"
	KindPrompt      Kind = "prompt"
	KindStall       Kind = "stall"
	KindWatchdog    Kind = "watchdog"
	KindDead        Kind = "dead"
	KindFailed      Kind = "failed"
	KindStale       Kind = "stale"
	KindRoleFailed  Kind = "role_failed"
	KindMergeReady  Kind = "merge_ready"
	KindLaneDead    Kind = "lane_dead"
	KindLaneStale   Kind = "lane_stale"
	KindHoldMatured Kind = "hold_matured"

	KindRelayLost         Kind = "relay_lost"
	KindPeerStale         Kind = "peer_stale"
	KindDispatcherGone    Kind = "dispatcher_gone"
	KindDispatcherWaiting Kind = "dispatcher_waiting"
	KindRemoteWatchDead   Kind = "remote_watch_dead"
)

// AttentionItem is one thing that needs the operator. Action is nil for peer
// workers, which belong to another host's dispatcher. CrewKey is the fleet
// crew row an item came from: nil outside the fleet and for host-level items.
type AttentionItem struct {
	ID       string   `json:"id"`
	CrewKey  *string  `json:"crew_key"`
	Crew     string   `json:"crew"`
	Branch   string   `json:"branch"`
	Worker   string   `json:"worker"`
	Tmux     *string  `json:"tmux,omitempty"`
	Host     string   `json:"host"`
	Origin   Origin   `json:"origin"`
	Severity Severity `json:"severity"`
	Kind     Kind     `json:"kind"`
	Message  string   `json:"message"`
	Clears   string   `json:"clears"`
	AgeSec   int64    `json:"age_sec"`
	Action   *Action  `json:"action"`
}

func InspectLaneArgv(crew []string, crewID string) []string {
	return append(slices.Clone(crew), "stream", "--crew", crewID, "--status")
}

func InspectHoldArgv(crew []string, crewID string) []string {
	return append(slices.Clone(crew), "hold", "list", "--crew", crewID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

func (w WorkerRow) display() string {
	return NudgeTarget(deref(w.Name), w.Branch)
}

func (w WorkerRow) action(key string) *Action {
	for i := range w.Actions {
		if w.Actions[i].Key == key {
			return &w.Actions[i]
		}
	}
	return nil
}

func watchdogKind(detail string) Kind {
	switch {
	case strings.HasPrefix(detail, "prompt:"):
		return KindPrompt
	case strings.HasPrefix(detail, "quiet:"), strings.HasPrefix(detail, "stalled:"), strings.HasPrefix(detail, "turn-stall:"):
		return KindStall
	}
	return KindWatchdog
}

// stateItem classifies a worker from its roster row alone, so it works for
// local, remote and peer rows. Peer rows lack source, so they fall back to the
// plain blocked/failed kinds.
func stateItem(w WorkerRow) (kind Kind, sev Severity, key string, ok bool) {
	detail := deref(w.Detail)
	watchdog := deref(w.Source) == "watchdog"
	if w.NeedsCleanup {
		return KindStale, SeverityWarning, "p", true
	}
	switch w.State {
	case "blocked":
		if !watchdog {
			return KindBlocked, SeverityWarning, "r", true
		}
		kind = watchdogKind(detail)
		key = "p"
		if kind == KindStall {
			key = "n"
		}
		return kind, SeverityWarning, key, true
	case "failed":
		if watchdog && strings.HasPrefix(detail, "dead:") {
			return KindDead, SeverityCritical, "p", true
		}
		return KindFailed, SeverityCritical, "p", true
	}
	return "", "", "", false
}

func message(w WorkerRow, fallback string) string {
	if d := deref(w.Detail); d != "" {
		return d
	}
	return fallback
}

func attentionOf(w WorkerRow, kind Kind, sev Severity, key, msg, keep string) AttentionItem {
	id := fmt.Sprintf("%s:%s:%s", kind, w.Crew, w.Branch)
	if w.Host != "" {
		id = fmt.Sprintf("%s:%s/%s:%s", kind, w.Host, w.Crew, w.Branch)
	}
	return AttentionItem{
		ID:       id,
		Crew:     w.Crew,
		Branch:   w.Branch,
		Worker:   w.display(),
		Tmux:     w.Tmux,
		Host:     w.Host,
		Origin:   w.Origin,
		Severity: sev,
		Kind:     kind,
		Message:  msg,
		Clears:   clearsText(kind, clearsCtx{PR: w.PR, Branch: w.Branch, CrewID: w.Crew, Keep: keep, Inspect: w.CleanupInspect, Remove: w.CleanupRemove}),
		AgeSec:   derefInt(w.AgeS),
		Action:   w.action(key),
	}
}

func roleFailed(role string, r *data.RoleDigest) bool {
	switch r.State {
	case "blocked":
		return strings.Contains(r.Detail, "role "+role+" engine exited")
	case "failed":
		return r.Detail != "no assignment"
	}
	return false
}

// ledgerPresent reads the digest ledger; a remote worker has no local bus
// events, so its (clipped) roster detail stands in.
func ledgerPresent(w WorkerRow, d *data.WorkerDigest) bool {
	if d != nil && d.Ledger != "" {
		return true
	}
	if w.Origin == OriginRemote {
		_, _, ok := ledgerCount(deref(w.Detail))
		return ok
	}
	return false
}

func mergeReady(w WorkerRow, in Inputs) bool {
	if w.State != "pr_open" || w.PR == nil || w.Origin == OriginPeer {
		return false
	}
	pr := in.Bus.PRs[*w.PR]
	if pr == nil || pr.Checks != "SUCCESS" || pr.Mergeable != "MERGEABLE" || pr.State != "OPEN" {
		return false
	}
	return ledgerPresent(w, in.Bus.Workers[data.WorkerKey{Crew: w.Crew, Branch: w.Branch}])
}

var severityRank = map[Severity]int{SeverityCritical: 0, SeverityWarning: 1, SeverityReady: 2}

// laneGraceSec is how long a freshly launched crew may take to bring its lane up.
const laneGraceSec = 300

var crewIDRe = regexp.MustCompile(`^(\d{10})-\d+$`)

// crewLaunched parses the launch time out of a crew id of the form <unix>-<n>.
func crewLaunched(id string, now int64) (int64, bool) {
	m := crewIDRe.FindStringSubmatch(id)
	if m == nil {
		return 0, false
	}
	t, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || t < now-10*365*86400 || t > now+60 {
		return 0, false
	}
	return t, true
}

// laneActionable reports whether a bad lane needs a human now: the dispatcher
// is here, something depends on it, and the crew is past its launch grace.
func laneActionable(c data.RosterCrew, rows []WorkerRow, now int64) bool {
	if c.Dispatcher != nil && c.Dispatcher.Where != "here" {
		return false
	}
	launched, ok := crewLaunched(c.ID, now)
	if c.Dispatcher != nil && c.Dispatcher.Since != nil {
		launched, ok = max(launched, *c.Dispatcher.Since), true
	}
	if ok && now-launched < laneGraceSec {
		return false
	}
	for _, w := range rows {
		if w.Crew == c.ID && w.Origin != OriginPeer && w.working() {
			return true
		}
	}
	return slices.ContainsFunc(c.Holds, func(h map[string]any) bool { return HoldRowOf(h, now).Matured })
}

func buildAttention(in Inputs, rows []WorkerRow) []AttentionItem {
	now := in.nowSec()
	items := []AttentionItem{}
	byKey := map[data.WorkerKey]WorkerRow{}
	for _, w := range rows {
		if w.Origin == OriginLocal {
			byKey[data.WorkerKey{Crew: w.Crew, Branch: w.Branch}] = w
		}
		if kind, sev, key, ok := stateItem(w); ok {
			var keep string
			if w.Origin == OriginLocal {
				keep = in.reapKeep(w)
			}
			msg := message(w, w.State)
			if kind == KindStale {
				msg = "stale: needs human cleanup"
				if w.CleanupReason != "" {
					msg += " — " + w.CleanupReason
				}
			}
			it := attentionOf(w, kind, sev, key, msg, keep)
			if kind == KindBlocked && w.BlockedSince != nil {
				it.AgeSec = max(now-*w.BlockedSince/1000, 0)
			}
			items = append(items, it)
		}
		if mergeReady(w, in) {
			items = append(items, attentionOf(w, KindMergeReady, SeverityReady, "m", fmt.Sprintf("PR #%d checks green, mergeable", *w.PR), ""))
		}
	}

	failedRoles := map[data.WorkerKey][]string{}
	for k, r := range in.Bus.Roles {
		wk := data.WorkerKey{Crew: k.Crew, Branch: k.Branch}
		if w, live := byKey[wk]; live && w.working() && roleFailed(k.Role, r) {
			failedRoles[wk] = append(failedRoles[wk], k.Role)
		}
	}
	for wk, roles := range failedRoles {
		slices.Sort(roles)
		w := byKey[wk]
		var parts []string
		age := derefInt(w.AgeS)
		for _, role := range roles {
			r := in.Bus.Roles[data.RoleKey{Crew: wk.Crew, Branch: wk.Branch, Role: role}]
			parts = append(parts, role+": "+r.Detail)
			age = max(age, now-r.TS/1000)
		}
		it := attentionOf(w, KindRoleFailed, SeverityCritical, "p", strings.Join(parts, "; "), "")
		it.AgeSec = age
		items = append(items, it)
	}

	for _, c := range in.Snap.Roster.Crews {
		if c.Lane != nil && (c.Lane.State == "dead" || c.Lane.State == "stale") && laneActionable(c, rows, now) {
			kind, sev := KindLaneDead, SeverityCritical
			if c.Lane.State == "stale" {
				kind, sev = KindLaneStale, SeverityWarning
			}
			act := withDir(newAction("", "inspect", "inspect", false, InspectLaneArgv(in.Crew, c.ID)), in.Dir)
			items = append(items, AttentionItem{
				ID: fmt.Sprintf("%s:%s:", kind, c.ID), Crew: c.ID, Origin: OriginLocal,
				Severity: sev, Kind: kind, Message: "crew stream lane is " + c.Lane.State,
				Clears: clearsText(kind, clearsCtx{CrewID: c.ID}), Action: &act,
			})
		}
		for _, h := range c.Holds {
			hr := HoldRowOf(h, now)
			if !hr.Matured {
				continue
			}
			act := withDir(newAction("", "inspect", "inspect", false, InspectHoldArgv(in.Crew, c.ID)), in.Dir)
			msg := "hold " + hr.ID + " matured"
			if t := deref(hr.Title); t != "" {
				msg += ": " + t
			}
			// A fleet-status hold has no reset time, only how long it waited.
			age := derefInt(optInt(h, "waited_s"))
			if hr.ResetAt != nil {
				age = now - *hr.ResetAt
			}
			items = append(items, AttentionItem{
				ID: fmt.Sprintf("%s:%s:%s", KindHoldMatured, c.ID, hr.ID), Crew: c.ID, Origin: OriginLocal,
				Severity: SeverityWarning, Kind: KindHoldMatured, Message: msg,
				Clears: clearsText(KindHoldMatured, clearsCtx{}), AgeSec: age, Action: &act,
			})
		}
	}

	sortAttention(items)
	return items
}

// sortAttention orders items by severity, then oldest first, then id.
func sortAttention(items []AttentionItem) {
	slices.SortFunc(items, func(a, b AttentionItem) int {
		return cmp.Or(
			cmp.Compare(severityRank[a.Severity], severityRank[b.Severity]),
			cmp.Compare(b.AgeSec, a.AgeSec),
			cmp.Compare(a.ID, b.ID),
		)
	})
}

var (
	ledgerItemRe = regexp.MustCompile(`^(\S+) (\w+)\(`)
	ledgerIDRe   = regexp.MustCompile(`^[A-Za-z]+(\d+)-(\d+)$`)
)

// ledgerCount counts acceptance items in a pr_open detail; a run id such as
// AC2-4 stands for 4-2+1 items, and pass and waived items are done.
func ledgerCount(detail string) (done, total int, ok bool) {
	for item := range strings.SplitSeq(detail, "; ") {
		m := ledgerItemRe.FindStringSubmatch(strings.TrimSpace(item))
		if m == nil {
			continue
		}
		n := 1
		if r := ledgerIDRe.FindStringSubmatch(m[1]); r != nil {
			var lo, hi int
			_, _ = fmt.Sscan(r[1], &lo)
			_, _ = fmt.Sscan(r[2], &hi)
			if hi >= lo {
				n = hi - lo + 1
			}
		}
		total += n
		if m[2] == "pass" || m[2] == "waived" {
			done += n
		}
	}
	return done, total, total > 0
}

// reapKeep is crew reap's keep reason for w: its own crew's row first, then
// the branch's newest one.
func (in Inputs) reapKeep(w WorkerRow) string {
	if r, ok := in.ReapKeeps[data.WorkerKey{Crew: w.Crew, Branch: w.Branch}]; ok {
		return r
	}
	return in.ReapKeeps[data.WorkerKey{Branch: w.Branch}]
}
