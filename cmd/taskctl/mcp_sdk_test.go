// Package main smoke-tests the built binary with the official MCP Go client.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPClientSmoke connects a real MCP client to the built binary. The
// hand-written E2E test checks the wire format; this one checks that a
// third-party implementation of the protocol accepts the server.
func TestMCPClientSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP client smoke test in short mode")
	}
	bin := buildTaskctl(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	cmd := exec.CommandContext(ctx, bin, "mcp")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir, "TASKCTL_LOG_LEVEL=error")

	client := mcp.NewClient(&mcp.Implementation{Name: "taskctl-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	init := session.InitializeResult()
	if init == nil || init.ServerInfo == nil {
		t.Fatalf("initialize result = %+v", init)
	}
	if init.ServerInfo.Name != "taskctl" {
		t.Errorf("server name = %q", init.ServerInfo.Name)
	}
	if !strings.HasPrefix(init.ProtocolVersion, "2025-") && !strings.HasPrefix(init.ProtocolVersion, "2024-") {
		t.Errorf("protocol version = %q", init.ProtocolVersion)
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, len(tools.Tools))
	for i, tool := range tools.Tools {
		names[i] = tool.Name
		if tool.Title == "" {
			t.Errorf("%s has no title", tool.Name)
		}
	}
	if strings.Join(names, ",") != "add,list,show,update" {
		t.Fatalf("tools = %v", names)
	}

	added := callTool(ctx, t, session, "add", map[string]any{"name": "smoke task", "content": "via SDK"})
	if added["name"] != "smoke task" || added["status"] != "create" {
		t.Fatalf("add = %v", added)
	}

	updated := callTool(ctx, t, session, "update", map[string]any{
		"id":     added["id"],
		"status": "in_progress",
	})
	if updated["status"] != "in_progress" {
		t.Fatalf("update = %v", updated)
	}

	listed := callTool(ctx, t, session, "list", map[string]any{"status": "in_progress"})
	tasks, _ := listed["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("list = %v", listed)
	}

	// A domain error must arrive as a tool result, not as a JSON-RPC failure.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "show",
		Arguments: map[string]any{"id": 999},
	})
	if err != nil {
		t.Fatalf("show sent a JSON-RPC error instead of a tool result: %v", err)
	}
	if !result.IsError {
		t.Errorf("show of a missing task must set isError: %+v", result)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
}

// callTool invokes a tool and decodes its JSON text payload.
func callTool(ctx context.Context, t *testing.T, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s returned a tool error: %+v", name, result.Content)
	}
	if len(result.Content) != 1 {
		t.Fatalf("%s content = %+v", name, result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s content type = %T", name, result.Content[0])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatalf("%s payload %q: %v", name, text.Text, err)
	}
	return payload
}
