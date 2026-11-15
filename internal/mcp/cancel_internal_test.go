// Package mcp tests cancellation from inside the protocol implementation.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// A notifications/cancelled must reach a tool that is already running and turn
// its response into -32800 instead of waiting for the call timeout.
func TestCancellationStopsRunningTool(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	original, existed := toolHandlers["slow"]
	toolHandlers["slow"] = func(ctx context.Context, _ *store.Store, _ json.RawMessage) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return "late", nil
		}
	}
	t.Cleanup(func() {
		if existed {
			toolHandlers["slow"] = original
			return
		}
		delete(toolHandlers, "slow")
	})

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2,"reason":"test"}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	started := time.Now()
	if err := NewServer(st, "test", nil).Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s, want a prompt return", elapsed)
	}

	var cancelled bool
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var envelope struct {
			ID    int `json:"id"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if envelope.ID == 2 {
			if envelope.Error == nil || envelope.Error.Code != codeRequestCancelled {
				t.Fatalf("response = %+v, want code %d", envelope, codeRequestCancelled)
			}
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("no response for the cancelled request: %q", out.String())
	}
}
