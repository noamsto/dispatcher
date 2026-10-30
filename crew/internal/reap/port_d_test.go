package reap

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	portDIdleTail = "────────────────── reef ─\n❯\n──────────────────\n  -- INSERT -- ⏵⏵ auto mode on · ← for agents"
	portDDone     = "  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)\n✻ Churned for 36s · done 11:20 AM\n"
	portDKeepNote = "crew reap: keeping feat/r — an engine is still running there ("
	portDLive     = "live turn, unsent input, or no idle input box"
	portDPlainKey = "capture-pane -p -t %1"
	portDColorKey = "capture-pane -e -p -t %1"
)

// portDFx is a done worker whose PR is merged, with one pane running engine
// at its worktree showing frame (plain and colored alike).
func portDFx(t *testing.T, engine, frame string) *fx {
	t.Helper()
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh[portAStateKey] = "MERGED"
	f.tmux[panesKey] = "@1\t%1\t" + engine + "\t" + f.wt
	f.tmux[portDPlainKey] = frame
	f.tmux[portDColorKey] = frame
	return f
}

func (f *fx) portDWantKept() {
	f.t.Helper()
	if !f.portAExists(f.wt) {
		f.t.Fatal("worktree removed")
	}
	f.portAWantNoCall("tmux kill-window")
	f.portAWantNoCall("gtrash")
}

// Frames that are not provably idle keep the worker's window and worktree,
// each for its own reason.
func TestPortDIdleEngineKept(t *testing.T) {
	cases := []struct{ name, engine, frame, reason string }{
		{"meter frame is a live turn", "claude",
			"✳ Perusing… (1m 2s · ↓ 3.1k tokens)\n" + portDIdleTail, portDLive},
		{"esc to interrupt above an empty box", "claude",
			"✻ Working (2s · esc to interrupt)\n" + portDIdleTail, portDLive},
		{"thinking turn with a todo list between it and the box", "claude",
			"✻ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)\n  ⎿  ☐ Read the reap arm\n     ☐ Reuse the idle classifier\n" +
				"     ☐ Re-sample before the kill\n     ☐ Wait on the reap lock\n     ☐ Run the suite\n" + portDIdleTail, portDLive},
		{"unsent input in the box", "claude",
			portDDone + "────────────────── reef ─\n❯ push it\n──────────────────\n  -- INSERT -- ⏵⏵ auto mode on · ← for agents", portDLive},
		{"live subagent row above a finished-looking box", "claude",
			"✻ Churned for 36s · done 11:20 AM\n  ◯ general-purpose  Revise spec per critic                              2m 3s · ↓ 71.5k tokens\n" + portDIdleTail, portDLive},
		{"option-select prompt", "claude",
			"  2. Gate everything on 3.8\n     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.\n  3. Require 3.8, drop legacy\n  4. Type something.\n" +
				"──────────────────────────────────────────────────────────────────────────\n  5. Chat about this\n\n" +
				"Enter to select · Tab/Arrow keys to navigate · Esc to cancel", "prompt on screen"},
		{"background shell still running", "claude",
			"✻ Churned for 36s · done 11:20 AM · 1 shell still running\n────────────────── reef ─\n❯\n──────────────────\n  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents",
			"background shell or monitor still running"},
		{"non-claude engine", "codex",
			"──────────────────────────────\n❯\n──────────────────────────────", "codex has no idle signature"},
		{"pi pane with no footer row under its box", "pi",
			"──────────────────────────────\n❯\n──────────────────────────────", "pi frame: live turn or no idle input box"},
		{"fresh boot, no finished-turn marker", "claude",
			"╭───────────────────────────────────────────╮\n│ ✻ Welcome to Claude Code!                 │\n╰───────────────────────────────────────────╯\n" +
				"────────────────── reef ─\n❯\n──────────────────\n  ? for shortcuts", "no finished-turn marker above the input box"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := portDFx(t, c.engine, c.frame)
			if rc := f.reap(f.main, Request{}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			f.wantOut(portDKeepNote+c.reason+")", "crew reap: nothing reclaimed")
			f.portDWantKept()
		})
	}
}

// The colored capture tells a real unsent draft from a dimmed prompt
// suggestion: the draft keeps the worker, the ghost text does not.
func TestPortDDraftVersusGhostText(t *testing.T) {
	plain := portDDone + "────────────────── reef ─\n❯ push it\n──────────────────\n  -- INSERT --"
	t.Run("real draft is kept", func(t *testing.T) {
		f := portDFx(t, "claude", plain)
		f.tmux[portDColorKey] = portDDone + "────────────────── reef ─\n❯ push it\n──────────────────\n  -- INSERT --"
		f.portBOK(Request{})
		f.wantOut(portDKeepNote+portDLive+")", "crew reap: nothing reclaimed")
		f.portDWantKept()
	})
	t.Run("dimmed suggestion is reaped", func(t *testing.T) {
		f := portDFx(t, "claude", plain)
		f.tmux[portDColorKey] = portDDone + "────────────────── reef ─\n❯ \x1b[2mpush it\x1b[0m\n──────────────────\n  -- INSERT --"
		f.portBOK(Request{})
		f.wantOut("crew reap: killed window @1 (feat/r idle engine)", "crew reap: reaped feat/r (MERGED)",
			"crew reap: kept local branch feat/r — could not verify the merged PR head")
		if !slices.Contains(f.calls, "tmux kill-window -t @1") {
			t.Fatalf("calls %q", f.calls)
		}
	})
}

// Workers whose engine is idle, or gone, are reclaimed; the verdict does not
// depend on the locale or on a trailing NBSP in the input box.
func TestPortDIdleEngineReaped(t *testing.T) {
	idle := portDDone + portDIdleTail
	cases := []struct {
		name, engine, frame, state, lcAll string
		window                            bool
	}{
		{"idle claude frame", "claude", idle, "done", "", true},
		{"pr_open worker whose PR merged", "claude", idle, "pr_open", "", true},
		{"LC_ALL=C", "claude", idle, "done", "C", true},
		{"trailing NBSP in the input box", "claude",
			portDDone + "────────────────── reef ─\n❯  \n──────────────────\n  -- INSERT -- ⏵⏵ auto mode on · ← for agents", "done", "", true},
		{"no engine pane", "fish", "", "done", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.lcAll != "" {
				t.Setenv("LC_ALL", c.lcAll)
			}
			f := portDFx(t, c.engine, c.frame)
			f.bus(status(c.state, prURL, 0))
			if rc := f.reap(f.main, Request{}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			want := []string{"crew reap: reaped feat/r (MERGED)", "crew reap: kept local branch feat/r — could not verify the merged PR head"}
			if c.window {
				want = slices.Insert(want, 0, "crew reap: killed window @1 (feat/r idle engine)")
			}
			f.wantOut(want...)
			if killed := slices.Contains(f.calls, "tmux kill-window -t @1"); killed != c.window {
				t.Fatalf("kill-window @1 = %v, want %v: %q", killed, c.window, f.calls)
			}
			if f.portAExists(f.wt) {
				t.Fatal("worktree not removed")
			}
		})
	}
}

// An idle engine frame does not override the other keep gates.
func TestPortDIdleEngineOtherGatesKeep(t *testing.T) {
	idle := portDDone + portDIdleTail
	t.Run("open PR", func(t *testing.T) {
		f := portDFx(t, "claude", idle)
		f.gh[portAStateKey] = "OPEN"
		f.portBOK(Request{})
		f.wantOut("crew reap: keeping feat/r — PR OPEN", "crew reap: nothing reclaimed")
		f.portDWantKept()
	})
	t.Run("tracked modification", func(t *testing.T) {
		f := portDFx(t, "claude", idle)
		f.portBCommit("tracked.txt", "tracked\n")
		if err := os.WriteFile(filepath.Join(f.wt, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.portBOK(Request{})
		f.wantOut("crew reap: keeping feat/r — uncommitted changes", "crew reap: nothing reclaimed")
		f.portDWantKept()
	})
	t.Run("exited state with a live idle engine", func(t *testing.T) {
		f := portDFx(t, "claude", idle)
		f.bus(status("exited", prURL, 0))
		f.portBOK(Request{})
		f.wantOut("crew reap: keeping feat/r — an engine is still running there (exited session)", "crew reap: nothing reclaimed")
		f.portDWantKept()
	})
}

// A pane that differs between reap's two samples keeps everything.
func TestPortDPaneChangesBetweenSamplesKept(t *testing.T) {
	idle := portDDone + portDIdleTail
	moved := "✻ Churned for 36s · done 11:20 AM\n> go on\n✻ Churned for 2s · done 11:21 AM\n" + portDIdleTail
	f := portDFx(t, "claude", idle)
	plain := 0
	rc := portCReap(f, f.main, Request{}, func(p *Probes) {
		p.Tmux = &tmuxtest.Fake{Func: func(args []string) (string, error) {
			key := strings.Join(args, " ")
			f.record("tmux " + key)
			if key == portDPlainKey {
				plain++
				if plain >= 2 {
					return moved, nil
				}
			}
			return f.tmux[key], nil
		}}
	})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/r — pane changed between samples", "crew reap: nothing reclaimed")
	f.portDWantKept()
}

// A live reap lock is waited out by default, then the reap does its work.
func TestPortDWaitsOutLiveLock(t *testing.T) {
	f := portDFx(t, "fish", "")
	ld := f.paths.Dir + "/reap.lock.d"
	if !lock.Acquire(ld, "1") {
		t.Fatal("could not plant the reap lock")
	}
	t.Cleanup(func() { lock.Release(ld) })
	sleeps := 0
	rc := portCReap(f, f.main, Request{}, func(p *Probes) {
		p.Sleep = func(time.Duration) {
			if sleeps++; sleeps == 2 {
				lock.Release(ld)
			}
		}
	})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if sleeps != 2 {
		t.Fatalf("slept %d times, want 2", sleeps)
	}
	f.wantOut("crew reap: reaped feat/r (MERGED)", "crew reap: kept local branch feat/r — could not verify the merged PR head")
}
