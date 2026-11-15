package mcp_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	t.Cleanup(func() { st.Close() })
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

func runServer(t *testing.T, st *store.Store, lines ...string) []rpcEnvelope {
	t.Helper()
	var out bytes.Buffer
	server := mcp.NewServer(st, "test")
	input := strings.Join(lines, "\n") + "\n"
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return decodeEnvelopes(t, out.Bytes())
}

func decodeEnvelopes(t *testing.T, raw []byte) []rpcEnvelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	envelopes := make([]rpcEnvelope, 0, len(lines))
	for _, line := range lines {
		var envelope rpcEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes
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
		t.Fatalf("decode tool payload %q: %v", result.Content[0].Text, err)
	}
	return payload, result.IsError
}

func requireToolError(t *testing.T, envelope rpcEnvelope, code string) {
	t.Helper()
	payload, isError := toolPayload(t, envelope)
	if !isError {
		t.Fatalf("expected isError, got payload %v", payload)
	}
	if payload["code"] != code {
		t.Fatalf("code = %v, want %s (payload %v)", payload["code"], code, payload)
	}
	if payload["message"] == nil || payload["message"] == "" {
		t.Fatalf("missing message in %v", payload)
	}
}

func TestInitialize(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st, encodeRequest(1, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	}))
	if len(envelopes) != 1 {
		t.Fatalf("responses = %d, want 1", len(envelopes))
	}
	envelope := envelopes[0]
	if envelope.JSONRPC != "2.0" || string(envelope.ID) != "1" || envelope.Error != nil {
		t.Fatalf("envelope = %+v", envelope)
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
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocolVersion = %q", result.ProtocolVersion)
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

	fallback := runServer(t, st, encodeRequest(2, "initialize", map[string]any{
		"protocolVersion": "1999-01-01",
	}))
	if len(fallback) != 1 {
		t.Fatalf("responses = %d, want 1", len(fallback))
	}
	var fallbackResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(fallback[0].Result, &fallbackResult); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if fallbackResult.ProtocolVersion != "2025-06-18" {
		t.Errorf("fallback protocolVersion = %q", fallbackResult.ProtocolVersion)
	}
}

func TestToolsList(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st, encodeRequest(1, "tools/list", nil))
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
		t.Fatalf("decode result: %v", err)
	}

	names := make([]string, len(result.Tools))
	for i, tool := range result.Tools {
		names[i] = tool.Name
		if tool.Description == "" {
			t.Errorf("%s has empty description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s schema type = %v", tool.Name, tool.InputSchema["type"])
		}
	}
	if strings.Join(names, ",") != "add,list,show,update" {
		t.Fatalf("tools = %v", names)
	}

	properties := func(name string) map[string]any {
		for _, tool := range result.Tools {
			if tool.Name == name {
				return tool.InputSchema["properties"].(map[string]any)
			}
		}
		t.Fatalf("tool %q not found", name)
		return nil
	}
	required := func(name string) []any {
		for _, tool := range result.Tools {
			if tool.Name == name {
				values, _ := tool.InputSchema["required"].([]any)
				return values
			}
		}
		return nil
	}

	if _, ok := properties("add")["name"]; !ok {
		t.Error("add must accept name")
	}
	if got := required("add"); len(got) != 1 || got[0] != "name" {
		t.Errorf("add required = %v", got)
	}

	limit := properties("list")["limit"].(map[string]any)
	if limit["default"] != float64(50) || limit["maximum"] != float64(500) || limit["minimum"] != float64(1) {
		t.Errorf("list limit schema = %v", limit)
	}
	listStatus := properties("list")["status"].(map[string]any)
	enum, _ := listStatus["enum"].([]any)
	if len(enum) != len(store.Statuses) {
		t.Errorf("list status enum = %v", enum)
	}

	if got := required("update"); len(got) != 1 || got[0] != "id" {
		t.Errorf("update required = %v", got)
	}
	updateStatus := properties("update")["status"].(map[string]any)
	updateEnum, _ := updateStatus["enum"].([]any)
	if len(updateEnum) != len(store.Statuses) {
		t.Errorf("update status enum = %v", updateEnum)
	}
	for _, value := range updateEnum {
		if value == "returned" || value == "cancelled" {
			t.Errorf("update status enum must not contain %q: %v", value, updateEnum)
		}
	}
}

func TestAddShowList(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st,
		encodeTool(1, "add", map[string]any{"name": "first", "content": "body"}),
		encodeTool(2, "show", map[string]any{"id": 1}),
		encodeTool(3, "list", map[string]any{}),
	)
	if len(envelopes) != 3 {
		t.Fatalf("responses = %d, want 3", len(envelopes))
	}

	added, isError := toolPayload(t, envelopes[0])
	if isError {
		t.Fatalf("add failed: %v", added)
	}
	if added["id"] != float64(1) || added["name"] != "first" || added["content"] != "body" ||
		added["status"] != string(store.StatusCreate) {
		t.Errorf("added = %v", added)
	}
	createdAt, ok := added["created_at"].(string)
	if !ok {
		t.Fatalf("created_at = %v", added["created_at"])
	}
	parsed, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("created_at %q is not RFC3339: %v", createdAt, err)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("created_at is not UTC: %s", parsed)
	}

	shown, isError := toolPayload(t, envelopes[1])
	if isError {
		t.Fatalf("show failed: %v", shown)
	}
	if shown["id"] != float64(1) || shown["created_at"] != createdAt {
		t.Errorf("shown = %v", shown)
	}

	page, isError := toolPayload(t, envelopes[2])
	if isError {
		t.Fatalf("list failed: %v", page)
	}
	if page["limit"] != float64(50) || page["offset"] != float64(0) || page["has_more"] != false {
		t.Errorf("page = %v", page)
	}
	tasks, _ := page["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %v", page["tasks"])
	}
}

func TestListPaginationAndFilter(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st,
		encodeTool(1, "add", map[string]any{"name": "one"}),
		encodeTool(2, "add", map[string]any{"name": "two"}),
		encodeTool(3, "add", map[string]any{"name": "three"}),
		encodeTool(4, "update", map[string]any{"id": 2, "status": "in_progress"}),
		encodeTool(5, "list", map[string]any{"limit": 2}),
		encodeTool(6, "list", map[string]any{"limit": 2, "offset": 2}),
		encodeTool(7, "list", map[string]any{"status": "in_progress"}),
		encodeTool(8, "list", map[string]any{"status": "cancelled"}),
		encodeTool(9, "list", map[string]any{"limit": 0}),
		encodeTool(10, "list", map[string]any{"limit": 501}),
		encodeTool(11, "list", map[string]any{"offset": -1}),
	)
	if len(envelopes) != 11 {
		t.Fatalf("responses = %d, want 11", len(envelopes))
	}

	page, isError := toolPayload(t, envelopes[4])
	if isError || page["has_more"] != true || page["limit"] != float64(2) {
		t.Errorf("first page = %v (isError %v)", page, isError)
	}
	tasks, _ := page["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("first page tasks = %v", page["tasks"])
	}

	second, isError := toolPayload(t, envelopes[5])
	if isError || second["has_more"] != false || second["offset"] != float64(2) {
		t.Errorf("second page = %v", second)
	}
	secondTasks, _ := second["tasks"].([]any)
	if len(secondTasks) != 1 || secondTasks[0].(map[string]any)["id"] != float64(3) {
		t.Errorf("second page tasks = %v", second["tasks"])
	}

	filtered, isError := toolPayload(t, envelopes[6])
	if isError {
		t.Fatalf("filtered list failed: %v", filtered)
	}
	filteredTasks, _ := filtered["tasks"].([]any)
	if len(filteredTasks) != 1 {
		t.Fatalf("filtered tasks = %v", filtered["tasks"])
	}
	if filteredTasks[0].(map[string]any)["id"] != float64(2) {
		t.Errorf("filtered task = %v", filteredTasks[0])
	}

	for _, envelope := range envelopes[7:] {
		requireToolError(t, envelope, store.CodeInvalidArgument)
	}
}

func TestUpdateTransitions(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st,
		encodeTool(1, "add", map[string]any{"name": "task", "content": "body"}),
		encodeTool(2, "update", map[string]any{"id": 1, "name": "renamed", "status": "in_progress"}),
		encodeTool(3, "update", map[string]any{"id": 1, "status": "create"}),
		encodeTool(4, "update", map[string]any{"id": 2, "status": "in_progress"}),
		encodeTool(5, "update", map[string]any{"id": 1, "status": "cancelled"}),
		encodeTool(6, "update", map[string]any{"id": 1}),
		encodeTool(7, "update", map[string]any{"id": 1, "status": "in_progress"}),
		encodeTool(8, "update", map[string]any{"id": 1, "status": "done"}),
		encodeTool(9, "update", map[string]any{"id": 1, "status": "create"}),
	)
	if len(envelopes) != 9 {
		t.Fatalf("responses = %d, want 9", len(envelopes))
	}

	updated, isError := toolPayload(t, envelopes[1])
	if isError {
		t.Fatalf("update failed: %v", updated)
	}
	if updated["name"] != "renamed" || updated["content"] != "body" || updated["status"] != "in_progress" {
		t.Errorf("updated = %v", updated)
	}

	reworked, isError := toolPayload(t, envelopes[2])
	if isError || reworked["status"] != "create" {
		t.Errorf("reworked = %v (isError %v)", reworked, isError)
	}
	requireToolError(t, envelopes[3], store.CodeNotFound)
	requireToolError(t, envelopes[4], store.CodeInvalidArgument)
	requireToolError(t, envelopes[5], store.CodeInvalidArgument)

	resumed, isError := toolPayload(t, envelopes[6])
	if isError || resumed["status"] != "in_progress" {
		t.Errorf("resumed = %v (isError %v)", resumed, isError)
	}
	done, isError := toolPayload(t, envelopes[7])
	if isError || done["status"] != "done" {
		t.Errorf("done = %v (isError %v)", done, isError)
	}
	requireToolError(t, envelopes[8], store.CodeInvalidTransition)
}

func TestArgumentAndDispatchErrors(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st,
		encodeTool(1, "show", map[string]any{"id": 999}),
		encodeTool(2, "add", map[string]any{}),
		encodeTool(3, "add", map[string]any{"name": 123}),
		encodeTool(4, "add", map[string]any{"name": "task", "unknown": true}),
		encodeTool(5, "unknown", map[string]any{}),
		encodeRequest(6, "unknown/method", nil),
	)
	if len(envelopes) != 6 {
		t.Fatalf("responses = %d, want 6", len(envelopes))
	}

	requireToolError(t, envelopes[0], store.CodeNotFound)
	for _, envelope := range envelopes[1:4] {
		requireToolError(t, envelope, store.CodeInvalidArgument)
	}

	if envelopes[4].Error == nil || envelopes[4].Error.Code != -32602 {
		t.Errorf("unknown tool error = %+v", envelopes[4].Error)
	}
	if envelopes[5].Error == nil || envelopes[5].Error.Code != -32601 {
		t.Errorf("unknown method error = %+v", envelopes[5].Error)
	}
}

func TestNotificationsAndPing(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st,
		encodeRequest(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}),
		encodeRequest(nil, "notifications/initialized", nil),
		encodeRequest(nil, "unknown/notification", nil),
		encodeRequest(2, "tools/list", nil),
		encodeRequest(3, "ping", nil),
	)
	if len(envelopes) != 3 {
		t.Fatalf("responses = %d, want 3", len(envelopes))
	}
	if string(envelopes[0].ID) != "1" || string(envelopes[1].ID) != "2" || string(envelopes[2].ID) != "3" {
		t.Errorf("ids = %s, %s, %s", envelopes[0].ID, envelopes[1].ID, envelopes[2].ID)
	}
	if envelopes[2].Error != nil {
		t.Errorf("ping error = %+v", envelopes[2].Error)
	}

	onlyNotification := runServer(t, st, encodeRequest(nil, "notifications/initialized", nil))
	if len(onlyNotification) != 0 {
		t.Fatalf("notification produced %d responses", len(onlyNotification))
	}
}

func TestStringID(t *testing.T) {
	st := newStore(t)
	envelopes := runServer(t, st, encodeRequest("abc", "tools/list", nil))
	if len(envelopes) != 1 || string(envelopes[0].ID) != `"abc"` {
		t.Fatalf("id = %s", envelopes[0].ID)
	}
}

func TestParseError(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	server := mcp.NewServer(st, "test")
	if err := server.Serve(strings.NewReader("{invalid\n"), &out); err == nil {
		t.Fatal("expected an error for malformed input")
	}
	envelopes := decodeEnvelopes(t, out.Bytes())
	if len(envelopes) != 1 || envelopes[0].Error == nil || envelopes[0].Error.Code != -32700 {
		t.Fatalf("envelopes = %+v", envelopes)
	}
	if string(envelopes[0].ID) != "null" {
		t.Errorf("parse error id = %s, want null", envelopes[0].ID)
	}
}

func TestEmptyInput(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	if err := mcp.NewServer(st, "test").Serve(strings.NewReader(""), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q", out.String())
	}
}
