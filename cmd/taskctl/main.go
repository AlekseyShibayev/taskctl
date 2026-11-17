package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AlekseyShibayev/taskctl/internal/mcp"
	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `taskctl — local task queue (MCP over stdio)

Usage:
  taskctl mcp      run the MCP server on stdio

Database: $XDG_DATA_HOME/taskctl/tasks.db
(or ~/.local/share/taskctl/tasks.db when XDG_DATA_HOME is unset).
The database is created on the first session.
`

func main() {
	// SIGTERM/SIGINT end the session cleanly, so the WAL is checkpointed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the signal-free entry point used by tests.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdin, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "mcp":
		return runMCP(ctx, stdin, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "taskctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runMCP(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	st, err := openStore()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	err = mcp.NewServer(st, version).Serve(ctx, stdin, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}
	return 0
}

func openStore() (*store.Store, error) {
	path, err := databasePath()
	if err != nil {
		return nil, err
	}
	return store.Open(path)
}

// databasePath resolves $XDG_DATA_HOME/taskctl/tasks.db.
func databasePath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" {
		return filepath.Join(dir, "taskctl", "tasks.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "taskctl", "tasks.db"), nil
}
