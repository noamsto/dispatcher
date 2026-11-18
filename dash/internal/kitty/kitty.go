// Package kitty speaks the slice of the kitty graphics protocol crew dash
// needs to show a PNG inside a bubbletea frame: the image is stored once as a
// virtual placement and the frame carries Unicode placeholder cells that
// reference it, so the terminal draws it wherever the cells land.
package kitty

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// placeholderRune is kitty's Unicode image placeholder (U+10EEEE).
const placeholderRune = "\U0010EEEE"

// Placeholders returns rows lines of cols placeholder cells that reference
// image id. The id is the 24-bit foreground colour of each line, so it must
// reach the terminal as true colour. cols and rows are clamped to MaxCells,
// the size of the row/column diacritics table.
func Placeholders(id, cols, rows int) []string {
	cols, rows = min(cols, MaxCells), min(rows, MaxCells)
	lines := make([]string, 0, max(rows, 0))
	for r := range rows {
		var b strings.Builder
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", (id>>16)&0xff, (id>>8)&0xff, id&0xff)
		for c := range cols {
			b.WriteString(placeholderRune)
			b.WriteRune(rowColDiacritics[r])
			b.WriteRune(rowColDiacritics[c])
		}
		b.WriteString("\x1b[39m")
		lines = append(lines, b.String())
	}
	return lines
}

// Store returns the sequence that stores the PNG at path as a virtual
// placement under id, sized cols x rows cells (pass the same values given to
// Placeholders), without drawing anything. t=f makes the terminal read the
// file itself, so it only works when the terminal runs on this host. q=2
// suppresses the terminal's replies, which would otherwise arrive on stdin as
// keystrokes.
func Store(id int, path string, cols, rows int, inTmux bool) string {
	enc := base64.StdEncoding.EncodeToString([]byte(path))
	return passthrough(fmt.Sprintf("\x1b_Gi=%d,a=T,U=1,q=2,f=100,c=%d,r=%d,t=f;%s\x1b\\",
		id, cols, rows, enc), inTmux)
}

// chunkSize is the largest base64 payload kitty accepts in one direct-transfer
// escape.
const chunkSize = 4096

// StoreDirect is Store for a terminal that cannot read this host's files (ssh,
// or a mirrored remote-bridge session): t=d carries the PNG itself, base64 in
// chunkSize pieces with m=1 on every piece but the last. Each piece is its own
// escape and its own tmux passthrough, so none outgrows tmux's DCS limit.
func StoreDirect(id int, png []byte, cols, rows int, inTmux bool) string {
	enc := base64.StdEncoding.EncodeToString(png)
	var b strings.Builder
	for first := true; first || enc != ""; first = false {
		chunk := enc[:min(len(enc), chunkSize)]
		enc = enc[len(chunk):]
		more := 0
		if enc != "" {
			more = 1
		}
		keys := fmt.Sprintf("m=%d,q=2", more)
		if first {
			keys = fmt.Sprintf("i=%d,a=T,U=1,q=2,f=100,c=%d,r=%d,t=d,m=%d", id, cols, rows, more)
		}
		b.WriteString(passthrough("\x1b_G"+keys+";"+chunk+"\x1b\\", inTmux))
	}
	return b.String()
}

// Delete returns the sequence that removes image id and its placements. It is
// scoped by id so it never clears another program's images in a shared kitty
// store.
func Delete(id int, inTmux bool) string {
	return passthrough(fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id), inTmux)
}

// passthrough wraps seq so tmux forwards it to the outer terminal verbatim.
func passthrough(seq string, inTmux bool) string {
	if !inTmux {
		return seq
	}
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// ID derives a stable image id for a dash running as pane on host. Ids are 24
// bits (the placeholder foreground colour) and never zero. Two ids are used
// per dash, alternating between base+1 and base+2 so a replacement image can
// be stored before the old one is deleted; the modulus leaves room for +2.
func ID(host, pane string) int {
	h := fnv.New32a()
	h.Write([]byte(host + "\x00" + pane))
	return int(h.Sum32()%(0xFFFFFF-2)) + 1
}

// Fallback cell size in pixels when the terminal does not answer CSI 16t.
const (
	FallbackCellW = 10
	FallbackCellH = 20
)

// ParseCellPx extracts the cell size from a CSI 16 t reply, which is
// ESC [ 6 ; <height> ; <width> t (height first).
func ParseCellPx(reply string) (w, h int, ok bool) {
	_, after, ok := strings.Cut(reply, "\x1b[6;")
	if !ok {
		return 0, 0, false
	}
	body, _, ok := strings.Cut(after, "t")
	if !ok {
		return 0, 0, false
	}
	hs, ws, ok := strings.Cut(body, ";")
	if !ok {
		return 0, 0, false
	}
	h, errH := strconv.Atoi(hs)
	w, errW := strconv.Atoi(ws)
	if errH != nil || errW != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// QueryCellPx asks the terminal behind tty for its cell size in pixels and
// returns the fallback size when it does not answer within 150ms. It takes
// ownership of tty and closes it: closing is what unblocks the reader
// goroutine after a timeout, so pass a handle opened for this query alone.
func QueryCellPx(tty *os.File) (w, h int) {
	defer func() { _ = tty.Close() }()
	old, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		return FallbackCellW, FallbackCellH
	}
	defer func() { _ = term.Restore(int(tty.Fd()), old) }()

	if _, err := tty.WriteString("\x1b[16t"); err != nil {
		return FallbackCellW, FallbackCellH
	}

	ch := make(chan string, 1)
	go func() {
		var buf []byte
		b := make([]byte, 1)
		for {
			n, err := tty.Read(b)
			if n > 0 {
				buf = append(buf, b[0])
				if b[0] == 't' {
					break
				}
			}
			if err != nil {
				break
			}
		}
		ch <- string(buf)
	}()

	select {
	case s := <-ch:
		if w, h, ok := ParseCellPx(s); ok {
			return w, h
		}
	case <-time.After(150 * time.Millisecond):
	}
	return FallbackCellW, FallbackCellH
}

type Runner func(ctx context.Context, name string, args ...string) (stdout []byte, err error)

// Mode is how a capable terminal receives the image.
type Mode int

const (
	// ModeNone: the terminal cannot show the image.
	ModeNone Mode = iota
	// ModeFile: the terminal runs on this host and reads the PNG by path.
	ModeFile
	// ModeDirect: the terminal is remote (ssh, or a remote-bridge mirror), so
	// the PNG bytes travel inline.
	ModeDirect
)

// Detect reports how this terminal can show unicode-placeholder images. There
// is no capability query for placeholders, so it is a TERM allowlist (kitty
// and ghostty). Under tmux the outer terminal's name comes from the attached
// client, and tmux must also pass the graphics escapes through. A remote
// terminal (an ssh session, or under tmux the most recent client's session,
// asked of tmux) gets ModeDirect: it cannot read this host's image file, but a
// kitty-capable one decodes inline data, and a remote bridge relays it.
// When mode is ModeNone, why is a short human reason fit to show as is.
func Detect(getenv func(string) string, run Runner) (mode Mode, why string) {
	termName := getenv("TERM")
	if getenv("TMUX") == "" {
		if !kittyTerm(termName) {
			return ModeNone, "needs kitty or ghostty"
		}
		if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
			return ModeDirect, ""
		}
		return ModeFile, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := run(ctx, "tmux", "display-message", "-p", "#{client_termname}")
	if err != nil {
		return ModeNone, "cannot read the tmux client terminal: " + err.Error()
	}
	termName = strings.TrimSpace(string(out))
	if !kittyTerm(termName) {
		return ModeNone, fmt.Sprintf("tmux client terminal is %s, not kitty", termName)
	}
	out, err = run(ctx, "tmux", "show-options", "-gv", "allow-passthrough")
	if v := strings.TrimSpace(string(out)); err != nil || (v != "on" && v != "all") {
		return ModeNone, "tmux allow-passthrough is off — run: tmux set -g allow-passthrough on"
	}
	// A pane keeps the SSH_CONNECTION it was created with, so ask tmux for
	// the most recent client's. Any error, including "unknown variable",
	// means the variable is unset and the client is local.
	out, err = run(ctx, "tmux", "show-environment", "SSH_CONNECTION")
	if err == nil && strings.HasPrefix(string(out), "SSH_CONNECTION=") {
		return ModeDirect, ""
	}
	return ModeFile, ""
}

func kittyTerm(name string) bool {
	return strings.HasPrefix(name, "xterm-kitty") || strings.HasPrefix(name, "xterm-ghostty")
}
