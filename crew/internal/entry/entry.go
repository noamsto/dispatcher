// Package entry holds the crew.sh arms that run ahead of the repo check: the
// help screens and their routing, and the four subcommands that need neither a
// repo nor a crew id — id, new, engine-cmd and dash (#1007).
//
// They are one package because they are one thing in crew.sh: the entry
// section, every arm of which execs this binary before `bus.Locate` is ever
// called. The screens are the bytes crew.sh cat'ed from its heredocs; the guard
// that they are still those bytes is scripts/help-parity.sh, which runs the two
// sides against each other from git — this change deleted the script's copy, so
// no in-tree drift guard could hold it.
package entry

import (
	"fmt"
	"io"
)

const (
	exitFailure = 1
	// exitCantExec and exitNotExec are the two statuses a shell reports for a
	// program it could not run: 127 when PATH has no such command, 126 when the
	// file is there but cannot be executed.
	exitCantExec = 127
	exitNotExec  = 126
)

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
