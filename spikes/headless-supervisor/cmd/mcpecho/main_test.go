package main

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	pr, pw := io.Pipe()
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- serve(pr, &out) }()
	w := bufio.NewWriter(pw)
	for _, l := range lines {
		_, _ = w.WriteString(l + "\n")
	}
	_ = w.Flush()
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func resultText(t *testing.T, m map[string]any) string {
	t.Helper()
	res := m["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestHandshakeAndTools(t *testing.T) {
	resps := roundTrip(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
	)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses (notification ignored), got %d: %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", init["protocolVersion"])
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities = %v", init["capabilities"])
	}
	if resps[1]["id"].(float64) != 2 {
		t.Fatalf("ping id = %v", resps[1]["id"])
	}
	list := resps[2]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range list {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "echo,permission_prompt" {
		t.Fatalf("tools = %v", names)
	}
}

func TestToolCalls(t *testing.T) {
	resps := roundTrip(t,
		`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`,
		`{"jsonrpc":"2.0","id":"b","method":"tools/call","params":{"name":"permission_prompt","arguments":{"tool_name":"Bash","input":{},"tool_use_id":"t1"}}}`,
		`{"jsonrpc":"2.0","id":"c","method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":"d","method":"resources/list"}`,
	)
	if got := resultText(t, resps[0]); got != "hello" {
		t.Fatalf("echo = %q", got)
	}
	if resps[0]["id"] != "a" {
		t.Fatalf("string id not preserved: %v", resps[0]["id"])
	}
	if got := resultText(t, resps[1]); got != `{"behavior":"deny","message":"denied by mcp permission_prompt"}` {
		t.Fatalf("permission_prompt = %q", got)
	}
	if resps[2]["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatalf("unknown tool: %v", resps[2])
	}
	if resps[3]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", resps[3])
	}
}

func TestParseErrorAnswered(t *testing.T) {
	resps := roundTrip(t, `{not json`)
	if len(resps) != 1 || resps[0]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("resps = %v", resps)
	}
}
