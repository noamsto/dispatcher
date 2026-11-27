package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/claude"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/pi"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
)

// serveOptions resolves what serve records in session.json so a TUI can
// mirror the child: the real binary and gate (fillBin), the effective model
// (read back from the adapter's argv so defaults stay in one place) and, for
// pi, the session dir. Other engines launch with o as fillBin leaves it.
func serveOptions(engine string, o event.Options, gate string) (event.Options, string, error) {
	if err := fillBin(engine, &o, gate); err != nil {
		return o, "", fmt.Errorf("serve: %w", err)
	}
	var args []string
	switch engine {
	case "claude":
		args = claude.Args(o)
	case "pi":
		args = pi.Args(o)
	default:
		return o, "", nil
	}
	o.Model = argValue(args, "--model")
	return o, argValue(args, "--session-dir"), nil
}

func argValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// printSession writes session.json as key=value lines for takeover.sh. A value
// holding a control character would break the line format, so it is refused.
func printSession(w io.Writer, s supervisor.Session) error {
	kv := [][2]string{
		{"engine", s.Engine}, {"session_id", s.SessionID}, {"work", s.Work},
		{"bin", s.Bin}, {"model", s.Model}, {"session_dir", s.SessionDir}, {"gate", s.Gate},
	}
	for _, p := range kv {
		if strings.ContainsFunc(p[1], func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("session: %s holds a control character", p[0])
		}
	}
	for _, p := range kv {
		_, _ = fmt.Fprintf(w, "%s=%s\n", p[0], p[1])
	}
	return nil
}
