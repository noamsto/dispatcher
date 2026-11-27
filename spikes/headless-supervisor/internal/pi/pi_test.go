package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

func TestMain(m *testing.M) {
	if os.Getenv("HSUP_PI_FAKE") == "1" {
		fakeChild()
		return
	}
	os.Exit(m.Run())
}

// fakeChild speaks just enough pi RPC for the round-trip test.
func fakeChild() {
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	read := func() map[string]any {
		if !sc.Scan() {
			os.Exit(0)
		}
		var m map[string]any
		_ = json.Unmarshal(sc.Bytes(), &m)
		return m
	}
	for {
		m := read()
		switch m["type"] {
		case "get_state":
			_ = out.Encode(map[string]any{"type": "response", "command": "get_state", "success": true, "data": map[string]any{"sessionId": "fake-sess"}})
		case "prompt":
			_ = out.Encode(map[string]any{"type": "agent_start"})
			_ = out.Encode(map[string]any{"type": "extension_ui_request", "id": "p1", "method": "select",
				"title":   `hsup-permission {"kind":"bash","tool":"bash","command":"touch x","input":{"command":"touch x"}}`,
				"options": []string{"allow_once", "allow_session", "deny"}})
			r := read()
			text := fmt.Sprint(r["value"])
			if r["value"] == "deny" {
				_ = out.Encode(map[string]any{"type": "extension_ui_request", "id": "p2", "method": "input", "title": "hsup-deny-reason"})
				text += ":" + fmt.Sprint(read()["value"])
			}
			_ = out.Encode(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}})
			_ = out.Encode(map[string]any{"type": "agent_settled", "aborted": false})
		}
	}
}

func decodeFile(t *testing.T, name string) []event.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var d decoder
	var evs []event.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		evs = append(evs, d.decode(sc.Bytes(), time.Unix(0, 0)).events...)
	}
	return evs
}

func kinds(evs []event.Event) []event.Kind {
	var k []event.Kind
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestDecodeRun(t *testing.T) {
	evs := decodeFile(t, "run.jsonl")
	want := []event.Kind{event.KindInit, event.KindTurnStart, event.KindText, event.KindToolStart, event.KindToolResult, event.KindTurnEnd}
	if !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("kinds = %v, want %v", kinds(evs), want)
	}
	if evs[0].SessionID != "sess-1" {
		t.Errorf("session id = %q", evs[0].SessionID)
	}
	if evs[2].Text != "Hello" {
		t.Errorf("text = %q", evs[2].Text)
	}
	if evs[3].Detail.Kind != "bash" || evs[3].Detail.Command != "ls -la" || evs[3].ToolUseID != "c1" {
		t.Errorf("tool start = %+v", evs[3])
	}
	if evs[4].Text != "out" || evs[4].IsError {
		t.Errorf("tool result = %+v", evs[4])
	}
}

func TestDecodePermissions(t *testing.T) {
	evs := decodeFile(t, "permission.jsonl")
	// u2 (deny reason) and u3 (notify) are not surfaced.
	if len(evs) != 4 {
		t.Fatalf("got %d events: %v", len(evs), kinds(evs))
	}
	perm, ask, plan, other := evs[0], evs[1], evs[2], evs[3]
	if perm.Req.ID != "u1" || perm.Detail.Kind != "bash" || perm.Detail.Command != "touch x" || perm.Tool != "bash" {
		t.Errorf("permission = %+v", perm)
	}
	if string(perm.Req.Input) != `{"command":"touch x"}` {
		t.Errorf("input = %s", perm.Req.Input)
	}
	if ask.Detail.Kind != "ask" || !reflect.DeepEqual(ask.Detail.Options, []string{"red", "blue"}) {
		t.Errorf("ask = %+v", ask.Detail)
	}
	if plan.Detail.Kind != "plan" || len(plan.Detail.Options) != 3 {
		t.Errorf("plan = %+v", plan.Detail)
	}
	if other.Detail.Kind != "other" || other.Req.ID != "u6" {
		t.Errorf("other = %+v", other)
	}
}

func TestDecodeErrors(t *testing.T) {
	evs := decodeFile(t, "error.jsonl")
	want := []event.Kind{event.KindRetry, event.KindError, event.KindError, event.KindError}
	if !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	if evs[0].Text != "529 overloaded" || evs[1].Text != "529 overloaded" || evs[2].Text != "boom" {
		t.Errorf("texts = %q %q %q", evs[0].Text, evs[1].Text, evs[2].Text)
	}
	if !strings.Contains(evs[3].Text, "bad") {
		t.Errorf("response error = %q", evs[3].Text)
	}
}

func TestRespondFields(t *testing.T) {
	bash := &event.Request{ID: "x", Detail: event.Detail{Kind: "bash"}}
	ask := &event.Request{ID: "x", Detail: event.Detail{Kind: "ask"}}
	plan := &event.Request{ID: "x", Detail: event.Detail{Kind: "plan", Options: []string{"Execute the plan", "Stay in plan mode"}}}
	cases := []struct {
		name   string
		r      *event.Request
		method string
		d      event.Decision
		want   map[string]any
	}{
		{"allow_once", bash, "select", event.Decision{Verdict: "allow_once"}, map[string]any{"value": "allow_once"}},
		{"allow_session", bash, "select", event.Decision{Verdict: "allow_session"}, map[string]any{"value": "allow_session"}},
		{"deny", bash, "select", event.Decision{Verdict: "deny", Reason: "no"}, map[string]any{"value": "deny"}},
		{"ask", ask, "select", event.Decision{Verdict: "answer", Answers: map[string]string{"Pick": "red"}}, map[string]any{"value": "red"}},
		{"plan option", plan, "select", event.Decision{Verdict: "answer", Answers: map[string]string{"option": "Stay in plan mode"}}, map[string]any{"value": "Stay in plan mode"}},
		{"plan allow", plan, "select", event.Decision{Verdict: "allow_once"}, map[string]any{"value": "Execute the plan"}},
		{"confirm", bash, "confirm", event.Decision{Verdict: "deny"}, map[string]any{"confirmed": false}},
		{"ask unanswered", ask, "select", event.Decision{Verdict: "deny"}, map[string]any{"cancelled": true}},
	}
	for _, c := range cases {
		if got := responseFields(c.r, c.method, c.d); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestSendPromptVsSteer(t *testing.T) {
	var buf strings.Builder
	eng, err := New(event.Options{Bin: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	e := eng.(*engine)
	e.stdin = nopCloser{&buf}
	if err := e.Send("one"); err != nil {
		t.Fatal(err)
	}
	e.handle([]byte(`{"type":"agent_start"}`))
	if err := e.Send("two"); err != nil {
		t.Fatal(err)
	}
	e.handle([]byte(`{"type":"agent_settled","aborted":false}`))
	if err := e.Send("three"); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		_ = json.Unmarshal([]byte(l), &m)
		if m["type"] != "get_state" {
			types = append(types, fmt.Sprint(m["type"], ":", m["message"]))
		}
	}
	if want := []string{"prompt:one", "steer:two", "prompt:three"}; !reflect.DeepEqual(types, want) {
		t.Fatalf("got %v want %v", types, want)
	}
}

type nopCloser struct{ *strings.Builder }

func (nopCloser) Close() error { return nil }

func TestArgs(t *testing.T) {
	t.Setenv("HSUP_PI_MODEL", "")
	base := event.Options{Work: "/w", SessionID: "s1", GatePath: "/g/gate.ts"}
	args := Args(base)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--mode rpc", "--model anthropic/claude-haiku-4-5", "--session-dir /w/.pi-sessions",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files",
		"-e /g/gate.ts", "--session-id s1", "--no-mcp",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	if strings.Contains(joined, "--approve") {
		t.Errorf("unexpected --approve: %v", args)
	}

	full := base
	full.Resume, full.Approve, full.MCP = true, true, true
	full.Model = "stub/stub-model"
	full.Extensions = []string{"/x/errstub.ts", "/x/plan.ts"}
	full.ExtraArgs = []string{"--plan"}
	joined = strings.Join(Args(full), " ")
	for _, want := range []string{"--model stub/stub-model", "-e builtin:mcp -e /x/errstub.ts -e /x/plan.ts -e /g/gate.ts", "--session-id s1", "--approve"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "--no-mcp") {
		t.Errorf("--no-mcp with MCP set: %s", joined)
	}
	if !strings.HasSuffix(joined, "--plan") {
		t.Errorf("--plan not last: %s", joined)
	}
}

func TestChildEnv(t *testing.T) {
	parent := []string{
		"PATH=/bin", "CREW_WORKER_ID=w", "CREW_ID=c", "CREW_LEAD_X=1", "TMUX=t", "TMUX_PANE=%1",
		"CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_EXECPATH=/x", "CLAUDE_CODE_SUBAGENT_EFFORT=high",
		"CLAUDE_CODE_SSE_PORT=9", "KEEP=1",
	}
	got := childEnv(parent, []string{"EXTRA=2"})
	want := []string{"PATH=/bin", "KEEP=1", "EXTRA=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestResolveBinEnv(t *testing.T) {
	t.Setenv("HSUP_PI_BIN", "/some/pi")
	if b, err := ResolveBin(); err != nil || b != "/some/pi" {
		t.Fatalf("got %q %v", b, err)
	}
}

func TestResolveBinWrapper(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nbin=/nix/store/aaa-pi-1.6.1/bin/pi\nif [ -z \"$X\" ]; then\n  bin=/nix/store/bbb-pi-bolt-compiled/bin/pi-bolt\nfi\nexec \"$bin\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HSUP_PI_BIN", "")
	t.Setenv("PATH", dir)
	if b, err := ResolveBin(); err != nil || b != "/nix/store/aaa-pi-1.6.1/bin/pi" {
		t.Fatalf("got %q %v", b, err)
	}
}

func TestFakeChildRoundTrip(t *testing.T) {
	for _, verdict := range []event.Verdict{"allow_once", "deny"} {
		t.Run(string(verdict), func(t *testing.T) {
			dir := t.TempDir()
			tw, err := transcript.New(filepath.Join(dir, "t.jsonl"), &transcript.Scrubber{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tw.Close() }()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			eng, err := New(event.Options{Bin: exe, Work: dir, Env: []string{"HSUP_PI_FAKE=1"}, Transcript: tw})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := eng.Start(ctx); err != nil {
				t.Fatal(err)
			}
			sent := false
			var final string
			for ev := range eng.Events() {
				switch ev.Kind {
				case event.KindInit:
					if eng.SessionID() != "fake-sess" {
						t.Errorf("session = %q", eng.SessionID())
					}
					if !sent {
						sent = true
						if err := eng.Send("go"); err != nil {
							t.Fatal(err)
						}
					}
				case event.KindPermission:
					if err := eng.Respond(ev.Req, event.Decision{Verdict: verdict, Reason: "because"}); err != nil {
						t.Fatal(err)
					}
				case event.KindText:
					final = ev.Text
				case event.KindTurnEnd:
					if err := eng.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := string(verdict)
			if verdict == "deny" {
				want = "deny:because"
			}
			if final != want {
				t.Errorf("final text = %q, want %q", final, want)
			}
			if err := eng.Wait(); err != nil {
				t.Errorf("wait: %v", err)
			}
		})
	}
}

func decodeLines(t *testing.T, lines ...string) []event.Event {
	t.Helper()
	var d decoder
	var evs []event.Event
	for _, l := range lines {
		evs = append(evs, d.decode([]byte(l), time.Unix(0, 0)).events...)
	}
	return evs
}

func lastTurnEnd(t *testing.T, evs []event.Event) event.Event {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].Kind != event.KindTurnEnd {
		t.Fatalf("last event is not turn_end: %v", kinds(evs))
	}
	return evs[len(evs)-1]
}

func TestTurnEndError(t *testing.T) {
	start := `{"type":"agent_start"}`
	settled := `{"type":"agent_settled","aborted":false}`
	errEnd := `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"429 rate limited","content":[]}}`
	retryFail := `{"type":"auto_retry_end","success":false,"finalError":"gave up"}`
	abortEnd := `{"type":"message_end","message":{"role":"assistant","stopReason":"aborted","content":[]}}`

	if te := lastTurnEnd(t, decodeLines(t, start, errEnd, settled)); !te.IsError || te.Text != "429 rate limited" {
		t.Errorf("error run = %+v", te)
	}
	if te := lastTurnEnd(t, decodeLines(t, start, retryFail, settled)); !te.IsError || te.Text != "gave up" {
		t.Errorf("retry-failed run = %+v", te)
	}
	if te := lastTurnEnd(t, decodeLines(t, start, abortEnd, `{"type":"agent_settled","aborted":true}`)); !te.IsError || te.Text != "aborted" {
		t.Errorf("aborted run = %+v", te)
	}
	if te := lastTurnEnd(t, decodeLines(t, start, `{"type":"agent_settled","aborted":true}`)); !te.IsError {
		t.Errorf("aborted flag only = %+v", te)
	}
	if te := lastTurnEnd(t, decodeLines(t, start, settled)); te.IsError || te.Text != "" {
		t.Errorf("clean run = %+v", te)
	}
	// The error clears on the next agent_start.
	evs := decodeLines(t, start, errEnd, settled, start, settled)
	if te := lastTurnEnd(t, evs); te.IsError {
		t.Errorf("run after error = %+v", te)
	}
}

func TestExampleExtension(t *testing.T) {
	got := ExampleExtension("/nix/store/x-pi-1.6.1/bin/pi", "plan-mode")
	want := "/nix/store/x-pi-1.6.1/libexec/pi/examples/extensions/plan-mode/index.ts"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCancelClosesChildGracefully(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(event.Options{Bin: exe, Work: t.TempDir(), Env: []string{"HSUP_PI_FAKE=1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Well inside graceWait: the child saw EOF on stdin, not SIGKILL.
	deadline := time.After(graceWait / 2)
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				t.Fatal("events closed without Exit")
			}
			if ev.Kind != event.KindExit {
				continue
			}
			if strings.Contains(ev.Text, "signal") {
				t.Fatalf("exit %q, want a clean EOF exit", ev.Text)
			}
			return
		case <-deadline:
			t.Fatal("child still running after ctx cancel")
		}
	}
}
