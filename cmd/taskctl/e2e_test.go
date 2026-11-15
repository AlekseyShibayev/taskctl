// Package main E2E tests build the binary and speak MCP over stdio.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type e2eEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestE2EStdioSession builds the binary and drives a real MCP session over
// pipes: the same path a client takes, including process start and shutdown.
func TestE2EStdioSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in short mode")
	}
	bin := filepath.Join(t.TempDir(), "taskctl")
	if output, err := exec.Command("go", "build", "-trimpath", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	cmd := exec.CommandContext(ctx, bin, "mcp")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir, "TASKCTL_LOG_LEVEL=warn")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	writer := bufio.NewWriter(stdin)
	reader := bufio.NewReader(stdout)
	request := func(id int, method string, params any) e2eEnvelope {
		t.Helper()
		payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			payload["params"] = params
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			t.Fatalf("write request: %v", err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatalf("flush request: %v", err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read response: %v (stderr %s)", err, stderr.String())
		}
		var envelope e2eEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		return envelope
	}
	notify := func(method string, params any) {
		t.Helper()
		payload := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			payload["params"] = params
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode notification: %v", err)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			t.Fatalf("write notification: %v", err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatalf("flush notification: %v", err)
		}
	}
	toolPayload := func(envelope e2eEnvelope) map[string]any {
		t.Helper()
		if envelope.Error != nil {
			t.Fatalf("unexpected rpc error: %+v", envelope.Error)
		}
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			t.Fatalf("decode tool result: %v", err)
		}
		if result.IsError {
			t.Fatalf("tool error: %s", result.Content[0].Text)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		return payload
	}

	init := request(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e", "version": "1"},
	})
	if init.Error != nil || init.JSONRPC != "2.0" {
		t.Fatalf("initialize = %+v", init)
	}
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(init.Result, &initResult); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if initResult.ProtocolVersion != "2025-06-18" || initResult.ServerInfo.Name != "taskctl" {
		t.Errorf("initialize result = %+v", initResult)
	}
	notify("notifications/initialized", nil)

	// A malformed line must be answered without killing the session.
	if _, err := writer.WriteString("{broken\n"); err != nil {
		t.Fatalf("write broken request: %v", err)
	}
	writer.Flush()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read parse error: %v", err)
	}
	var parseErr e2eEnvelope
	if err := json.Unmarshal(line, &parseErr); err != nil {
		t.Fatalf("decode parse error: %v", err)
	}
	if parseErr.Error == nil || parseErr.Error.Code != -32700 {
		t.Fatalf("parse error = %+v", parseErr)
	}

	tools := request(2, "tools/list", nil)
	if tools.Error != nil || !strings.Contains(string(tools.Result), `"add"`) {
		t.Fatalf("tools/list = %+v", tools)
	}

	added := toolPayload(request(3, "tools/call", map[string]any{
		"name":      "add",
		"arguments": map[string]any{"name": "e2e task", "content": "payload"},
	}))
	if added["name"] != "e2e task" || added["status"] != "create" {
		t.Fatalf("add = %v", added)
	}
	id := int64(added["id"].(float64))

	updated := toolPayload(request(4, "tools/call", map[string]any{
		"name":      "update",
		"arguments": map[string]any{"id": id, "status": "in_progress"},
	}))
	if updated["status"] != "in_progress" {
		t.Fatalf("update = %v", updated)
	}

	listed := toolPayload(request(5, "tools/call", map[string]any{
		"name":      "list",
		"arguments": map[string]any{"limit": 1},
	}))
	tasks, _ := listed["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["status"] != "in_progress" {
		t.Fatalf("list = %v", listed)
	}

	// Closing stdin is a normal client shutdown: the process must exit cleanly.
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v (stderr %s)", err, stderr.String())
	}
	if stderr.Len() == 0 || !strings.Contains(stderr.String(), "parse error") {
		t.Errorf("stderr must report the broken request and nothing else: %s", stderr.String())
	}

	if _, err := os.Stat(filepath.Join(dataDir, "taskctl", "tasks.db")); err != nil {
		t.Errorf("database file: %v", err)
	}
}

// TestE2EDoctor builds the binary once and checks the diagnostics command.
func TestE2EDoctor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in short mode")
	}
	bin := filepath.Join(t.TempDir(), "taskctl")
	if output, err := exec.Command("go", "build", "-trimpath", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, output)
	}

	dataDir := t.TempDir()
	cmd := exec.Command(bin, "doctor", "--json")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("doctor: %v (%s)", err, output)
	}
	var report struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode: %v (%s)", err, output)
	}
	if !report.OK {
		t.Errorf("doctor not ok: %s", output)
	}
}
