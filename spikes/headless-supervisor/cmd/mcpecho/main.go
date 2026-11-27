// Command mcpecho is a minimal MCP server over stdio (newline-delimited
// JSON-RPC 2.0) with two tools: echo and permission_prompt.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const denyReply = `{"behavior":"deny","message":"denied by mcp permission_prompt"}`

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcpecho:", err)
		os.Exit(1)
	}
}

// serve answers requests from in until EOF. Notifications (no id) get no reply.
func serve(in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	enc := json.NewEncoder(out)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if resp, ok := handle(line); ok {
				if encErr := enc.Encode(resp); encErr != nil {
					return encErr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func handle(line []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(json.RawMessage("null"), -32700, "parse error"), true
	}
	if len(req.ID) == 0 {
		return rpcResponse{}, false
	}
	switch req.Method {
	case "initialize":
		return initialize(req), true
	case "ping":
		return ok(req.ID, struct{}{}), true
	case "tools/list":
		return ok(req.ID, map[string]any{"tools": tools()}), true
	case "tools/call":
		return call(req), true
	}
	return fail(req.ID, -32601, "method not found: "+req.Method), true
}

func initialize(req rpcRequest) rpcResponse {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p)
	return ok(req.ID, map[string]any{
		"protocolVersion": p.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "mcpecho", "version": "0.1.0"},
	})
}

func tools() []map[string]any {
	str := map[string]any{"type": "string"}
	return []map[string]any{
		{
			"name":        "echo",
			"description": "Return the given text unchanged.",
			"inputSchema": map[string]any{
				"type": "object", "properties": map[string]any{"text": str}, "required": []string{"text"},
			},
		},
		{
			"name":        "permission_prompt",
			"description": "Permission prompt tool: always denies.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"tool_name": str, "input": map[string]any{"type": "object"}, "tool_use_id": str},
				"required":   []string{"tool_name", "input"},
			},
		},
	}
}

func call(req rpcRequest) rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return fail(req.ID, -32602, "invalid params")
	}
	switch p.Name {
	case "echo":
		var a struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return fail(req.ID, -32602, "echo: invalid arguments")
		}
		return ok(req.ID, textResult(a.Text))
	case "permission_prompt":
		return ok(req.ID, textResult(denyReply))
	}
	return fail(req.ID, -32602, "unknown tool: "+p.Name)
}

func textResult(s string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}

func ok(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func fail(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}
