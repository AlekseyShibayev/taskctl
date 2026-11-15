package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageAndErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("no args exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"nope"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("unknown command exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "nope"`) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDatabasePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	path, err := databasePath()
	if err != nil {
		t.Fatalf("databasePath: %v", err)
	}
	if want := filepath.Join("/data", "taskctl", "tasks.db"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/tester")
	path, err = databasePath()
	if err != nil {
		t.Fatalf("databasePath: %v", err)
	}
	if want := filepath.Join("/home/tester", ".local", "share", "taskctl", "tasks.db"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestMCPCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"mcp"}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses = %d: %q", len(lines), stdout.String())
	}
	if !strings.Contains(lines[0], `"taskctl"`) || !strings.Contains(lines[1], `"create"`) {
		t.Errorf("responses = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "taskctl", "tasks.db")); err != nil {
		t.Errorf("database file: %v", err)
	}
}

func TestMCPStopsOnCancelledContext(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"mcp"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
}
