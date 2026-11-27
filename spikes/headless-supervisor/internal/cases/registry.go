// Package cases holds the scripted verification cases. Each case file registers
// itself from init; the CLI looks cases up by name.
package cases

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

// DefaultEngines are the engines a case runs on when Case.Engines is empty.
var DefaultEngines = []string{"claude", "pi"}

// CaseEnv is everything a case needs from the CLI. Factory fills Work and
// Transcript on the options a case passes when it leaves them empty.
type CaseEnv struct {
	Engine     string
	Work       string // child cwd, fresh per run
	Outside    string // a directory outside Work, for outside-read/write cases
	State      string // supervisor state dir (session.json, lock, socket)
	Transcript *transcript.Writer
	Factory    supervisor.Factory
	MCPEcho    string // path of the mcpecho binary (HSUP_MCPECHO)
	Gate       string // path of pi/gate.ts (HSUP_GATE)
}

// Case is one scripted check. Run returns the assertion that held, or an error
// saying why it did not.
type Case struct {
	Name    string
	Engines []string // empty means DefaultEngines
	Run     func(ctx context.Context, env *CaseEnv) (string, error)
}

// Applies reports whether the case is defined for engine.
func (c Case) Applies(engine string) bool {
	engines := c.Engines
	if len(engines) == 0 {
		engines = DefaultEngines
	}
	return slices.Contains(engines, engine)
}

var (
	mu       sync.Mutex
	registry = map[string]Case{}
)

// Register adds a case; it panics on an empty or duplicate name, since that is
// a programming error caught at start-up.
func Register(c Case) {
	mu.Lock()
	defer mu.Unlock()
	if c.Name == "" || c.Run == nil {
		panic("cases: Register needs a name and a Run func")
	}
	if _, dup := registry[c.Name]; dup {
		panic(fmt.Sprintf("cases: duplicate case %q", c.Name))
	}
	registry[c.Name] = c
}

// Lookup returns the case registered under name.
func Lookup(name string) (Case, bool) {
	mu.Lock()
	defer mu.Unlock()
	c, ok := registry[name]
	return c, ok
}

// All returns every case sorted by name.
func All() []Case {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Case, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Case) int { return cmp.Compare(a.Name, b.Name) })
	return out
}
