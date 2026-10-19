// Package proctest is a scripted proc.Table for tests.
package proctest

import (
	"context"
	"maps"
	"slices"

	"github.com/noamsto/dispatcher/crew/internal/proc"
)

var _ proc.Table = (*Table)(nil)

// Proc is one live process; a zero field reads as "ps cannot say".
type Proc struct {
	Parent  int
	Comm    string
	Args    string
	Cwd     string
	Elapsed int64 // seconds
}

// Table is a scripted proc.Table: a pid is alive when it is a key of Procs,
// and Kill only records.
type Table struct {
	Procs  map[int]Proc
	killed []int
}

// New is a Table of the live pids plus each parent link, which makes both ends
// of the link processes too.
func New(live map[int]bool, parents map[int]int) *Table {
	t := &Table{Procs: map[int]Proc{}}
	for pid, ok := range live {
		if ok {
			t.Procs[pid] = Proc{}
		}
	}
	for pid, parent := range parents {
		t.Procs[pid] = Proc{Parent: parent}
	}
	return t
}

func (t *Table) Alive(pid int) bool { _, ok := t.Procs[pid]; return ok }

func (t *Table) Elapsed(pid int) (int64, bool) { e := t.Procs[pid].Elapsed; return e, e != 0 }

func (t *Table) Parent(pid int) (int, bool) { p := t.Procs[pid].Parent; return p, p != 0 }

func (t *Table) Comm(pid int) (string, bool) { c := t.Procs[pid].Comm; return c, c != "" }

func (t *Table) Args(pid int) string { return t.Procs[pid].Args }

// Cwds lists the processes with a Cwd, by pid.
func (t *Table) Cwds(context.Context) []proc.Cwd {
	var out []proc.Cwd
	for _, pid := range slices.Sorted(maps.Keys(t.Procs)) {
		if dir := t.Procs[pid].Cwd; dir != "" {
			out = append(out, proc.Cwd{PID: pid, Dir: dir})
		}
	}
	return out
}

func (t *Table) Kill(pid int) { t.killed = append(t.killed, pid) }

// Killed returns the pids Kill was called with, in order.
func (t *Table) Killed() []int { return t.killed }
