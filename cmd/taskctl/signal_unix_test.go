//go:build unix

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A signal must end a session whose stdin is still open and idle: closing the
// file does not interrupt a blocked read, a read deadline does. The store must
// be closed and checkpointed on the way out.
func TestE2ESignalShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in short mode")
	}
	bin := buildTaskctl(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir, "TASKCTL_LOG_LEVEL=warn")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n")); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read initialize: %v (stderr %s)", err, stderr.String())
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil || len(envelope.Result) == 0 {
		t.Fatalf("initialize response %q: %v", line, err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait after SIGTERM: %v (stderr %s)", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("server did not exit within 5s after SIGTERM with stdin open")
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dataDir, "taskctl", "tasks.db") + suffix); !os.IsNotExist(err) {
			t.Errorf("%s left after shutdown: %v", suffix, err)
		}
	}
	if strings.Contains(stderr.String(), "taskctl:") {
		t.Errorf("stderr reports an error: %s", stderr.String())
	}
}
