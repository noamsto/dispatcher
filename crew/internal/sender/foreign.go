package sender

import (
	"strings"
	"sync"
	"time"
)

// retryAfter is how long a failed host-name or settings read stands before a
// later row reads it again: watch re-filters the whole log on every poll, so a
// read per row would run the settings once per overseer row.
const retryAfter = 30 * time.Second

// OverseerHosts decides which overseer:<host> senders a dispatcher reads: this
// host and the settings hosts keys. This host's name is read on the first
// overseer: row, the settings only on the first overseer:<h≠self> row; a
// successful read is kept, a failed one refuses every such row with its cause
// and is retried once retryAfter has passed. A nil *OverseerHosts knows no
// host, so every overseer: sender is foreign (self included). It is safe for
// concurrent use.
type OverseerHosts struct {
	p   Probes
	now func() time.Time

	mu    sync.Mutex
	self  cached[string]
	hosts cached[map[string]bool]
}

func NewOverseerHosts(p Probes) *OverseerHosts {
	return &OverseerHosts{p: p, now: time.Now}
}

// cached is a read kept once it succeeds; a failure stands for retryAfter.
type cached[T any] struct {
	val    T
	ok     bool
	err    error
	failed time.Time
}

func (c *cached[T]) get(now time.Time, read func() (T, error)) (T, error) {
	if c.ok || c.err != nil && now.Sub(c.failed) < retryAfter {
		return c.val, c.err
	}
	v, err := read()
	if err != nil {
		c.err, c.failed = err, now
		return c.val, err
	}
	c.val, c.ok, c.err = v, true, nil
	return v, nil
}

// Foreign reports the host of an overseer:<host> sender and whether it is
// outside this host and the settings hosts. Any other sender is ("", false).
func (h *OverseerHosts) Foreign(from string) (string, bool) {
	host, ok := strings.CutPrefix(from, "overseer:")
	if !ok {
		return "", false
	}
	return host, h.refusal(host) != ""
}

// refusal is why rows from overseer:<host> are not read, "" when they are.
func (h *OverseerHosts) refusal(host string) string {
	notIn := "overseer host " + sanitize(host) + " is not in settings hosts"
	if h == nil {
		return notIn
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	self, err := h.self.get(now, h.p.self)
	if err != nil {
		return "this host's name cannot be read (" + printable(err.Error()) + ")"
	}
	if host == self {
		return ""
	}
	hosts, err := h.hosts.get(now, h.p.hostSet)
	if err != nil {
		return "settings hosts cannot be read (" + printable(err.Error()) + ")"
	}
	if !hosts[host] {
		return notIn
	}
	return ""
}

func (p Probes) hostSet() (map[string]bool, error) {
	if p.Hosts == nil {
		return nil, errNoHosts
	}
	hosts, err := p.Hosts()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(hosts))
	for name := range hosts {
		set[name] = true
	}
	return set, nil
}

// ForeignOverseer returns the row a session reads in place of a row from a
// foreign overseer:<host> (its body dropped, the refusal stated) and true, or
// the row unchanged and false. The input row is never mutated.
func ForeignOverseer(row map[string]any, h *OverseerHosts) (map[string]any, bool) {
	from, ok := row["from"].(string)
	if !ok {
		return row, false
	}
	host, ok := strings.CutPrefix(from, "overseer:")
	if !ok {
		return row, false
	}
	reason := h.refusal(host)
	if reason == "" {
		return row, false
	}
	out := map[string]any{
		"ts":      row["ts"],
		"kind":    "msg",
		"crew_id": row["crew_id"],
		"from":    sanitize(from),
		"body":    nil,
		"refused": reason,
	}
	if to, ok := row["to"]; ok {
		out["to"] = to
	}
	return out, true
}

const sanitizeCap = 200

// sanitize keeps [A-Za-z0-9:/#._@-], writes '?' for any other rune, and caps
// the result at sanitizeCap bytes, so a hostile sender cannot inject terminal
// controls or bulk into a reader.
func sanitize(s string) string {
	return keep(s, func(r rune) bool {
		return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune(":/#._@-", r)
	})
}

// printable is sanitize for an error text: it keeps every printable ASCII rune.
func printable(s string) string {
	return keep(s, func(r rune) bool { return ' ' <= r && r <= '~' })
}

func keep(s string, ok func(rune) bool) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() == sanitizeCap {
			break
		}
		if ok(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
