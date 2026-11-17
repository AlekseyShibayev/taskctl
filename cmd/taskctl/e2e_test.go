package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestStdioSmoke builds the binary and drives one real MCP session over pipes:
// initialize, tools/list, two creates and three gets. Every line the server
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
	toolContent := func(envelope map[string]any) ([]any, bool) {
		t.Helper()
		result, _ := envelope["result"].(map[string]any)
		content, _ := result["content"].([]any)
		isError, _ := result["isError"].(bool)
		return content, isError
	}
	toolText := func(envelope map[string]any) (string, bool) {
		t.Helper()
		content, isError := toolContent(envelope)
		if len(content) != 1 {
			t.Fatalf("content = %v", content)
		}
		return content[0].(map[string]any)["text"].(string), isError
	}
	requireNoContent := func(envelope map[string]any) {
		t.Helper()
		content, isError := toolContent(envelope)
		if isError || len(content) != 0 {
			t.Fatalf("create result = %v", envelope)
		}
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
	if got := strings.Join(names, ","); got != "create,get" {
		t.Fatalf("tools = %s", got)
	}

	created := request(3, "tools/call", map[string]any{
		"name": "create", "arguments": map[string]any{"text": "smoke task"},
	})
	requireNoContent(created)

	second := request(4, "tools/call", map[string]any{
		"name": "create", "arguments": map[string]any{"text": "second task"},
	})
	requireNoContent(second)

	taken := request(5, "tools/call", map[string]any{"name": "get", "arguments": map[string]any{}})
	if text, isError := toolText(taken); isError || text != "smoke task" {
		t.Fatalf("first get = %q, isError = %v", text, isError)
	}

	next := request(6, "tools/call", map[string]any{"name": "get", "arguments": map[string]any{}})
	if text, isError := toolText(next); isError || text != "second task" {
		t.Fatalf("second get = %q, isError = %v", text, isError)
	}

	empty := request(7, "tools/call", map[string]any{"name": "get", "arguments": map[string]any{}})
	if _, isError := toolText(empty); !isError {
		t.Fatalf("get on empty queue = %v", empty)
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

// TestConcurrentColdStart starts several processes on a fresh database at the
// same time. Initialisation must serialise instead of failing with SQLITE_BUSY,
// and every queued task must survive.
func TestConcurrentColdStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-process test in short mode")
	}
	bin := buildTaskctl(t)
	dataDir := t.TempDir()

	const processes = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, processes)
	for i := 0; i < processes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(bin, "mcp")
			cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir)
			cmd.Stdin = strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
					fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create","arguments":{"text":"cold %d"}}}`, i) + "\n")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			<-start
			if err := cmd.Run(); err != nil {
				errs <- fmt.Sprintf("process %d: %v (stderr: %s)", i, err, stderr.String())
				return
			}
			if got := strings.Count(stdout.String(), "\n"); got != 2 {
				errs <- fmt.Sprintf("process %d: %d responses (stderr: %s)", i, got, stderr.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Drain the queue in one session: every create must be visible, once.
	var input strings.Builder
	for i := 0; i < processes+1; i++ {
		fmt.Fprintf(&input, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"get"}}`+"\n", i+1)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir)
	cmd.Stdin = strings.NewReader(input.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	seen := make(map[string]bool, processes)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		if len(envelope.Result.Content) != 1 {
			continue
		}
		text := envelope.Result.Content[0].Text
		if !strings.HasPrefix(text, "cold ") {
			continue
		}
		if seen[text] {
			t.Fatalf("task %q taken twice", text)
		}
		seen[text] = true
	}
	if len(seen) != processes {
		t.Fatalf("taken %d tasks, want %d", len(seen), processes)
	}
}

// TestDatabasePermissionsAndShutdown checks the documented modes while the
// server runs, and that SIGTERM ends the session cleanly.
func TestDatabasePermissionsAndShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-process test in short mode")
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	reader := bufio.NewReader(stdout)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read initialize response: %v", err)
	}

	dbPath := filepath.Join(dataDir, "taskctl", "tasks.db")
	checkMode := func(name string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(name), err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(name), got, want)
		}
	}
	checkMode(filepath.Join(dataDir, "taskctl"), 0o700)
	checkMode(dbPath, 0o600)
	for _, companion := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(companion); err == nil {
			checkMode(companion, 0o600)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v (stderr %s)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	checkMode(dbPath, 0o600)
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
