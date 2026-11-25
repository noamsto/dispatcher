package tmuxtest

import (
	"regexp"
	"strings"
)

// Window is one window of a Server. Opts are its own user options.
type Window struct {
	ID, Index, Session, Name string
	// SessionID is the session's id; "" is "$0". A window linked into several
	// sessions is one Window per session, sharing its ID.
	SessionID string
	Opts      map[string]string
	Panes     []Pane
}

// Pane is one pane of a Window. Opts are its own user options; a read of one it
// lacks falls through to its window's, as tmux's does.
type Pane struct {
	ID, Index, Command string
	Opts               map[string]string
}

// Server is a scripted tmux server that answers the reads the crew commands make
// (`list-windows`, `list-panes`, `show-options -wqv|-pqv`, `display-message -p`)
// from a model of windows and panes. It expands a format exactly as tmux does, an
// option or name value raw, so a value holding a tab or a newline prints extra
// fields or rows: that is what a test of a forged option asserts against.
type Server struct {
	Windows []Window
	// Identity answers `display-message -p '#{pid}:#{start_time}'`.
	Identity string
	// FailWindows and FailPanes make that list fail, as a dead server does.
	FailWindows, FailPanes bool
}

// Fake is a Fake that records its calls and answers from s.
func (s *Server) Fake() *Fake { return &Fake{Func: s.Run} }

var formatToken = regexp.MustCompile(`#\{([^}]*)\}`)

// Run answers one argv, its output cut of trailing newlines as tmux.Exec cuts it; a
// call it does not model fails with ErrNoServer.
func (s *Server) Run(args []string) (string, error) {
	out, err := s.run(args)
	return strings.TrimRight(out, "\n"), err
}

func (s *Server) run(args []string) (string, error) {
	if len(args) == 0 {
		return "", ErrNoServer
	}
	switch args[0] {
	case "list-windows":
		if s.FailWindows {
			return "", ErrNoServer
		}
		var rows []string
		for i := range s.Windows {
			rows = append(rows, s.expand(args[len(args)-1], &s.Windows[i], nil))
		}
		return strings.Join(rows, "\n"), nil
	case "list-panes":
		if s.FailPanes {
			return "", ErrNoServer
		}
		var rows []string
		for i := range s.Windows {
			for j := range s.Windows[i].Panes {
				rows = append(rows, s.expand(args[len(args)-1], &s.Windows[i], &s.Windows[i].Panes[j]))
			}
		}
		return strings.Join(rows, "\n"), nil
	case "show-options":
		if len(args) != 5 || args[1] != "-wqv" && args[1] != "-pqv" || args[2] != "-t" {
			return "", ErrNoServer
		}
		w, p, ok := s.find(args[3])
		if !ok {
			return "", ErrNoServer
		}
		if args[1] == "-wqv" {
			p = nil
		}
		if p != nil {
			return p.Opts[args[4]], nil
		}
		return w.Opts[args[4]], nil
	case "display-message":
		fmtArg := args[len(args)-1]
		if len(args) == 3 && args[1] == "-p" {
			return s.Identity, nil
		}
		if len(args) != 5 || args[1] != "-p" || args[2] != "-t" {
			return "", ErrNoServer
		}
		w, p, ok := s.find(args[3])
		if !ok {
			return "", ErrNoServer
		}
		return s.expand(fmtArg, w, p), nil
	}
	return "", ErrNoServer
}

// find is the window or pane a target names: `@7`, `%3` or `$1:@7`. A bare window
// id that is linked into several sessions names the last of them, as tmux picks a
// session of its own for it; `$1:@7` names that session's winlink.
func (s *Server) find(id string) (*Window, *Pane, bool) {
	if sid, wid, ok := strings.Cut(id, ":"); ok && strings.HasPrefix(sid, "$") {
		for i := range s.Windows {
			if s.Windows[i].ID == wid && s.sessionID(&s.Windows[i]) == sid {
				return &s.Windows[i], nil, true
			}
		}
		return nil, nil, false
	}
	for i := len(s.Windows) - 1; i >= 0; i-- {
		if s.Windows[i].ID == id {
			return &s.Windows[i], nil, true
		}
	}
	for i := range s.Windows {
		for j := range s.Windows[i].Panes {
			if s.Windows[i].Panes[j].ID == id {
				return &s.Windows[i], &s.Windows[i].Panes[j], true
			}
		}
	}
	return nil, nil, false
}

func (s *Server) sessionID(w *Window) string {
	if w.SessionID == "" {
		return "$0"
	}
	return w.SessionID
}

func (s *Server) expand(format string, w *Window, p *Pane) string {
	return formatToken.ReplaceAllStringFunc(format, func(tok string) string {
		name := tok[2 : len(tok)-1]
		if strings.HasPrefix(name, "@") {
			if p != nil {
				if v, ok := p.Opts[name]; ok {
					return v
				}
			}
			return w.Opts[name]
		}
		switch name {
		case "window_id":
			return w.ID
		case "window_index":
			return w.Index
		case "session_id":
			return s.sessionID(w)
		case "session_name":
			return w.Session
		case "window_name":
			return w.Name
		}
		if p != nil {
			switch name {
			case "pane_id":
				return p.ID
			case "pane_index":
				return p.Index
			case "pane_current_command":
				return p.Command
			}
		}
		return ""
	})
}
