package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AlekseyShibayev/taskctl/internal/logging"
	"github.com/AlekseyShibayev/taskctl/internal/mcp"
	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `taskctl — local task tracker (MCP over stdio)

Usage:
  taskctl mcp           run the MCP server on stdio
  taskctl doctor        check the database and print task counts
  taskctl doctor --json print the diagnostics as JSON
  taskctl version       print the version
  taskctl help          show this help

The database is $XDG_DATA_HOME/taskctl/tasks.db
(or ~/.local/share/taskctl/tasks.db when XDG_DATA_HOME is unset).
Logs go to stderr; set TASKCTL_LOG_LEVEL=debug|info|warn|error (default warn).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run keeps the testable signature; commands run with a background context.
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
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-v":
		_, _ = fmt.Fprintf(stdout, "taskctl %s\n", version)
		return 0
	case "help", "--help", "-h":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
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
	defer func() {
		if err := st.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "taskctl: %v\n", err)
		}
	}()

	logger := logging.New(stderr)
	if err := mcp.NewServer(st, version, logger).Serve(ctx, stdin, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}
	return 0
}

// doctorCheck is one diagnostic result.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// doctorReport is the machine-readable form of doctor output.
type doctorReport struct {
	OK                    bool          `json:"ok"`
	Version               string        `json:"version"`
	DB                    string        `json:"db"`
	SchemaVersion         int           `json:"schema_version"`
	ExpectedSchemaVersion int           `json:"expected_schema_version"`
	JournalMode           string        `json:"journal_mode"`
	Tasks                 store.Stats   `json:"tasks"`
	Checks                []doctorCheck `json:"checks"`
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			asJSON = true
		case "help", "--help", "-h":
			_, _ = fmt.Fprint(stdout, usage)
			return 0
		default:
			_, _ = fmt.Fprintf(stderr, "taskctl: unknown doctor flag %q\n\n%s", arg, usage)
			return 2
		}
	}

	report, err := diagnose(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "taskctl: %v\n", err)
		return 1
	}

	if asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "taskctl: encode report: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, string(data))
	} else {
		printReport(stdout, report)
	}
	if !report.OK {
		return 1
	}
	return 0
}

func diagnose(ctx context.Context) (doctorReport, error) {
	st, err := openStore()
	if err != nil {
		return doctorReport{}, err
	}
	defer func() { _ = st.Close() }()

	report := doctorReport{
		Version:               version,
		DB:                    st.Path(),
		ExpectedSchemaVersion: store.SupportedSchemaVersion(),
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		return doctorReport{}, err
	}
	report.Tasks = stats

	versionNow, err := st.SchemaVersion(ctx)
	if err != nil {
		return doctorReport{}, err
	}
	report.SchemaVersion = versionNow
	report.Checks = append(report.Checks, doctorCheck{
		Name:   "schema",
		OK:     versionNow == report.ExpectedSchemaVersion,
		Detail: fmt.Sprintf("%d (expected %d)", versionNow, report.ExpectedSchemaVersion),
	})

	mode, err := st.JournalMode(ctx)
	if err != nil {
		return doctorReport{}, err
	}
	report.JournalMode = mode
	report.Checks = append(report.Checks, doctorCheck{
		Name:   "journal_mode",
		OK:     strings.EqualFold(mode, "wal"),
		Detail: mode,
	})

	permissions, ok := permissionCheck(st.Path())
	report.Checks = append(report.Checks, doctorCheck{
		Name:   "permissions",
		OK:     ok,
		Detail: permissions,
	})

	if err := st.CheckWritable(ctx); err != nil {
		report.Checks = append(report.Checks, doctorCheck{Name: "writable", OK: false, Detail: err.Error()})
	} else {
		report.Checks = append(report.Checks, doctorCheck{Name: "writable", OK: true, Detail: "write transaction accepted"})
	}

	if result, err := st.QuickCheck(ctx); err != nil {
		report.Checks = append(report.Checks, doctorCheck{Name: "integrity", OK: false, Detail: err.Error()})
	} else {
		report.Checks = append(report.Checks, doctorCheck{Name: "integrity", OK: result == "ok", Detail: result})
	}

	report.OK = true
	for _, check := range report.Checks {
		if !check.OK {
			report.OK = false
		}
	}
	return report, nil
}

// permissionCheck verifies the database directory is 0700 and the file 0600.
func permissionCheck(dbPath string) (string, bool) {
	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		return fmt.Sprintf("data directory: %v", err), false
	}
	fileInfo, err := os.Stat(dbPath)
	if err != nil {
		return fmt.Sprintf("database file: %v", err), false
	}
	dirMode := dirInfo.Mode().Perm()
	fileMode := fileInfo.Mode().Perm()
	detail := fmt.Sprintf("dir %04o, file %04o", dirMode, fileMode)
	return detail, dirMode&0o077 == 0 && fileMode&0o077 == 0
}

func printReport(w io.Writer, report doctorReport) {
	_, _ = fmt.Fprintf(w, "db: %s\n", report.DB)
	_, _ = fmt.Fprintf(w, "version: %s\n", report.Version)
	_, _ = fmt.Fprintf(w, "schema: %d (expected %d)\n", report.SchemaVersion, report.ExpectedSchemaVersion)
	_, _ = fmt.Fprintf(w, "journal_mode: %s\n", report.JournalMode)
	_, _ = fmt.Fprintf(w, "tasks: %d\n", report.Tasks.Total)
	for _, status := range store.Statuses() {
		_, _ = fmt.Fprintf(w, "  %-12s %d\n", status, report.Tasks.ByStatus[status])
	}
	_, _ = fmt.Fprintf(w, "  %-12s %d\n", "unknown", report.Tasks.Unknown)
	for _, check := range report.Checks {
		mark := "ok  "
		if !check.OK {
			mark = "FAIL"
		}
		_, _ = fmt.Fprintf(w, "%s %-13s %s\n", mark, check.Name, check.Detail)
	}
	if report.OK {
		_, _ = fmt.Fprintln(w, "ok")
	} else {
		_, _ = fmt.Fprintln(w, "not ok")
	}
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
