package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlekseyShibayev/taskctl/internal/mcp"
	"github.com/AlekseyShibayev/taskctl/internal/store"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func serve(t *testing.T, input string) []envelope {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var out bytes.Buffer
	if err := mcp.NewServer(st, "test").Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var responses []envelope
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		responses = append(responses, env)
	}
	return responses
}

func line(payload any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data) + "\n"
}

func call(id int, name string, args any) string {
	return line(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
}

func decodeTool(t *testing.T, env envelope) toolResult {
	t.Helper()
	if env.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", env.Error)
	}
	var result toolResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode tool result %s: %v", env.Result, err)
	}
	return result
}

func TestInitializeAndToolsList(t *testing.T) {
	responses := serve(t, line(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})+
		line(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}))
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(responses[0].Result, &init); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if init.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q", init.ProtocolVersion)
	}
	if init.Capabilities.Tools == nil {
		t.Errorf("tools capability is missing")
	}
	if init.ServerInfo.Name != "taskctl" {
		t.Errorf("server name = %q", init.ServerInfo.Name)
	}

	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(responses[1].Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(list.Tools) != 2 || list.Tools[0].Name != "create" || list.Tools[1].Name != "get" {
		t.Fatalf("tools = %+v", list.Tools)
	}
	required, _ := list.Tools[0].InputSchema["required"].([]any)
	if len(required) != 1 || required[0] != "text" {
		t.Errorf("create required = %v", required)
	}
}

func TestCreateAndGet(t *testing.T) {
	responses := serve(t,
		call(1, "create", map[string]any{"text": "first"})+
			call(2, "create", map[string]any{"text": "second"})+
			call(3, "get", map[string]any{})+
			call(4, "get", map[string]any{})+
			call(5, "get", map[string]any{}))
	if len(responses) != 5 {
		t.Fatalf("responses = %d", len(responses))
	}

	for _, env := range responses[:2] {
		result := decodeTool(t, env)
		if result.IsError || len(result.Content) != 0 {
			t.Fatalf("create result = %+v, want empty success", result)
		}
	}
	for i, want := range []string{"first", "second"} {
		result := decodeTool(t, responses[2+i])
		if result.IsError || len(result.Content) != 1 || result.Content[0].Text != want {
			t.Fatalf("get %d = %+v, want %q", i+1, result, want)
		}
	}
	empty := decodeTool(t, responses[4])
	if !empty.IsError || empty.Content[0].Text != store.ErrEmpty.Error() {
		t.Fatalf("get on empty queue = %+v", empty)
	}
}

func TestGetReturnsPlainText(t *testing.T) {
	const text = `{"task":"not json"}`
	responses := serve(t,
		call(1, "create", map[string]any{"text": text})+
			call(2, "get", map[string]any{}))
	result := decodeTool(t, responses[1])
	if len(result.Content) != 1 || result.Content[0].Text != text {
		t.Fatalf("get = %+v, want the text as sent", result)
	}
}

func TestBadArguments(t *testing.T) {
	responses := serve(t,
		call(1, "create", map[string]any{})+
			call(2, "create", map[string]any{"text": 42})+
			call(3, "create", map[string]any{"text": "x", "extra": true})+
			call(4, "get", map[string]any{"limit": 1})+
			call(5, "nope", map[string]any{})+
			call(6, "get", map[string]any{}))

	missing := decodeTool(t, responses[0])
	if !missing.IsError || missing.Content[0].Text != "text is required" {
		t.Errorf("create without text = %+v", missing)
	}
	for i, env := range responses[1:5] {
		if env.Error == nil || env.Error.Code != -32602 {
			t.Errorf("call %d: error = %+v, want -32602", i+2, env.Error)
		}
	}
	// Rejected calls must not queue anything.
	if result := decodeTool(t, responses[5]); !result.IsError {
		t.Fatalf("queue is not empty after rejected calls: %+v", result)
	}
}

func TestParseErrorKeepsStream(t *testing.T) {
	responses := serve(t, "{not json\n"+
		line(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}))
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	if responses[0].Error == nil || responses[0].Error.Code != -32700 {
		t.Fatalf("parse error response = %+v", responses[0])
	}
	if responses[1].Error != nil {
		t.Fatalf("initialize after parse error = %+v", responses[1].Error)
	}
}

func TestUnknownMethod(t *testing.T) {
	responses := serve(t, line(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "nope"}))
	if responses[0].Error == nil || responses[0].Error.Code != -32601 {
		t.Fatalf("unknown method response = %+v", responses[0])
	}
}

func TestNotificationHasNoEffect(t *testing.T) {
	responses := serve(t,
		line(map[string]any{
			"jsonrpc": "2.0",
			"method":  "tools/call",
			"params":  map[string]any{"name": "create", "arguments": map[string]any{"text": "x"}},
		})+
			call(1, "get", map[string]any{}))
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want only the get", len(responses))
	}
	if result := decodeTool(t, responses[0]); !result.IsError {
		t.Fatalf("notification created a task: %+v", result)
	}
}

func TestEmptyInput(t *testing.T) {
	if responses := serve(t, ""); len(responses) != 0 {
		t.Fatalf("responses = %d, want none", len(responses))
	}
}
