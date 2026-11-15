// Package mcp tests cancellation from inside the protocol implementation.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// A call that outlives callTimeout must return -32800 instead of hanging.
func TestCallTimeout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	toolHandlers["hang"] = func(ctx context.Context, _ *store.Store, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { delete(toolHandlers, "hang") })

	server := NewServer(st, "test", nil)
	server.callTimeout = 50 * time.Millisecond

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hang","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	started := time.Now()
	if err := server.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}

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
			if !strings.Contains(envelope.Error.Message, "timed out") {
				t.Errorf("message = %q", envelope.Error.Message)
			}
			return
		}
	}
	t.Fatalf("no response for the timed-out call: %q", out.String())
}

// The queue must never block the read loop. One blocked call plus a full buffer
// makes the next call fail fast with -32603, while a cancellation is still read.
func TestQueueOverflowAnswersInternalError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var calls atomic.Int64
	toolHandlers["hang"] = func(ctx context.Context, _ *store.Store, _ json.RawMessage) (any, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return "ok", nil
	}
	t.Cleanup(func() { delete(toolHandlers, "hang") })

	server := NewServer(st, "test", nil)
	server.callTimeout = 200 * time.Millisecond

	lines := []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`}
	const callsToSend = 258 // one running + 256 buffered + one rejected
	for id := 2; id < 2+callsToSend; id++ {
		lines = append(lines, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"hang","arguments":{}}}`, id))
	}
	lines = append(lines,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`,
		`{"jsonrpc":"2.0","id":1000,"method":"ping"}`,
	)

	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	busy := 0
	cancelled := false
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var envelope struct {
			ID    int `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if envelope.Error == nil {
			continue
		}
		switch envelope.Error.Code {
		case codeInternalError:
			busy++
		case codeRequestCancelled:
			if envelope.ID == 2 {
				cancelled = true
			}
		}
	}
	if busy != 1 {
		t.Errorf("busy responses = %d, want 1", busy)
	}
	if !cancelled {
		t.Error("the running call was not cancelled")
	}
}
