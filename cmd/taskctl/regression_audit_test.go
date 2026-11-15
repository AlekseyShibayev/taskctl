package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// failingStdout accepts one response and fails every write after it. The
// failure is delayed so the read loop sees EOF before the write error: only a
// synchronised error state can report it.
type failingStdout struct {
	writes int
}

func (w *failingStdout) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		time.Sleep(200 * time.Millisecond)
		return 0, errors.New("stdout is a broken pipe")
	}
	return len(p), nil
}

// H3: Serve evaluates serveErr before wg.Wait, so a response that could not be
// written is reported as success and the process exits with code 0.
func TestAuditH3ExitCodeOnWriteFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create","arguments":{"name":"task","content":"x"}}}`,
	}, "\n") + "\n"

	var stderr bytes.Buffer
	code := run(context.Background(), []string{"mcp"}, strings.NewReader(input), &failingStdout{}, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
}
