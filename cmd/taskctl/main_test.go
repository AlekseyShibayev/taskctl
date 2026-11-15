package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "taskctl "+version+"\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestHelpAndUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "taskctl mcp") {
		t.Errorf("help output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("no args exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"nope"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("unknown command exit code = %d, want 2", code)
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

func TestDoctorCreatesDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	dbPath := filepath.Join(dir, "taskctl", "tasks.db")
	if !strings.Contains(stdout.String(), "db: "+dbPath) {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "tasks: 0") || !strings.Contains(stdout.String(), "ok") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "returned") {
		t.Errorf("doctor must not mention returned: %q", stdout.String())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database file: %v", err)
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
	if code := run([]string{"mcp"}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses = %d: %q", len(lines), stdout.String())
	}
	if !strings.Contains(lines[0], `"taskctl"`) || !strings.Contains(lines[1], `"add"`) {
		t.Errorf("responses = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "taskctl", "tasks.db")); err != nil {
		t.Errorf("database file: %v", err)
	}
}

func TestDoctorJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var report struct {
		OK                    bool   `json:"ok"`
		DB                    string `json:"db"`
		SchemaVersion         int    `json:"schema_version"`
		ExpectedSchemaVersion int    `json:"expected_schema_version"`
		JournalMode           string `json:"journal_mode"`
		Checks                []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode JSON: %v (%s)", err, stdout.String())
	}
	if !report.OK {
		t.Errorf("report not ok: %+v", report)
	}
	if report.DB != filepath.Join(dir, "taskctl", "tasks.db") {
		t.Errorf("db = %q", report.DB)
	}
	if report.SchemaVersion != report.ExpectedSchemaVersion || report.SchemaVersion == 0 {
		t.Errorf("schema = %d, expected %d", report.SchemaVersion, report.ExpectedSchemaVersion)
	}
	if !strings.EqualFold(report.JournalMode, "wal") {
		t.Errorf("journal mode = %q", report.JournalMode)
	}
	names := make([]string, len(report.Checks))
	for i, check := range report.Checks {
		names[i] = check.Name
		if !check.OK {
			t.Errorf("check %s failed: %s", check.Name, check.Detail)
		}
	}
	if strings.Join(names, ",") != "schema,journal_mode,permissions,writable,integrity" {
		t.Errorf("checks = %v", names)
	}
}

func TestDoctorUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "--nope"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestMCPStopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := runContext(ctx, []string{"mcp"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoctorReportsLooseDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("first doctor: %d, %s", code, stderr.String())
	}
	dataDir := filepath.Join(dir, "taskctl")
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("chmod data dir: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"doctor"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1 for a loose directory", code)
	}
	if !strings.Contains(stdout.String(), "FAIL permissions") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestDoctorJSONOnOpenError(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", blocker)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	var report struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v (%s)", err, stdout.String())
	}
	if report.OK || report.Error == "" {
		t.Errorf("report = %+v", report)
	}
}
