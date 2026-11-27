package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

const fakeEnv = "HSUP_FAKE_ACP_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		fakeAgent()
		return
	}
	os.Exit(m.Run())
}

// fakeAgent speaks just enough ACP: initialize, session/new, then one
// session/prompt with text, a tool call, an unsupported client request, a
// permission request and a result that echoes the chosen option.
func fakeAgent() {
	in := bufio.NewReader(os.Stdin)
	read := func() map[string]any {
		line, err := in.ReadString('\n')
		if err != nil {
			os.Exit(0)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			os.Exit(2)
		}
		return m
	}
	out := func(s string) { fmt.Println(s) }
	for {
		m := read()
		id := m["id"]
		switch m["method"] {
		case "initialize":
			out(fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}`, id))
		case "session/new":
			out(fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"sessionId":"s1"}}`, id))
		case "session/set_config_option":
			params, _ := m["params"].(map[string]any)
			if params["configId"] == os.Getenv("HSUP_FAKE_ACP_FAIL_CONFIG") {
				out(fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"error":{"code":-32602,"message":"bad config"}}`, id))
				continue
			}
			out(fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{}}`, id))
		case "session/prompt":
			out(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hel"}}}}`)
			out(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"lo"}}}}`)
			out(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"touch f","kind":"execute","status":"pending","rawInput":{"command":"touch f"}}}}`)
			out(`{"jsonrpc":"2.0","id":50,"method":"fs/read_text_file","params":{"sessionId":"s1","path":"/x"}}`)
			unsupported := read()
			code := -1.0
			if e, ok := unsupported["error"].(map[string]any); ok {
				code, _ = e["code"].(float64)
			}
			out(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"t1","title":"touch f","kind":"execute","rawInput":{"command":"touch f"}},"options":[{"optionId":"a","name":"Yes","kind":"allow_once"},{"optionId":"r","name":"No","kind":"reject_once"}]}}`)
			resp := read()
			res, _ := json.Marshal(resp["result"])
			status, text := "completed", string(res)
			if strings.Contains(text, `"optionId":"r"`) {
				status = "failed"
			}
			tu, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{
				"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": status,
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": fmt.Sprintf("unsupported=%v outcome=%s", code, text)}}},
			}}})
			out(string(tu))
			out(fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"stopReason":"end_turn"}}`, id))
		}
	}
}

func startFake(t *testing.T) (*engine, <-chan event.Event) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(spec{name: "acp-fake", bin: exe, env: []string{fakeEnv + "=1"}}, event.Options{Work: t.TempDir()})
	if err := e.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Kill() })
	return e, e.Events()
}

func next(t *testing.T, ch <-chan event.Event) event.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return event.Event{}
}

func expect(t *testing.T, ch <-chan event.Event, k event.Kind) event.Event {
	t.Helper()
	ev := next(t, ch)
	if ev.Kind != k {
		t.Fatalf("got %s (%q), want %s", ev.Kind, ev.Text, k)
	}
	return ev
}

func TestFramingAndPermissionRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		d       event.Decision
		option  string
		isError bool
	}{
		{"allow", event.Decision{Verdict: "allow_once"}, `"optionId":"a"`, false},
		{"deny", event.Decision{Verdict: "deny", Reason: "no"}, `"optionId":"r"`, true},
		{"answer", event.Decision{Verdict: "answer", Answers: map[string]string{"q": "yes"}}, `"optionId":"a"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ch := startFake(t)
			if init := expect(t, ch, event.KindInit); init.SessionID != "s1" || e.SessionID() != "s1" {
				t.Fatalf("session = %q", init.SessionID)
			}
			if err := e.Send("go"); err != nil {
				t.Fatal(err)
			}
			expect(t, ch, event.KindTurnStart)
			if txt := expect(t, ch, event.KindText); txt.Text != "hello" {
				t.Fatalf("text = %q, want coalesced hello", txt.Text)
			}
			ts := expect(t, ch, event.KindToolStart)
			if ts.ToolUseID != "t1" || ts.Detail.Kind != "bash" || ts.Detail.Command != "touch f" {
				t.Fatalf("tool start = %+v", ts)
			}
			perm := expect(t, ch, event.KindPermission)
			if perm.Req == nil || perm.Req.Engine != "acp-fake" || perm.Detail.Command != "touch f" ||
				strings.Join(perm.Detail.Options, ",") != "Yes,No" {
				t.Fatalf("permission = %+v", perm)
			}
			if err := e.Respond(perm.Req, tc.d); err != nil {
				t.Fatal(err)
			}
			res := expect(t, ch, event.KindToolResult)
			if res.IsError != tc.isError || !strings.Contains(res.Text, tc.option) || !strings.Contains(res.Text, "unsupported=-32601") {
				t.Fatalf("tool result = %+v, want %s and a -32601 reply", res, tc.option)
			}
			if end := expect(t, ch, event.KindTurnEnd); end.Text != "end_turn" {
				t.Fatalf("turn end = %+v", end)
			}
		})
	}
}

func TestRespondUnknownRequest(t *testing.T) {
	e, ch := startFake(t)
	expect(t, ch, event.KindInit)
	if err := e.Respond(&event.Request{ID: "99"}, event.Decision{Verdict: "allow_once"}); err == nil {
		t.Fatal("want error for an unknown permission id")
	}
}

func TestChooseOutcome(t *testing.T) {
	pi := []option{{"0", "allow_once", "allow_once"}, {"1", "allow_session", "allow_once"}, {"2", "deny", "reject_once"}}
	claude := []option{{"allow-once", "Yes", "allow_once"}, {"reject", "No", "reject_once"}}
	for _, tc := range []struct {
		opts []option
		d    event.Decision
		want string
	}{
		{pi, event.Decision{Verdict: "allow_session"}, "1"},
		{pi, event.Decision{Verdict: "deny"}, "2"},
		{claude, event.Decision{Verdict: "allow_once"}, "allow-once"},
		{claude, event.Decision{Verdict: "deny"}, "reject"},
	} {
		got, err := chooseOutcome(tc.opts, tc.d)
		if err != nil || got["optionId"] != tc.want {
			t.Errorf("%v %v: got %v, %v; want %s", tc.opts, tc.d.Verdict, got, err, tc.want)
		}
	}
	if _, err := chooseOutcome(claude, event.Decision{Verdict: "allow_session"}); err == nil {
		t.Error("allow_session with no allow_always option should error")
	}
	got, err := chooseOutcome([]option{{"a", "Yes", "allow_once"}}, event.Decision{Verdict: "deny"})
	if err != nil || got["outcome"] != "cancelled" {
		t.Errorf("deny without a reject option = %v, %v; want cancelled", got, err)
	}
}

func TestSetConfigFailure(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fail    string
		wantErr bool
	}{{"mode", true}, {"model", false}} {
		t.Run(tc.fail, func(t *testing.T) {
			e := newEngine(spec{
				name: "acp-fake", bin: exe,
				env:    []string{fakeEnv + "=1", "HSUP_FAKE_ACP_FAIL_CONFIG=" + tc.fail},
				config: [][2]string{{"model", "m"}, {"mode", "auto"}},
			}, event.Options{Work: t.TempDir()})
			err := e.Start(t.Context())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Start err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				t.Cleanup(func() { _ = e.Kill() })
			}
			if tc.wantErr && !strings.Contains(err.Error(), "mode") {
				t.Errorf("error should name the mode: %v", err)
			}
		})
	}
}

func TestCancelClosesChildGracefully(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(spec{name: "acp-fake", bin: exe, env: []string{fakeEnv + "=1"}}, event.Options{Work: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Well inside closeGrace: the child saw EOF on stdin, not SIGKILL.
	deadline := time.After(closeGrace / 2)
	for {
		select {
		case ev, ok := <-e.Events():
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
