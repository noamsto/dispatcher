package stack

import (
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gh"
)

// Probes are the arm's side effects that are not git — every git call goes
// through internal/wtgit, the one anchored invocation.
type Probes struct {
	// Gh is `gh …` as the arm ran it inside `$(…)`: stdout with trailing
	// newlines stripped, the status as an error. The arm left the child's
	// stderr on the terminal and never read it; this discards it, so a failed
	// gh surfaces as the arm's own refusal line alone.
	Gh gh.Client
	// Append is `_bus_append`.
	Append func(path, line string) error
	// Now stamps the row: the arm's `jq -n 'now*1000|floor'` is real time,
	// never $CREW_CLOCK.
	Now func() time.Time
}

// DefaultProbes is the production probe set.
func DefaultProbes() Probes {
	return Probes{
		Gh:     gh.Exec{},
		Append: bus.Append,
		Now:    time.Now,
	}
}

// Options is what `crew stack` needs beyond its arguments and streams.
type Options struct {
	Probes Probes
}

// withDefaults fills any probe a caller left unset, so a test that scripts gh
// alone still gets a real bus append and clock.
func (p Probes) withDefaults() Probes {
	d := DefaultProbes()
	if p.Gh == nil {
		p.Gh = d.Gh
	}
	if p.Append == nil {
		p.Append = d.Append
	}
	if p.Now == nil {
		p.Now = d.Now
	}
	return p
}
