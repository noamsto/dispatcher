package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

const fakeEnv = "HSUP_FAKE_CLAUDE_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeEnv) {
	case "1":
		fakeChild()
		return
	case "elicit":
		fakeElicitChild()
		return
	}
	os.Exit(m.Run())
}

// fakeChild speaks just enough stream-json: user frame in, init + tool_use +
// can_use_tool out, control_response in, tool_result + result out, then EOF.
func fakeChild() {
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		os.Exit(2)
	}
	out := func(s string) { fmt.Println(s) }
	out(`{"type":"system","subtype":"init","session_id":"sess-fake"}`)
	out(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_f","name":"Bash","input":{"command":"touch f.txt"}}]}}`)
	out(`{"type":"control_request","request_id":"rq-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"touch f.txt"},"tool_use_id":"toolu_f"}}`)
	line, err := in.ReadString('\n')
	if err != nil {
		os.Exit(3)
	}
	var resp struct {
		Response struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(line), &resp) != nil || resp.Response.RequestID != "rq-1" {
		os.Exit(4)
	}
	out(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_f","content":"ok"}]}}`)
	out(`{"type":"result","subtype":"success","result":"behavior=` + resp.Response.Response.Behavior + `"}`)
	for {
		if _, err := in.ReadString('\n'); err != nil {
			return
		}
	}
}

// fakeElicitChild sends an elicitation control_request and reports the reply it gets.
func fakeElicitChild() {
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		os.Exit(2)
	}
	fmt.Println(`{"type":"control_request","request_id":"el-1","request":{"subtype":"elicitation","message":"?"}}`)
	line, err := in.ReadString('\n')
	if err != nil {
		os.Exit(3)
	}
	var resp struct {
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(line), &resp) != nil {
		os.Exit(4)
	}
	r := resp.Response
	fmt.Printf(`{"type":"result","subtype":"success","result":"%s|%s|%s"}`+"\n", r.Subtype, r.RequestID, r.Error)
}

func decodeFile(t *testing.T, name string) []event.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := Decode([]byte(strings.TrimSpace(string(data))), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return evs
}

func one(t *testing.T, evs []event.Event, k event.Kind) {
	t.Helper()
	if len(evs) != 1 || evs[0].Kind != k {
		t.Fatalf("want one %s, got %+v", k, evs)
	}
}

func permCheck(id, tool string, d event.Detail, useID string) func(*testing.T, []event.Event) {
	return func(t *testing.T, evs []event.Event) {
		t.Helper()
		one(t, evs, event.KindPermission)
		r := evs[0].Req
		if r == nil || r.ID != id || r.Tool != tool || evs[0].ToolUseID != useID {
			t.Fatalf("req %+v ev %+v", r, evs[0])
		}
		if !reflect.DeepEqual(r.Detail, d) || !reflect.DeepEqual(evs[0].Detail, d) {
			t.Errorf("detail %+v want %+v", r.Detail, d)
		}
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		file  string
		check func(t *testing.T, evs []event.Event)
	}{
		{"init", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindInit)
			if evs[0].SessionID != "11111111-2222-3333-4444-555555555555" {
				t.Errorf("session %q", evs[0].SessionID)
			}
		}},
		{"assistant_text", func(t *testing.T, evs []event.Event) {
			if len(evs) != 2 || evs[0].Kind != event.KindText || evs[0].Text != "Running it now." {
				t.Fatalf("text: %+v", evs)
			}
			ts := evs[1]
			if ts.Kind != event.KindToolStart || ts.Tool != "Bash" || ts.ToolUseID != "toolu_01" ||
				ts.Detail.Kind != "bash" || ts.Detail.Command != "touch made-by-probe.txt" {
				t.Errorf("tool_start: %+v", ts)
			}
		}},
		{"tool_result_ok", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindToolResult)
			if evs[0].Text != "done" || evs[0].IsError || evs[0].ToolUseID != "toolu_01" {
				t.Errorf("%+v", evs[0])
			}
		}},
		{"tool_result_err", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindToolResult)
			if evs[0].Text != "policy: no file creation" || !evs[0].IsError {
				t.Errorf("%+v", evs[0])
			}
		}},
		{"perm_bash", permCheck("req-1", "Bash", event.Detail{Kind: "bash", Command: "touch made-by-probe.txt"}, "toolu_01")},
		{"perm_write", permCheck("req-2", "Write", event.Detail{Kind: "write", Path: "/outside/w.txt"}, "toolu_02")},
		{"perm_read", permCheck("req-3", "Read", event.Detail{Kind: "read", Path: "/outside/secret-free.txt"}, "toolu_03")},
		{"perm_mcp", permCheck("req-4", "mcp__echo__echo", event.Detail{Kind: "mcp", Server: "echo"}, "toolu_04")},
		{"perm_web", permCheck("req-5", "WebFetch", event.Detail{Kind: "web", URL: "https://example.com/a?b=1", Domain: "example.com"}, "toolu_05")},
		{"perm_plan", permCheck("req-7", "ExitPlanMode", event.Detail{Kind: "plan"}, "toolu_07")},
		{"perm_ask", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindPermission)
			d := evs[0].Detail
			if d.Kind != "ask" || !slices.Equal(d.Options, []string{"red", "blue"}) || !strings.Contains(string(d.Questions), "Pick a colour") {
				t.Errorf("%+v", d)
			}
		}},
		{"hook_callback", func(t *testing.T, evs []event.Event) {
			permCheck("req-8", "Bash", event.Detail{Kind: "bash", Command: "ls -la"}, "toolu_08")(t, evs)
			if evs[0].Req.Engine != engineHookName {
				t.Errorf("engine %q", evs[0].Req.Engine)
			}
		}},
		{"api_retry", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindRetry)
			if !strings.Contains(evs[0].Text, "429") {
				t.Errorf("text %q", evs[0].Text)
			}
		}},
		{"rate_limit_allowed", func(t *testing.T, evs []event.Event) {
			if len(evs) != 0 {
				t.Errorf("want ignored, got %+v", evs)
			}
		}},
		{"rate_limit_rejected", func(t *testing.T, evs []event.Event) { one(t, evs, event.KindRetry) }},
		{"result_ok", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindTurnEnd)
			if evs[0].IsError || evs[0].Text != "all done" {
				t.Errorf("%+v", evs[0])
			}
		}},
		{"result_err", func(t *testing.T, evs []event.Event) {
			one(t, evs, event.KindTurnEnd)
			if !evs[0].IsError {
				t.Errorf("want IsError")
			}
		}},
		{"control_response", func(t *testing.T, evs []event.Event) {
			if len(evs) != 0 {
				t.Errorf("want log-only, got %+v", evs)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) { tc.check(t, decodeFile(t, tc.file)) })
	}
}

func TestArgs(t *testing.T) {
	base := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-prompt-tool", "stdio", "--model", "haiku",
		"--setting-sources", "local", "--strict-mcp-config",
	}
	tests := []struct {
		name string
		opts event.Options
		want []string
	}{
		{"new", event.Options{SessionID: "abc"}, append(slices.Clone(base), "--session-id", "abc")},
		{"resume", event.Options{SessionID: "abc", Resume: true}, append(slices.Clone(base), "--resume", "abc")},
		{"plan", event.Options{PermissionMode: "plan"}, append(slices.Clone(base), "--permission-mode", "plan")},
		{"mcp", event.Options{MCPConfig: "/m.json"}, append(slices.Clone(base), "--mcp-config", "/m.json")},
		{"bare", event.Options{Bare: true, Model: "haiku"}, append(slices.Clone(base), "--bare")},
		{"hooks adds no flag", event.Options{Hooks: true}, base},
		{"extra args last", event.Options{ExtraArgs: []string{"--tools", "Bash,WebFetch"}}, append(slices.Clone(base), "--tools", "Bash,WebFetch")},
		{"prompt tool mcp", event.Options{PromptToolMCP: "mcp__echo__permission_prompt", MCPConfig: "/m.json"}, []string{
			"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
			"--permission-prompt-tool", "mcp__echo__permission_prompt", "--model", "haiku",
			"--setting-sources", "local", "--strict-mcp-config", "--mcp-config", "/m.json",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Args(tc.opts); !slices.Equal(got, tc.want) {
				t.Errorf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestChildEnv(t *testing.T) {
	in := []string{
		"PATH=/bin", "CREW_WORKER_ID=w", "CREW_ID=c", "CREW_LEAD_X=1", "TMUX=/s", "TMUX_PANE=%1",
		"CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_EXECPATH=/x", "CLAUDE_CODE_SUBAGENT_EFFORT=high",
		"CLAUDE_CODE_SSE_PORT=9", "CLAUDE_CODE_OTHER=keep", "CREW_OTHER=keep", "TMUXISH=keep",
	}
	got := ChildEnv(in, []string{"A=b"})
	want := []string{"PATH=/bin", "CLAUDE_CODE_OTHER=keep", "CREW_OTHER=keep", "TMUXISH=keep", "A=b"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestParseWrapper(t *testing.T) {
	script := "#!/bin/bash\nexec -a claude /nix/store/old/bin/claude --x\nif x; then\n  exec -a claude /nix/store/real/bin/claude --settings s \"$@\"\nfi\n"
	got, err := ParseWrapper([]byte(script))
	if err != nil || got != "/nix/store/real/bin/claude" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := ParseWrapper([]byte("#!/bin/sh\necho hi\n")); err == nil {
		t.Error("want error without exec line")
	}
}

func TestResolveBinEnv(t *testing.T) {
	t.Setenv("HSUP_CLAUDE_BIN", "/x/claude")
	if got, err := ResolveBin(); err != nil || got != "/x/claude" {
		t.Errorf("got %q, %v", got, err)
	}
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonOf(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRespondJSON(t *testing.T) {
	bash := &event.Request{ID: "r", Tool: "Bash", Input: json.RawMessage(`{"command":"touch a.txt"}`), Detail: event.Detail{Kind: "bash", Command: "touch a.txt"}}
	web := &event.Request{ID: "r", Tool: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com/x"}`), Detail: event.Detail{Kind: "web", Domain: "example.com"}}
	mcp := &event.Request{ID: "r", Tool: "mcp__echo__echo", Input: json.RawMessage(`{"text":"hi"}`), Detail: event.Detail{Kind: "mcp", Server: "echo"}}
	ask := &event.Request{ID: "r", Tool: "AskUserQuestion", Input: json.RawMessage(`{"questions":[{"question":"Pick a colour"}]}`), Detail: event.Detail{Kind: "ask"}}
	plan := &event.Request{ID: "r", Tool: "ExitPlanMode", Input: json.RawMessage(`{"plan":"p"}`), Detail: event.Detail{Kind: "plan"}}
	write := &event.Request{ID: "r", Tool: "Write", Input: json.RawMessage(`{"file_path":"/w/src/a.go"}`), Detail: event.Detail{Kind: "write", Path: "/w/src/a.go"}}
	noPath := &event.Request{ID: "r", Tool: "Edit", Input: json.RawMessage(`{}`), Detail: event.Detail{Kind: "write"}}
	noDomain := &event.Request{ID: "r", Tool: "WebFetch", Input: json.RawMessage(`{}`), Detail: event.Detail{Kind: "web"}}
	hook := &event.Request{ID: "r", Engine: engineHookName, Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}

	tests := []struct {
		name string
		req  *event.Request
		dec  event.Decision
		want string
	}{
		{"allow_once", bash, event.Decision{Verdict: "allow_once"},
			`{"behavior":"allow","updatedInput":{"command":"touch a.txt"}}`},
		{"allow_session bash", bash, event.Decision{Verdict: "allow_session"},
			`{"behavior":"allow","updatedInput":{"command":"touch a.txt"},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"touch:*"}],"behavior":"allow","destination":"session"}]}`},
		{"allow_session web", web, event.Decision{Verdict: "allow_session"},
			`{"behavior":"allow","updatedInput":{"url":"https://example.com/x"},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"WebFetch","ruleContent":"domain:example.com"}],"behavior":"allow","destination":"session"}]}`},
		{"allow_session mcp", mcp, event.Decision{Verdict: "allow_session"},
			`{"behavior":"allow","updatedInput":{"text":"hi"},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"mcp__echo__echo"}],"behavior":"allow","destination":"session"}]}`},
		{"allow_session write scoped to dir", write, event.Decision{Verdict: "allow_session"},
			`{"behavior":"allow","updatedInput":{"file_path":"/w/src/a.go"},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Write","ruleContent":"//w/src/**"}],"behavior":"allow","destination":"session"}]}`},
		{"allow_session no path downgrades", noPath, event.Decision{Verdict: "allow_session"},
			`{"behavior":"allow","updatedInput":{}}`},
		{"allow_session web no domain downgrades", noDomain, event.Decision{Verdict: "allow_session"},
			`{"behavior":"allow","updatedInput":{}}`},
		{"deny", bash, event.Decision{Verdict: "deny", Reason: "policy: no"},
			`{"behavior":"deny","message":"policy: no"}`},
		{"answer", ask, event.Decision{Verdict: "answer", Answers: map[string]string{"Pick a colour": "blue"}},
			`{"behavior":"allow","updatedInput":{"questions":[{"question":"Pick a colour"}],"answers":{"Pick a colour":"blue"}}}`},
		{"plan allow", plan, event.Decision{Verdict: "allow_once"},
			`{"behavior":"allow","updatedInput":{"plan":"p"}}`},
		{"plan deny", plan, event.Decision{Verdict: "deny", Reason: "revise"},
			`{"behavior":"deny","message":"revise"}`},
		{"hook deny", hook, event.Decision{Verdict: "deny", Reason: "hook says no"},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"hook says no"}}`},
		{"hook empty verdict denies", hook, event.Decision{},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"supervisor: unsupported verdict "}}`},
		{"hook answer denies", hook, event.Decision{Verdict: "answer"},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"supervisor: unsupported verdict answer"}}`},
		{"hook allow", hook, event.Decision{Verdict: "allow_once"},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var payload any
			if tc.req.Engine == engineHookName {
				payload = HookOutput(tc.dec)
			} else {
				var err error
				payload, err = PermissionResult(tc.req, tc.dec)
				if err != nil {
					t.Fatal(err)
				}
			}
			if got, want := asJSON(t, payload), jsonOf(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Errorf("got  %v\nwant %v", got, want)
			}
		})
	}
	if _, err := PermissionResult(bash, event.Decision{Verdict: "bogus"}); err == nil {
		t.Error("want error for unknown verdict")
	}
}

func TestRoundTrip(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "t.jsonl")
	tw, err := transcript.New(logPath, &transcript.Scrubber{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tw.Close() }()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(event.Options{
		Bin: exe, Work: t.TempDir(), SessionID: "sess-opt", Transcript: tw,
		Env: []string{fakeEnv + "=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.SessionID(); got != "sess-opt" {
		t.Errorf("pre-start session %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.Send("hello"); err != nil {
		t.Fatal(err)
	}

	var kinds []event.Kind
	var last event.Event
	for ev := range eng.Events() {
		kinds = append(kinds, ev.Kind)
		last = ev
		if ev.Kind == event.KindPermission {
			if err := eng.Respond(ev.Req, event.Decision{Verdict: "allow_once"}); err != nil {
				t.Fatal(err)
			}
		}
		if ev.Kind == event.KindTurnEnd {
			if ev.Text != "behavior=allow" {
				t.Errorf("child saw %q", ev.Text)
			}
			if err := eng.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		}
	}
	want := []event.Kind{event.KindInit, event.KindToolStart, event.KindPermission, event.KindToolResult, event.KindTurnEnd, event.KindExit}
	if !slices.Equal(kinds, want) {
		t.Errorf("kinds %v want %v", kinds, want)
	}
	if last.Text != "" {
		t.Errorf("exit text %q", last.Text)
	}
	if got := eng.SessionID(); got != "sess-fake" {
		t.Errorf("session %q", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{`"dir":"in"`, `"dir":"out"`} {
		if !strings.Contains(string(data), dir) {
			t.Errorf("transcript missing %s", dir)
		}
	}
}

func TestAllowSessionDowngradeNote(t *testing.T) {
	r := &event.Request{ID: "r", Tool: "Edit", Input: json.RawMessage(`{}`)}
	if _, note, err := permissionResult(r, event.Decision{Verdict: "allow_session"}); err != nil || note == "" {
		t.Errorf("want downgrade note, got %q err %v", note, err)
	}
	r = &event.Request{ID: "r", Tool: "Write", Detail: event.Detail{Path: "/w/a"}}
	if _, note, err := permissionResult(r, event.Decision{Verdict: "allow_session"}); err != nil || note != "" {
		t.Errorf("want no note, got %q err %v", note, err)
	}
}

func TestFileRuleScope(t *testing.T) {
	tests := []struct {
		path, want string
		ok         bool
	}{
		{"/w/src/x", "//w/src/**", true},
		{"/w/src/../y/x", "//w/y/**", true},
		{"/x", "", false},
		{"/tmp/../x", "", false},
		{"/w/*/x", "", false},
		{"/w/{a,b}/x", "", false},
		{"rel/x", "", false},
	}
	for _, tc := range tests {
		r := &event.Request{Tool: "Read", Detail: event.Detail{Path: tc.path}}
		got, ok := ruleContent(r)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %q,%v want %q,%v", tc.path, got, ok, tc.want, tc.ok)
		}
		res, note, err := permissionResult(r, event.Decision{Verdict: "allow_session"})
		if err != nil {
			t.Fatal(err)
		}
		if _, has := res["updatedPermissions"]; has != tc.ok || (note == "") != tc.ok {
			t.Errorf("%s: updatedPermissions=%v note=%q", tc.path, has, note)
		}
	}
}

func TestUnknownControlRequestErrorsAndReplies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(event.Options{Bin: exe, Work: t.TempDir(), Env: []string{fakeEnv + "=elicit"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := eng.Send("hi"); err != nil {
		t.Fatal(err)
	}
	var sawErr bool
	var result string
	timeout := time.After(30 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				break loop
			}
			switch ev.Kind {
			case event.KindError:
				sawErr = ev.Text == "unhandled control_request elicitation"
			case event.KindTurnEnd:
				result = ev.Text
				_ = eng.Close()
			}
		case <-timeout:
			t.Fatal("child blocked: no reply to unsupported control_request")
		}
	}
	if !sawErr {
		t.Error("missing KindError for elicitation")
	}
	if want := "error|el-1|unsupported by hsup"; result != want {
		t.Errorf("child saw %q, want %q", result, want)
	}
}

func TestCancelClosesChildGracefully(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(event.Options{Bin: exe, Work: t.TempDir(), Env: []string{fakeEnv + "=1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Well inside closeGrace: the child saw EOF on stdin, not SIGKILL.
	deadline := time.After(closeGrace / 2)
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				t.Fatal("events closed without Exit")
			}
			if ev.Kind != event.KindExit {
				continue
			}
			// The fake exits 2 on EOF before its first frame.
			if ev.Text != "exit status 2" {
				t.Fatalf("exit %q, want the child's own EOF exit", ev.Text)
			}
			return
		case <-deadline:
			t.Fatal("child still running after ctx cancel")
		}
	}
}
