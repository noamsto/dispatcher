package cases

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

// fake is a scripted engine: onSend and onRespond decide what the "child" does.
type fake struct {
	opts      event.Options
	events    chan event.Event
	onSend    func(f *fake, text string)
	onRespond func(f *fake, r *event.Request, d event.Decision)

	mu      sync.Mutex
	session string
	closed  bool
}

func newFake(o event.Options) *fake {
	return &fake{opts: o, events: make(chan event.Event, 256), session: o.SessionID}
}

func (f *fake) emit(ev event.Event) {
	ev.At = time.Now()
	f.events <- ev
}

func (f *fake) setSession(id string) {
	f.mu.Lock()
	f.session = id
	f.mu.Unlock()
	f.emit(event.Event{Kind: event.KindInit, SessionID: id})
}

func (f *fake) Start(context.Context) error { return nil }
func (f *fake) Events() <-chan event.Event  { return f.events }
func (f *fake) Send(text string) error      { f.onSend(f, text); return nil }
func (f *fake) Interrupt() error            { f.emit(event.Event{Kind: event.KindTurnEnd}); return nil }
func (f *fake) Kill() error                 { return f.Close() }
func (f *fake) Wait() error                 { return nil }
func (f *fake) SessionID() string           { f.mu.Lock(); defer f.mu.Unlock(); return f.session }

func (f *fake) Respond(r *event.Request, d event.Decision) error {
	f.onRespond(f, r, d)
	return nil
}

func (f *fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	f.events <- event.Event{Kind: event.KindExit, At: time.Now()}
	close(f.events)
	return nil
}

// newEnv builds a CaseEnv whose engines are fakes configured by mk.
func newEnv(t *testing.T, engine string, mk func(*CaseEnv, *fake)) (*CaseEnv, *[]*fake) {
	t.Helper()
	var made []*fake
	ce := &CaseEnv{Engine: engine, Work: t.TempDir(), Outside: t.TempDir(), State: t.TempDir()}
	ce.Factory = func(o event.Options) (event.Engine, error) {
		f := newFake(o)
		mk(ce, f)
		made = append(made, f)
		return f, nil
	}
	return ce, &made
}

var touchArg = regexp.MustCompile(`touch (\S+)`)

// bashScript models a child that asks permission for `touch X`, honours an
// allow_session verdict for later calls, and reports a tool result.
func bashScript(ce *CaseEnv, f *fake) {
	sessionAllowed := false
	f.onSend = func(f *fake, text string) {
		m := touchArg.FindStringSubmatch(text)
		cmd := "touch " + m[1]
		f.emit(event.Event{Kind: event.KindTurnStart})
		f.emit(event.Event{Kind: event.KindToolStart, Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: cmd}})
		if sessionAllowed {
			_ = os.WriteFile(filepath.Join(ce.Work, m[1]), nil, 0o600)
			f.emit(event.Event{Kind: event.KindToolResult})
			f.emit(event.Event{Kind: event.KindTurnEnd})
			return
		}
		f.emit(event.Event{Kind: event.KindPermission, Tool: "Bash", Req: &event.Request{ID: "r-" + m[1], Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: cmd}}})
	}
	f.onRespond = func(f *fake, r *event.Request, d event.Decision) {
		if d.Verdict == "deny" {
			f.emit(event.Event{Kind: event.KindToolResult, IsError: true, Text: "blocked: " + d.Reason})
		} else {
			if d.Verdict == "allow_session" {
				sessionAllowed = true
			}
			_ = os.WriteFile(filepath.Join(ce.Work, strings.TrimPrefix(r.Detail.Command, "touch ")), nil, 0o600)
			f.emit(event.Event{Kind: event.KindToolResult})
		}
		f.emit(event.Event{Kind: event.KindTurnEnd})
	}
}

func runCaseT(t *testing.T, name string, ce *CaseEnv) string {
	t.Helper()
	c, ok := Lookup(name)
	if !ok {
		t.Fatalf("case %q not registered", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg, err := c.Run(ctx, ce)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return msg
}

func TestBashCases(t *testing.T) {
	for _, name := range []string{"bash-allow", "bash-deny", "bash-session"} {
		t.Run(name, func(t *testing.T) {
			ce, _ := newEnv(t, "claude", bashScript)
			msg := runCaseT(t, name, ce)
			if !strings.Contains(msg, "permission bash 'touch ") {
				t.Fatalf("message lacks the evidence: %q", msg)
			}
		})
	}
}

func TestBashFailsWithoutPermission(t *testing.T) {
	ce, _ := newEnv(t, "claude", func(_ *CaseEnv, f *fake) {
		f.onSend = func(f *fake, _ string) {
			f.emit(event.Event{Kind: event.KindToolStart, Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: "touch allowed.txt"}})
			f.emit(event.Event{Kind: event.KindToolResult})
			f.emit(event.Event{Kind: event.KindTurnEnd})
		}
	})
	c, _ := Lookup("bash-allow")
	if _, err := c.Run(context.Background(), ce); err == nil || !strings.Contains(err.Error(), "no bash Permission") {
		t.Fatalf("want a missing-Permission failure, got %v", err)
	}
}

// askEnv scripts an ask round trip; result is the ask tool's own result text
// and reply the assistant text after it.
func askEnv(t *testing.T, engine, result, reply string, got *map[string]string) *CaseEnv {
	ask := toolsFor(engine).Ask
	ce, _ := newEnv(t, engine, func(_ *CaseEnv, f *fake) {
		f.onSend = func(f *fake, _ string) {
			f.emit(event.Event{Kind: event.KindToolStart, Tool: ask, ToolUseID: "q"})
			f.emit(event.Event{Kind: event.KindPermission, Req: &event.Request{ID: "q", Tool: ask, Detail: event.Detail{
				Kind: "ask", Options: []string{"Red", "Blue", "Teal"}, Questions: []byte(`[{"question":"Which colour?"}]`),
			}}})
		}
		f.onRespond = func(f *fake, _ *event.Request, d event.Decision) {
			*got = d.Answers
			f.emit(event.Event{Kind: event.KindToolResult, ToolUseID: "q", Text: result})
			f.emit(event.Event{Kind: event.KindText, Text: reply})
			f.emit(event.Event{Kind: event.KindTurnEnd})
		}
	})
	return ce
}

func TestAsk(t *testing.T) {
	for engine, wantKey := range map[string]string{"claude": "Which colour?", "pi": "answer"} {
		t.Run(engine, func(t *testing.T) {
			var got map[string]string
			runCaseT(t, "ask", askEnv(t, engine, "User answered: Teal", "You picked Teal.", &got))
			if got[wantKey] != "Teal" {
				t.Fatalf("answers = %v, want key %q = Teal", got, wantKey)
			}
		})
	}
}

func TestAskEmptyResultFallsBackToText(t *testing.T) {
	var got map[string]string
	runCaseT(t, "ask", askEnv(t, "pi", "", "You answered Teal.", &got))
}

func TestAskFailsWhenAnswerIsLost(t *testing.T) {
	for name, tc := range map[string]struct{ result, reply string }{
		"result without teal, text guesses":  {"no answer", "You picked Red."},
		"result without teal, text echoes":   {"no answer", "Red, Blue or Teal?"},
		"empty result, text lists all":       {"", "Red, Blue or Teal?"},
		"empty result, text names other one": {"", "You picked Blue."},
	} {
		t.Run(name, func(t *testing.T) {
			var got map[string]string
			ce := askEnv(t, "pi", tc.result, tc.reply, &got)
			c, _ := Lookup("ask")
			if _, err := c.Run(context.Background(), ce); err == nil {
				t.Fatal("want a lost-answer failure, got a pass")
			}
		})
	}
}

// planEnv scripts claude plan mode: plan Permission, then a Write that the
// policy must approve; writeErr makes that Write's result an error.
func planEnv(t *testing.T, writeErr bool) *CaseEnv {
	ce, _ := newEnv(t, "claude", func(ce *CaseEnv, f *fake) {
		f.onSend = func(f *fake, _ string) {
			f.emit(event.Event{Kind: event.KindTurnStart})
			f.emit(event.Event{Kind: event.KindPermission, Detail: event.Detail{Kind: "plan"}, Req: &event.Request{ID: "p", Tool: "ExitPlanMode", Detail: event.Detail{Kind: "plan"}}})
		}
		f.onRespond = func(f *fake, r *event.Request, _ event.Decision) {
			if r.ID == "p" {
				f.emit(event.Event{Kind: event.KindToolResult, ToolUseID: "p"})
				f.emit(event.Event{Kind: event.KindToolStart, Tool: "Write", ToolUseID: "w", Detail: event.Detail{Kind: "write", Path: "plan.txt"}})
				f.emit(event.Event{Kind: event.KindPermission, Detail: event.Detail{Kind: "write", Path: "plan.txt"}, Req: &event.Request{ID: "w", Tool: "Write", Detail: event.Detail{Kind: "write", Path: "plan.txt"}}})
				return
			}
			if !writeErr {
				_ = os.WriteFile(filepath.Join(ce.Work, "plan.txt"), []byte("planned"), 0o600)
			}
			f.emit(event.Event{Kind: event.KindToolResult, ToolUseID: "w", IsError: writeErr, Text: "write result"})
			f.emit(event.Event{Kind: event.KindTurnEnd})
		}
	})
	return ce
}

func TestPlanWaitsForApprovedWrite(t *testing.T) {
	runCaseT(t, "plan", planEnv(t, false))
}

func TestPlanFailsWhenWriteFails(t *testing.T) {
	c, _ := Lookup("plan")
	if _, err := c.Run(context.Background(), planEnv(t, true)); err == nil || !strings.Contains(err.Error(), "post-approval Write") {
		t.Fatalf("want a failed-write failure, got %v", err)
	}
}

func TestMidturn(t *testing.T) {
	ce, _ := newEnv(t, "claude", func(ce *CaseEnv, f *fake) {
		bash := func(id, cmd string) {
			f.emit(event.Event{Kind: event.KindToolStart, Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: cmd}})
			f.emit(event.Event{Kind: event.KindPermission, Req: &event.Request{ID: id, Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: cmd}}})
		}
		f.onSend = func(f *fake, text string) {
			if strings.Contains(text, "midturn.txt") {
				bash("m2", "touch midturn.txt")
				return
			}
			f.emit(event.Event{Kind: event.KindTurnStart})
			bash("m1", "sleep 3")
		}
		f.onRespond = func(f *fake, r *event.Request, _ event.Decision) {
			f.emit(event.Event{Kind: event.KindToolResult})
			if r.ID == "m2" {
				_ = os.WriteFile(filepath.Join(ce.Work, "midturn.txt"), nil, 0o600)
				f.emit(event.Event{Kind: event.KindTurnEnd})
			}
		}
	})
	msg := runCaseT(t, "midturn", ce)
	if !strings.Contains(msg, "injected") {
		t.Fatalf("want injected, got %q", msg)
	}
}

func TestKillResume(t *testing.T) {
	ce, made := newEnv(t, "claude", func(_ *CaseEnv, f *fake) {
		f.onRespond = func(*fake, *event.Request, event.Decision) {}
		f.onSend = func(f *fake, text string) {
			switch {
			case f.opts.Resume:
				f.setSession(f.opts.SessionID)
				f.emit(event.Event{Kind: event.KindText, Text: "PLUM-7731"})
				f.emit(event.Event{Kind: event.KindTurnEnd})
			case strings.Contains(text, "slow.sh 120"):
				f.emit(event.Event{Kind: event.KindToolStart, Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: "sh slow.sh 120"}})
			default:
				f.setSession("sess-1")
				f.emit(event.Event{Kind: event.KindText, Text: "ok"})
				f.emit(event.Event{Kind: event.KindTurnEnd})
			}
		}
	})
	msg := runCaseT(t, "kill-resume", ce)
	if !strings.Contains(msg, "equals the original") {
		t.Fatalf("message = %q", msg)
	}
	if n := len(*made); n != 2 || !(*made)[1].opts.Resume || (*made)[1].opts.SessionID != "sess-1" {
		t.Fatalf("want a resumed second engine on sess-1, got %d engines", n)
	}
}

// slowEnv scripts a child that starts `sh slow.sh` and, when blocked is set,
// immediately reports a refusal as the tool result.
func slowEnv(t *testing.T, blocked bool) *CaseEnv {
	ce, _ := newEnv(t, "claude", func(ce *CaseEnv, f *fake) {
		f.onRespond = func(*fake, *event.Request, event.Decision) {}
		f.onSend = func(f *fake, text string) {
			if !strings.Contains(text, "slow.sh") {
				f.emit(event.Event{Kind: event.KindToolStart, Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: "touch steered.txt"}})
				_ = os.WriteFile(filepath.Join(ce.Work, "steered.txt"), nil, 0o600)
				f.emit(event.Event{Kind: event.KindToolResult})
				f.emit(event.Event{Kind: event.KindTurnEnd})
				return
			}
			f.emit(event.Event{Kind: event.KindToolStart, Tool: "Bash", ToolUseID: "u1", Detail: event.Detail{Kind: "bash", Command: "sh slow.sh 60"}})
			if blocked {
				f.emit(event.Event{Kind: event.KindToolResult, ToolUseID: "u1", IsError: true, Text: "<tool_use_error>Blocked: standalone sleep 60"})
			}
		}
	})
	return ce
}

func TestInterruptFailsWhenToolAlreadyFinished(t *testing.T) {
	ce := slowEnv(t, true)
	c, _ := Lookup("interrupt")
	_, err := c.Run(context.Background(), ce)
	if err == nil || !strings.Contains(err.Error(), "tool finished before interrupt: <tool_use_error>Blocked") {
		t.Fatalf("want the finished-before-interrupt failure, got %v", err)
	}
}

func TestInterruptHappyPath(t *testing.T) {
	ce := slowEnv(t, false)
	msg := runCaseT(t, "interrupt", ce)
	if !strings.Contains(msg, "slow.sh running at interrupt") || !strings.Contains(msg, "no ToolResult") {
		t.Fatalf("message = %q", msg)
	}
	if !exists(filepath.Join(ce.Work, "slow.sh")) {
		t.Fatal("slow.sh was not written")
	}
}

func TestRegistered(t *testing.T) {
	want := []string{"bash-allow", "bash-deny", "bash-session", "outside-write", "outside-read", "mcp", "web", "ask", "plan", "error", "midturn", "interrupt", "hang", "kill-resume", "bash-deny-hook", "bash-deny-mcptool"}
	for _, n := range want {
		if _, ok := Lookup(n); !ok {
			t.Errorf("case %s not registered", n)
		}
	}
	for _, n := range []string{"bash-deny-hook", "bash-deny-mcptool"} {
		c, _ := Lookup(n)
		if c.Applies("pi") || !c.Applies("claude") {
			t.Errorf("%s must be claude-only", n)
		}
	}
}
