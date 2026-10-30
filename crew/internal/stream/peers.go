// The peer link: the repo stream's two goroutines over the peer store. One
// syncs with every peer: true host that maps this clone, at once and then every
// peer.SyncEvery; the other announces each new peer msg in the store, whether a
// sync pulled it or a peer's own sync pushed it.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// peerHooks are the peer link's seams, in Options so a test can supply them.
// Zero fields take the production defaults: the settings from dispatch-config,
// ssh, peer.SyncEvery, the stream's --interval and peer.SelfName.
type peerHooks struct {
	hosts       func() (map[string]remote.Host, error)
	transport   peer.Transport
	every, wake time.Duration
	self        string
}

// peerMsgLine announces one peer msg; its fields are the stored row's.
type peerMsgLine struct {
	Stream string `json:"stream"`
	Crew   string `json:"crew"`
	Peer   string `json:"peer"`
	From   string `json:"from"`
	Body   string `json:"body"`
	TS     int64  `json:"ts"`
}

// peerLink writes only through out, the loop's line-locked writer. sup is
// shared by both goroutines, so supMu guards it.
type peerLink struct {
	out         io.Writer
	paths       bus.Paths
	store       peer.Store
	crew        string
	heartbeatMS int64
	now         func() int64
	hooks       peerHooks

	supMu sync.Mutex
	sup   *suppress

	// seen, the wake goroutine's own, is each host's announced offset as this
	// link knows it, so a failing WriteSeen does not re-announce every tick.
	seen map[string]int64

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	wg     sync.WaitGroup
}

func newPeerLink(l *loop, paths bus.Paths) *peerLink {
	ctx, cancel := context.WithCancel(context.Background())
	h := l.o.peer
	if h.hosts == nil {
		h.hosts = func() (map[string]remote.Host, error) {
			return remote.Load(ctx, remote.ConfigBin(), io.Discard)
		}
	}
	if h.transport == nil {
		h.transport = peer.SSHTransport
	}
	if h.every == 0 {
		h.every = peer.SyncEvery
	}
	if h.wake == 0 {
		h.wake = time.Duration(l.c.interval) * time.Second
	}
	return &peerLink{
		out:         l.stdout,
		paths:       paths,
		store:       peer.StoreFor(paths),
		crew:        l.c.crew,
		heartbeatMS: l.heartbeatMS(),
		now:         l.nowMS,
		hooks:       h,
		sup:         &suppress{key: map[string]string{}, ts: map[string]int64{}},
		seen:        map[string]int64{},
		ctx:         ctx,
		cancel:      cancel,
		stop:        make(chan struct{}),
	}
}

func (pl *peerLink) start() {
	pl.wg.Add(2)
	go pl.every(pl.hooks.every, pl.syncAll)
	go pl.every(pl.hooks.wake, pl.wake)
}

// shutdown stops both goroutines, killing an in-flight transport through its
// context, and waits for them.
func (pl *peerLink) shutdown() {
	close(pl.stop)
	pl.cancel()
	pl.wg.Wait()
}

// every runs f at once and then every d until shutdown.
func (pl *peerLink) every(d time.Duration, f func()) {
	defer pl.wg.Done()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		f()
		select {
		case <-pl.stop:
			return
		case <-t.C:
		}
	}
}

// syncAll is one round with each peer host that maps this clone. A clone with
// no origin slug, or no dispatch-config to read settings from, is silent: a
// dispatcher without peers sees no new lines. Settings that will not load are
// reported, as they silently end the link and let peer claims age out.
func (pl *peerLink) syncAll() {
	slug := remote.CloneSlug(pl.ctx, pl.paths.Common)
	if slug == "" {
		return
	}
	hosts, err := pl.hooks.hosts()
	if errors.Is(err, exec.ErrNotFound) {
		return
	}
	if err != nil {
		pl.report("settings", "load", err.Error())
		return
	}
	names := make([]string, 0, len(hosts))
	for name, h := range hosts {
		if h.Peer {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if pl.ctx.Err() != nil {
			return
		}
		target, err := remote.Resolve(hosts, name, slug)
		if err != nil {
			continue
		}
		res, err := peer.Sync(pl.ctx, peer.SyncOptions{
			Paths:     pl.paths,
			Self:      pl.hooks.self,
			Name:      name,
			Target:    target,
			Slug:      slug,
			Transport: pl.hooks.transport,
		})
		if err != nil {
			if pl.ctx.Err() == nil {
				pl.report(name, "sync", err.Error())
			}
			continue
		}
		if n, reasons := tally(res.Dropped); n > 0 {
			pl.report(name, "dropped", fmt.Sprintf("dropped %d rows (%s)", n, reasons))
		}
		if n, reasons := tally(res.RemoteDropped); n > 0 {
			pl.report(name, "refused", fmt.Sprintf("peer refused %d of our rows (%s)", n, reasons))
		}
	}
}

// tally is the total of counts and each reason with its count, in reason order.
func tally(counts map[string]int) (int, string) {
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	n := 0
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		n += counts[r]
		parts[i] = fmt.Sprintf("%s %d", r, counts[r])
	}
	return n, strings.Join(parts, ", ")
}

// wake announces the msgs past each host's seen offset, then advances it under
// the host's lock; a busy lock waits for the next tick.
func (pl *peerLink) wake() {
	entries, err := os.ReadDir(pl.store.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		host, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !e.Type().IsRegular() || !remote.HostNameRe.MatchString(host) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() <= pl.seenOf(host) {
			continue
		}
		pl.announce(host)
	}
}

func (pl *peerLink) seenOf(host string) int64 {
	return max(pl.store.Seen(host), pl.seen[host])
}

func (pl *peerLink) announce(host string) {
	unlock, ok := pl.store.Lock(host)
	if !ok {
		return
	}
	defer unlock()
	seen := pl.seenOf(host)
	msgs, next, err := peer.Messages(pl.store, host, seen)
	if err != nil {
		pl.report(host, "wake", err.Error())
		return
	}
	for _, m := range msgs {
		say(pl.out, "%s\n", encode(peerMsgLine{
			Stream: "peer_msg",
			Crew:   pl.crew,
			Peer:   m.Peer,
			From:   m.From,
			Body:   m.Body,
			TS:     m.TS,
		}))
	}
	if next == seen {
		return
	}
	pl.seen[host] = next
	if err := pl.store.WriteSeen(host, next); err != nil {
		pl.report(host, "wake", err.Error())
	}
}

// report is an error line naming the host, one per cause per --heartbeat. Each
// kind of failure is suppressed on its own, so a round that both drops rows
// and has its rows refused does not alternate the two lines.
func (pl *peerLink) report(host, kind, detail string) {
	detail = "peer " + host + ": " + detail
	ts := pl.now()
	pl.supMu.Lock()
	fresh := pl.sup.repeat(host+" "+kind, digitsToN(detail), ts, pl.heartbeatMS)
	pl.supMu.Unlock()
	if !fresh {
		return
	}
	say(pl.out, "%s\n", encode(errorLine{
		Stream: "error",
		Crew:   pl.crew,
		Host:   host,
		RC:     1,
		Detail: detail,
		TS:     ts,
	}))
}
