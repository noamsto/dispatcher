package data

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	sshProbeTTL     = 60 * time.Second
	maxSuggested    = 20
	zeroTailscaleTS = "0001-01-01T00:00:00Z"
)

// probeTimeout bounds one ssh or tailscale probe; a var so tests can shorten it.
var probeTimeout = 10 * time.Second

// HostConfig is one hosts.<name> settings entry.
type HostConfig struct {
	Name      string  `json:"name"`
	SSH       string  `json:"ssh"`
	Peer      bool    `json:"peer"`
	Tailscale *string `json:"tailscale"`
}

// HostReach is one configured host's reachability. Online nil means unknown.
type HostReach struct {
	Host     string  `json:"host"`
	Source   string  `json:"source"`
	Online   *bool   `json:"online"`
	LastSeen *int64  `json:"last_seen"`
	Error    *string `json:"error"`
}

// SuggestedHost is a tailnet peer that matches no configured host.
type SuggestedHost struct {
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

// HostsFromSettings reads hosts.<name>.{ssh,peer,tailscale} leaves, sorted by
// name. Any hosts.<name> leaf makes the host exist.
func HostsFromSettings(rows []SettingRow) []HostConfig {
	byName := map[string]*HostConfig{}
	for _, row := range rows {
		if len(row.Path) < 3 || row.Path[0] != "hosts" {
			continue
		}
		h := byName[row.Path[1]]
		if h == nil {
			h = &HostConfig{Name: row.Path[1]}
			byName[h.Name] = h
		}
		if len(row.Path) != 3 {
			continue
		}
		switch row.Path[2] {
		case "ssh":
			_ = json.Unmarshal(row.Value, &h.SSH)
		case "peer":
			_ = json.Unmarshal(row.Value, &h.Peer)
		case "tailscale":
			var s string
			if json.Unmarshal(row.Value, &s) == nil && s != "" {
				h.Tailscale = &s
			}
		}
	}
	out := make([]HostConfig, 0, len(byName))
	for _, h := range byName {
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b HostConfig) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// TailscaleEnabled reports whether the dashTailscale setting is JSON true.
func TailscaleEnabled(rows []SettingRow) bool {
	for _, row := range rows {
		if len(row.Path) == 1 && row.Path[0] == "dashTailscale" {
			var on bool
			return json.Unmarshal(row.Value, &on) == nil && on
		}
	}
	return false
}

// TailscaleNode is one node of `tailscale status --json`. The strings are
// untrusted text, kept verbatim.
type TailscaleNode struct {
	HostName string
	DNSName  string
	Online   bool
	LastSeen *int64 // unix s; nil for the zero time or an unparseable value
}

// TailscaleDoc is the parsed Self node and the peers sorted by HostName.
type TailscaleDoc struct {
	Self  TailscaleNode
	Peers []TailscaleNode
}

type tailscaleWire struct {
	Self *tailscaleNodeWire           `json:"Self"`
	Peer map[string]tailscaleNodeWire `json:"Peer"`
}

type tailscaleNodeWire struct {
	HostName string `json:"HostName"`
	DNSName  string `json:"DNSName"`
	Online   bool   `json:"Online"`
	LastSeen string `json:"LastSeen"`
}

func (n tailscaleNodeWire) node() TailscaleNode {
	out := TailscaleNode{HostName: n.HostName, DNSName: n.DNSName, Online: n.Online}
	if n.LastSeen != "" && n.LastSeen != zeroTailscaleTS {
		if t, err := time.Parse(time.RFC3339Nano, n.LastSeen); err == nil {
			s := t.Unix()
			out.LastSeen = &s
		}
	}
	return out
}

// ParseTailscale decodes `tailscale status --json`. A document without a
// Self node (logged out, stopped) is an error: it cannot answer reachability.
func ParseTailscale(raw []byte) (TailscaleDoc, error) {
	var w tailscaleWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return TailscaleDoc{}, err
	}
	if w.Self == nil {
		return TailscaleDoc{}, errors.New("tailscale status has no Self")
	}
	doc := TailscaleDoc{Self: w.Self.node()}
	for _, p := range w.Peer {
		doc.Peers = append(doc.Peers, p.node())
	}
	slices.SortFunc(doc.Peers, func(a, b TailscaleNode) int {
		return strings.Compare(a.HostName+"\x00"+a.DNSName, b.HostName+"\x00"+b.DNSName)
	})
	return doc, nil
}

func (n TailscaleNode) matches(name string) bool {
	label, _, _ := strings.Cut(n.DNSName, ".")
	return strings.EqualFold(n.HostName, name) || strings.EqualFold(label, name)
}

// ReachFromTailscale resolves each configured host against the tailnet and
// lists the peers no configured host claims.
func ReachFromTailscale(hosts []HostConfig, doc TailscaleDoc) ([]HostReach, []SuggestedHost) {
	reach := make([]HostReach, 0, len(hosts))
	claimed := map[int]bool{}
	for _, h := range hosts {
		want := h.Name
		if h.Tailscale != nil {
			want = *h.Tailscale
		}
		re := HostReach{Host: h.Name, Source: "tailscale"}
		node, ok := TailscaleNode{}, false
		if doc.Self.matches(want) {
			node, ok = doc.Self, true
		}
		for i, p := range doc.Peers {
			if p.matches(want) {
				claimed[i] = true
				if !ok {
					node, ok = p, true
				}
			}
		}
		if !ok {
			msg := "not in tailnet"
			re.Error = &msg
		} else {
			online := node.Online
			re.Online, re.LastSeen = &online, node.LastSeen
		}
		reach = append(reach, re)
	}
	suggested := []SuggestedHost{}
	for i, p := range doc.Peers {
		if claimed[i] {
			continue
		}
		name := p.HostName
		if name == "" {
			name, _, _ = strings.Cut(p.DNSName, ".")
		}
		suggested = append(suggested, SuggestedHost{Name: name, Online: p.Online})
	}
	slices.SortFunc(suggested, func(a, b SuggestedHost) int { return strings.Compare(a.Name, b.Name) })
	if len(suggested) > maxSuggested {
		suggested = suggested[:maxSuggested]
	}
	return reach, suggested
}

// HostProber runs and caches ssh reachability probes; it lives across
// collects so a host is probed at most once per sshProbeTTL.
type HostProber struct {
	r   Runner
	now func() time.Time

	mu    sync.Mutex
	cache map[string]probeEntry
}

type probeEntry struct {
	at    time.Time
	reach HostReach
}

func NewHostProber(r Runner, now func() time.Time) *HostProber {
	return &HostProber{r: r, now: now, cache: map[string]probeEntry{}}
}

// Probe returns one ssh-sourced HostReach per host, in order, probing the
// uncached ones concurrently.
func (p *HostProber) Probe(ctx context.Context, hosts []HostConfig) []HostReach {
	out := make([]HostReach, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		dest := h.SSH
		if dest == "" {
			dest = h.Name
		}
		key := h.Name + "\x00" + dest
		p.mu.Lock()
		e, ok := p.cache[key]
		p.mu.Unlock()
		if ok && p.now().Sub(e.at) < sshProbeTTL {
			out[i] = e.reach
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			re, cacheable := p.probe(ctx, h.Name, dest)
			if cacheable {
				p.mu.Lock()
				p.cache[key] = probeEntry{at: p.now(), reach: re}
				p.mu.Unlock()
			}
			out[i] = re
		}()
	}
	wg.Wait()
	return out
}

// probe bounds one ssh attempt. Online is set only when ssh ran and exited;
// a missing binary, a timeout or a cancelled ctx leave it unknown. The bool
// reports whether the result is worth caching: timeouts and cancellations
// are not.
func (p *HostProber) probe(ctx context.Context, name, dest string) (HostReach, bool) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, errb, err := p.r.Run(pctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
		"-o", "ServerAliveInterval=2", "-o", "ServerAliveCountMax=1", "--", dest, "true")
	re := HostReach{Host: name, Source: "ssh"}
	setErr := func(msg string) { re.Error = &msg }
	switch {
	case err == nil:
		online := true
		re.Online = &online
	case ctx.Err() != nil:
		setErr(ctx.Err().Error())
		return re, false
	case pctx.Err() != nil:
		setErr("timeout")
		return re, false
	default:
		rc := exitCode(err)
		if rc == 127 {
			setErr(err.Error())
			break
		}
		online := false
		re.Online = &online
		setErr(firstLineOrExit(errb, rc))
	}
	return re, true
}
