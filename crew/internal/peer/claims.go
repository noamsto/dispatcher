package peer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/claimindex"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// evidenceBranchMax caps the branch text an evidence line quotes.
const evidenceBranchMax = 80

// Msg is a peer message read from the store.
type Msg struct {
	Peer, From, Body string
	TS               int64
}

// peerRow is the part of a stored row the readers use; admission has already
// held every field to its type.
type peerRow struct {
	TS         int64             `json:"ts"`
	Kind       string            `json:"kind"`
	Branch     string            `json:"branch"`
	Issue      json.RawMessage   `json:"issue"`
	AlsoCloses []json.RawMessage `json:"also_closes"`
	From       string            `json:"from"`
	Body       string            `json:"body"`
}

func decodeStored(line []byte) (peerRow, bool) {
	var sl storedLine
	var row peerRow
	if json.Unmarshal(line, &sl) != nil || json.Unmarshal(sl.Row, &row) != nil {
		return row, false
	}
	return row, true
}

// Messages reads the peer messages in host's store file after offset from
// (complete lines only); next is the end of the last complete line.
func Messages(s Store, host string, from int64) (msgs []Msg, next int64, err error) {
	next, err = eachLine(s.path(host, ".jsonl"), from, func(line []byte, _ int64) bool {
		if row, ok := decodeStored(line); ok && row.Kind == "msg" {
			msgs = append(msgs, Msg{Peer: host, From: row.From, Body: row.Body, TS: row.TS})
		}
		return true
	})
	if err != nil {
		return nil, 0, err
	}
	return msgs, next, nil
}

// Claims is the evidence line for a peer host's live claim on issue, or ""
// when no peer store file shows one. An unreadable store file is an error, so
// the claim gate fails closed; a missing store is no evidence.
//
// Live is a dispatch row on `feat/<issue>-*` (or whose also_closes names the
// issue) within claimindex.StaleAfter, or a claim-issue row for the issue
// within ClaimTTL, with no later reap row for its branch in the same file.
func Claims(s Store, issue string, now time.Time) (string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		host, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !remote.HostNameRe.MatchString(host) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			return "", err
		}
		if ev := hostClaim(host, data, issue, now); ev != "" {
			return ev, nil
		}
	}
	return "", nil
}

type candidate struct {
	line     int
	branch   string
	evidence string
}

// hostClaim judges one host's store file. Candidates are judged after the whole
// file is read, so a reap row later in the file cancels an earlier claim. File
// order, not seq: a seq is the peer's log offset, which restarts after the
// peer's log is reset and re-pinned.
func hostClaim(host string, data []byte, issue string, now time.Time) string {
	reaped := map[string]int{}
	var cands []candidate
	n := 0
	for line := range bytes.Lines(data) {
		n++
		row, ok := decodeStored(bytes.TrimSuffix(line, []byte("\n")))
		if !ok {
			continue
		}
		// A future ts is never live: a row stored before Admit refused one would
		// never expire.
		age := now.Sub(time.UnixMilli(row.TS))
		live := func(ttl time.Duration) bool { return age >= 0 && age <= ttl }
		switch row.Kind {
		case "reap":
			reaped[row.Branch] = n
		case "dispatch":
			if live(claimindex.StaleAfter) && (strings.HasPrefix(row.Branch, "feat/"+issue+"-") || slices.ContainsFunc(row.AlsoCloses, func(r json.RawMessage) bool { return issueText(r) == issue })) {
				cands = append(cands, candidate{n, row.Branch, "peer " + host + " dispatch row for " + capText(row.Branch, evidenceBranchMax)})
			}
		case "claim-issue":
			if live(ClaimTTL) && issueText(row.Issue) == issue {
				cands = append(cands, candidate{n, row.Branch, "peer " + host + " claim on #" + issue + " (" + capText(row.Branch, evidenceBranchMax) + ")"})
			}
		}
	}
	for _, c := range cands {
		if reaped[c.branch] < c.line {
			return c.evidence
		}
	}
	return ""
}

// issueText is an issue number as its decimal text, whether it was stored as
// a JSON string or a number.
func issueText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// capText is s cut to at most n bytes, whole runes only.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
