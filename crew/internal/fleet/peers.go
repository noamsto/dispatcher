package fleet

import (
	"errors"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// peerRows are the peer rows of the repo at common, minus those that collide
// with a local crew id or branch. A read error leaves no rows; dropped is
// counted either way.
func peerRows(common string, hosts func() (map[string]remote.Host, error), events []bus.Event, now time.Time) (rows []map[string]any, dropped int, err error) {
	store := peer.StoreFor(bus.PathsFor(common))
	rows, err = peer.RosterRows(store, hosts, now)
	rows, dropped = peer.DropLocal(rows, events)
	return rows, dropped, err
}

// PeerStatus is a repo's peer store as `fleet status` shows it.
type PeerStatus struct {
	Links   []PeerLink       `json:"links"`
	Rows    []map[string]any `json:"rows"`
	Dropped int              `json:"dropped"`
}

// PeerLink is one host's last exchange, in ms; null is never.
type PeerLink struct {
	Host   string  `json:"host"`
	OKTS   *int64  `json:"ok_ts"`
	ErrTS  *int64  `json:"err_ts"`
	Detail *string `json:"detail"`
}

// peerStatusOf reads the peer store of the repo at common. A read error gives
// empty lists and its text: peers never fail the document.
func peerStatusOf(common string, hosts func() (map[string]remote.Host, error), events []bus.Event, now time.Time) (*PeerStatus, *string) {
	p := &PeerStatus{Links: []PeerLink{}, Rows: []map[string]any{}}
	rows, dropped, err := peerRows(common, hosts, events, now)
	links, lerr := peer.StoreFor(bus.PathsFor(common)).Links()
	if err = errors.Join(err, lerr); err != nil {
		msg := oneLine(err)
		return p, &msg
	}
	for _, l := range links {
		pl := PeerLink{Host: l.Host, OKTS: msOrNil(l.OK), ErrTS: msOrNil(l.Err)}
		if l.Detail != "" {
			pl.Detail = &l.Detail
		}
		p.Links = append(p.Links, pl)
	}
	p.Rows = append(p.Rows, rows...)
	p.Dropped = dropped
	return p, nil
}

func msOrNil(ms int64) *int64 {
	if ms == 0 {
		return nil
	}
	return &ms
}
