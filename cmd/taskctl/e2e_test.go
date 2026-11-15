package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStdioSmoke builds the binary and drives one real MCP session over pipes:
// initialize, tools/list, add, list and a failing show. Every line the server
// writes must be a JSON-RPC envelope; mixing logs into stdout breaks MCP.
func TestStdioSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping smoke test in short mode")
	}
	bin := buildTaskctl(t)

	dataDir := t.TempDir()
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	request := func(id int, method string, params any) map[string]any {
		t.Helper()
		payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			payload["params"] = params
		}
		line, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !scanner.Scan() {
			t.Fatalf("no response to %s (stderr %s)", method, stderr.String())
		}
		var envelope map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %q", scanner.Text())
		}
		return envelope
	}
	toolPayload := func(envelope map[string]any) map[string]any {
		t.Helper()
		result, _ := envelope["result"].(map[string]any)
		content, _ := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content = %v", content)
		}
		text := content[0].(map[string]any)["text"].(string)
		var payload map[string]any
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatalf("payload %q: %v", text, err)
		}
		return payload
	}

	init := request(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %v", init)
	}

	tools := request(2, "tools/list", nil)
	var names []string
	for _, tool := range tools["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "create,slice,get,update" {
		t.Fatalf("tools = %s", got)
	}

	added := toolPayload(request(3, "tools/call", map[string]any{
		"name": "create", "arguments": map[string]any{"name": "smoke task"},
	}))
	if added["id"] != float64(1) || added["status"] != "create" {
		t.Fatalf("add = %v", added)
	}

	listed := toolPayload(request(4, "tools/call", map[string]any{
		"name": "slice", "arguments": map[string]any{},
	}))
	if tasks, _ := listed["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("list = %v", listed)
	}

	missing := request(5, "tools/call", map[string]any{
		"name": "get", "arguments": map[string]any{"id": 999},
	})
	result := missing["result"].(map[string]any)
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatalf("show 999 = %v", missing)
	}
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, "not found") {
		t.Fatalf("show 999 = %q", content)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v (stderr %s)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "taskctl", "tasks.db")); err != nil {
		t.Errorf("database file: %v", err)
	}
}

// buildTaskctl compiles the command under test into a temporary directory.
func buildTaskctl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "taskctl")
	output, err := exec.Command("go", "build", "-trimpath", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build binary: %v\n%s", err, output)
	}
	return bin
}
