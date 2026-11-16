package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/AlekseyShibayev/taskctl/internal/mcp"
	"github.com/AlekseyShibayev/taskctl/internal/store"
)

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func encodeRequest(id any, method string, params any) string {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		payload["id"] = id
	}
	if params != nil {
		payload["params"] = params
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func encodeTool(id int, name string, args any) string {
	return encodeRequest(id, "tools/call", map[string]any{"name": name, "arguments": args})
}

func runServerRaw(t *testing.T, st *store.Store, lines ...string) []rpcEnvelope {
	t.Helper()
	var out bytes.Buffer
	input := strings.Join(lines, "\n") + "\n"
	if err := mcp.NewServer(st, "test").Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	trimmed := strings.TrimSpace(out.String())
	if trimmed == "" {
		return nil
	}
	var envelopes []rpcEnvelope
	for _, line := range strings.Split(trimmed, "\n") {
		var envelope rpcEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes
}

// runServer sends initialize and returns the responses to the given lines,
// sorted by id because tools/call runs on a worker.
func runServer(t *testing.T, st *store.Store, lines ...string) []rpcEnvelope {
	t.Helper()
	initLine := encodeRequest(0, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	envelopes := runServerRaw(t, st, append([]string{initLine}, lines...)...)
	rest := make([]rpcEnvelope, 0, len(envelopes))
	for _, envelope := range envelopes {
		if string(envelope.ID) == "0" {
			continue
		}
		rest = append(rest, envelope)
	}
	sort.SliceStable(rest, func(i, j int) bool { return numericID(rest[i].ID) < numericID(rest[j].ID) })
	return rest
}

func numericID(raw json.RawMessage) int {
	value, err := strconv.Atoi(string(raw))
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return value
}

func toolPayload(t *testing.T, envelope rpcEnvelope) (map[string]any, bool) {
	t.Helper()
	if envelope.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", envelope.Error)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, envelope.Result)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("content = %+v", result.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode payload %q: %v", result.Content[0].Text, err)
	}
	return payload, result.IsError
}

func requireToolError(t *testing.T, envelope rpcEnvelope) {
	t.Helper()
	if text := toolErrorText(t, envelope); text == "" {
		t.Fatal("tool error has no message")
	}
}

func toolErrorText(t *testing.T, envelope rpcEnvelope) string {
	t.Helper()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, envelope.Result)
	}
	if !result.IsError {
		t.Fatalf("expected isError, got %s", envelope.Result)
	}
	if len(result.Content) == 0 || result.Content[0].Text == "" {
		t.Fatalf("error has no message: %s", envelope.Result)
	}
	return result.Content[0].Text
}

func TestInitialize(t *testing.T) {
	st := newStore(t)
	for _, requested := range []string{"2024-11-05", "2025-03-26", "1999-01-01"} {
		envelopes := runServerRaw(t, st, encodeRequest(1, "initialize", map[string]any{
			"protocolVersion": requested,
		}))
		if len(envelopes) != 1 || envelopes[0].Error != nil {
			t.Fatalf("%s: envelopes = %+v", requested, envelopes)
		}
		var result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if result.ProtocolVersion != "2025-06-18" {
			t.Errorf("%s: protocolVersion = %q", requested, result.ProtocolVersion)
		}
		if result.ServerInfo.Name != "taskctl" || result.ServerInfo.Version != "test" {
			t.Errorf("serverInfo = %+v", result.ServerInfo)
		}
		if _, ok := result.Capabilities["tools"]; !ok {
			t.Errorf("capabilities = %v", result.Capabilities)
		}
		if result.Instructions == "" {
			t.Error("instructions must not be empty")
		}
	}
}

func TestToolsList(t *testing.T) {
	envelopes := runServer(t, newStore(t), encodeRequest(1, "tools/list", nil))
	if len(envelopes) != 1 || envelopes[0].Error != nil {
		t.Fatalf("envelopes = %+v", envelopes)
	}
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var names []string
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("%s: incomplete definition", tool.Name)
		}
	}
	if got := strings.Join(names, ","); got != "create,get" {
		t.Errorf("tools = %s", got)
	}
}

func TestCreateAndGet(t *testing.T) {
	st := newStore(t)

	created := runServer(t, st, encodeTool(1, "create", map[string]any{"text": "  first task  "}))
	payload, isError := toolPayload(t, created[0])
	if isError {
		t.Fatalf("create failed: %v", payload)
	}
	if payload["id"] != float64(1) || payload["text"] != "  first task  " || payload["get_at"] != nil {
		t.Fatalf("create = %v", payload)
	}

	second := runServer(t, st, encodeTool(1, "create", map[string]any{"text": "second task"}))
	if payload, _ := toolPayload(t, second[0]); payload["id"] != float64(2) {
		t.Fatalf("second create = %v", payload)
	}

	// get takes the oldest unclaimed task and stamps get_at.
	taken := runServer(t, st, encodeTool(1, "get", nil))
	payload, isError = toolPayload(t, taken[0])
	if isError {
		t.Fatalf("get failed: %v", payload)
	}
	if payload["id"] != float64(1) || payload["text"] != "  first task  " {
		t.Fatalf("first get = %v", payload)
	}
	if takenAt, ok := payload["get_at"].(string); !ok || takenAt == "" {
		t.Fatalf("get_at = %v, want a timestamp", payload["get_at"])
	}

	next := runServer(t, st, encodeTool(1, "get", nil))
	if payload, _ := toolPayload(t, next[0]); payload["id"] != float64(2) {
		t.Fatalf("second get = %v", payload)
	}

	empty := runServer(t, st, encodeTool(1, "get", nil))
	if text := toolErrorText(t, empty[0]); !strings.Contains(text, "queue is empty") {
		t.Fatalf("get on empty queue = %q", text)
	}
}

func TestArgumentAndDispatchErrors(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st,
		encodeRequest(1, "tools/call", map[string]any{"name": "nope"}),
		encodeTool(2, "create", map[string]any{}),
		encodeTool(3, "create", map[string]any{"text": ""}),
		encodeTool(4, "create", map[string]any{"text": "x", "extra": 1}),
		encodeTool(5, "get", map[string]any{"id": 1}),
		encodeTool(6, "get", nil),
	)
	if envelopes[0].Error == nil || envelopes[0].Error.Code != -32602 {
		t.Errorf("unknown tool = %+v", envelopes[0])
	}
	for _, envelope := range envelopes[1:] {
		requireToolError(t, envelope)
	}
}

// One task is enough for one client: a second get finds nothing to take.
func TestGetTakesEachTaskOnce(t *testing.T) {
	st := newStore(t)
	runServer(t, st, encodeTool(1, "create", map[string]any{"text": "only"}))

	taken := runServer(t, st, encodeTool(1, "get", nil))
	if _, isError := toolPayload(t, taken[0]); isError {
		t.Fatalf("get = %v", taken[0].Result)
	}
	again := runServer(t, st, encodeTool(1, "get", nil))
	requireToolError(t, again[0])
}

func TestParseErrorDoesNotStopServer(t *testing.T) {
	st := newStore(t)
	envelopes := runServerRaw(t, st,
		`{"jsonrpc":"2.0","id":1,`,
		encodeRequest(2, "initialize", map[string]any{"protocolVersion": "2025-06-18"}),
	)
	if len(envelopes) != 2 {
		t.Fatalf("responses = %d", len(envelopes))
	}
	if envelopes[0].Error == nil || envelopes[0].Error.Code != -32700 {
		t.Fatalf("parse error = %+v", envelopes[0])
	}
	if envelopes[1].Error != nil {
		t.Fatalf("initialize after parse error = %+v", envelopes[1])
	}
}

func TestInvalidJSONRPCAndMethod(t *testing.T) {
	envelopes := runServerRaw(t, newStore(t),
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	)
	if envelopes[0].Error == nil || envelopes[0].Error.Code != -32600 {
		t.Errorf("jsonrpc = %+v", envelopes[0])
	}
	if envelopes[1].Error == nil || envelopes[1].Error.Code != -32600 {
		t.Errorf("method = %+v", envelopes[1])
	}
	if envelopes[2].Error == nil || envelopes[2].Error.Code != -32601 {
		t.Errorf("unknown method = %+v", envelopes[2])
	}
}

func TestInitializeRequired(t *testing.T) {
	st := newStore(t)
	envelopes := runServerRaw(t, st,
		encodeRequest(1, "tools/list", nil),
		encodeTool(2, "create", map[string]any{"text": "task"}),
	)
	for _, envelope := range envelopes {
		if envelope.Error == nil || envelope.Error.Code != -32002 {
			t.Errorf("envelope = %+v", envelope)
		}
	}
}

func TestOversizedMessageRejected(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("x", 5<<20) + `"}`
	envelopes := runServerRaw(t, newStore(t),
		huge,
		encodeRequest(2, "initialize", map[string]any{}),
	)
	if len(envelopes) != 2 {
		t.Fatalf("responses = %d", len(envelopes))
	}
	if envelopes[0].Error == nil || envelopes[1].Error != nil {
		t.Fatalf("envelopes = %+v", envelopes)
	}
}

func TestNotificationsPingAndStringID(t *testing.T) {
	st := newStore(t)
	envelopes := runServerRaw(t, st,
		encodeRequest(1, "initialize", map[string]any{}),
		encodeRequest(nil, "notifications/initialized", nil),
		encodeRequest("abc", "ping", nil),
	)
	// initialize and ping answer; the notification does not.
	count := 0
	for _, envelope := range envelopes {
		if envelope.Error != nil {
			t.Fatalf("envelope = %+v", envelope)
		}
		count++
		if string(envelope.ID) == `"abc"` && string(envelope.Result) != "{}" {
			t.Errorf("ping result = %s", envelope.Result)
		}
	}
	if count != 2 {
		t.Fatalf("responses = %d", count)
	}
}

func TestToolsCallWithoutIDHasNoEffect(t *testing.T) {
	st := newStore(t)
	envelopes := runServerRaw(t, st,
		encodeRequest(1, "initialize", map[string]any{}),
		encodeRequest(nil, "tools/call", map[string]any{"name": "create", "arguments": map[string]any{"text": "task"}}),
		encodeTool(2, "get", nil),
	)
	// The id-less call is a notification: no task was created, so get fails.
	for _, envelope := range envelopes {
		if string(envelope.ID) == "2" {
			requireToolError(t, envelope)
		}
	}
}

func TestEmptyInput(t *testing.T) {
	var out bytes.Buffer
	if err := mcp.NewServer(newStore(t), "test").Serve(context.Background(), strings.NewReader(""), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q", out.String())
	}
}
