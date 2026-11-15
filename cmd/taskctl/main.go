package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlekseyShibayev/taskctl/internal/mcp"
	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `taskctl — local task tracker (MCP over stdio)

Usage:
  taskctl mcp       run the MCP server on stdio
  taskctl doctor    check the database and print task counts
  taskctl version   print the version
  taskctl help      show this help

The database is $XDG_DATA_HOME/taskctl/tasks.db
(or ~/.local/share/taskctl/tasks.db when XDG_DATA_HOME is unset).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "mcp":
		return runMCP(stdin, stdout, stderr)
	case "doctor":
		return runDoctor(stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "taskctl %s\n", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "taskctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runMCP(stdin io.Reader, stdout, stderr io.Writer) int {
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}
	defer st.Close()

	if err := mcp.NewServer(st, version).Serve(stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}
	return 0
}

func runDoctor(stdout, stderr io.Writer) int {
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}
	defer st.Close()

	stats, err := st.Stats(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "db: %s\n", st.Path())
	fmt.Fprintf(stdout, "tasks: %d\n", stats.Total)
	for _, status := range store.Statuses {
		fmt.Fprintf(stdout, "  %-12s %d\n", status, stats.ByStatus[status])
	}
	fmt.Fprintln(stdout, "ok")
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
