package sender

import (
	"strings"
	"sync"
)

// OverseerHosts decides which overseer:<host> senders a dispatcher reads: this
// host and the settings hosts keys. This host's name is read on the first
// overseer: row, the settings only on the first overseer:<h≠self> row, each
// once; an unreadable host name or settings makes every such row foreign. A nil
// *OverseerHosts knows no host, so every overseer: sender is foreign (self
// included); callers that must admit self build one with Probes.Self set.
type OverseerHosts struct {
	p Probes

	selfOnce sync.Once
	self     string
	selfOK   bool

	hostsOnce sync.Once
	hosts     map[string]bool
}

func NewOverseerHosts(p Probes) *OverseerHosts {
	return &OverseerHosts{p: p}
}

// Foreign reports the host of an overseer:<host> sender and whether it is
// outside this host and the settings hosts. Any other sender is ("", false).
func (h *OverseerHosts) Foreign(from string) (string, bool) {
	host, ok := strings.CutPrefix(from, "overseer:")
	if !ok {
		return "", false
	}
	if h == nil {
		return host, true
	}
	h.selfOnce.Do(func() {
		self, err := h.p.self()
		h.self, h.selfOK = self, err == nil
	})
	if !h.selfOK {
		return host, true
	}
	if host == h.self {
		return host, false
	}
	h.hostsOnce.Do(func() {
		if h.p.Hosts == nil {
			return
		}
		hosts, err := h.p.Hosts()
		if err != nil {
			return
		}
		h.hosts = make(map[string]bool, len(hosts))
		for name := range hosts {
			h.hosts[name] = true
		}
	})
	return host, !h.hosts[host]
}

// ForeignOverseer returns the row a session reads in place of a row from a
// foreign overseer:<host> (its body dropped, the refusal stated) and true, or
// the row unchanged and false. The input row is never mutated.
func ForeignOverseer(row map[string]any, h *OverseerHosts) (map[string]any, bool) {
	from, ok := row["from"].(string)
	if !ok {
		return row, false
	}
	host, foreign := h.Foreign(from)
	if !foreign {
		return row, false
	}
	out := map[string]any{
		"ts":      row["ts"],
		"kind":    "msg",
		"crew_id": row["crew_id"],
		"from":    sanitize(from),
		"body":    nil,
		"refused": "overseer host " + sanitize(host) + " is not in settings hosts",
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
	var b strings.Builder
	for _, r := range s {
		if b.Len() == sanitizeCap {
			break
		}
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune(":/#._@-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
