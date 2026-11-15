package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlekseyShibayev/taskctl/internal/mcp"
	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// auditWriter records server output and can block or fail a chosen write, so a
// test can play the part of a client that stopped reading stdout.
type auditWriter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	buf       bytes.Buffer
	writes    int
	armAt     int
	hold      bool
	fail      bool
	armed     bool
	released  bool
	failDelay time.Duration
	reached   chan struct{}
	once      sync.Once
}

func newAuditWriter(armAt int, hold, fail bool) *auditWriter {
	w := &auditWriter{armAt: armAt, hold: hold, fail: fail, reached: make(chan struct{})}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *auditWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.armAt > 0 && w.writes >= w.armAt {
		w.armed = true
		w.once.Do(func() { close(w.reached) })
	}
	if w.hold {
		for w.armed && !w.released {
			w.cond.Wait()
		}
	}
	if w.armed && w.fail {
		delay := w.failDelay
		if delay > 0 {
			w.mu.Unlock()
			time.Sleep(delay)
			w.mu.Lock()
		}
		return 0, errors.New("stdout is a broken pipe")
	}
	return w.buf.Write(p)
}

func (w *auditWriter) release() {
	w.mu.Lock()
	w.released = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

func (w *auditWriter) waitArmed(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-w.reached:
	case <-time.After(timeout):
		t.Fatalf("server never attempted the blocked write")
	}
}

func (w *auditWriter) lineCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.buf.String(), "\n")
}

func (w *auditWriter) envelopes(t *testing.T) []rpcEnvelope {
	t.Helper()
	w.mu.Lock()
	raw := strings.TrimSpace(w.buf.String())
	w.mu.Unlock()
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	envelopes := make([]rpcEnvelope, 0, len(lines))
	for _, line := range lines {
		var envelope rpcEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes
}

// auditSession is a full-duplex MCP server wired to io.Pipe, so a test controls
// when the client writes and when it reads.
type auditSession struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	out  *auditWriter
	done chan struct{}
	err  error
}

func startAuditServer(t *testing.T, ctx context.Context, st *store.Store, armAt int, hold, fail bool) *auditSession {
	t.Helper()
	return startAuditServerWith(t, ctx, st, newAuditWriter(armAt, hold, fail))
}

func startAuditServerWith(t *testing.T, ctx context.Context, st *store.Store, out *auditWriter) *auditSession {
	t.Helper()
	pr, pw := io.Pipe()
	s := &auditSession{pr: pr, pw: pw, out: out, done: make(chan struct{})}
	go func() {
		s.err = mcp.NewServer(st, "test").Serve(ctx, pr, s.out)
		close(s.done)
	}()
	t.Cleanup(func() {
		s.out.release()
		_ = pw.Close()
		_ = pr.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	})
	return s
}

func (s *auditSession) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.pw, line+"\n"); err != nil {
		t.Fatalf("send request: %v", err)
	}
}

func (s *auditSession) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-s.done:
		return s.err
	case <-time.After(timeout):
		t.Fatalf("Serve did not return within %s", timeout)
		return nil
	}
}

func initLine() string {
	return encodeRequest(0, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
}

// H1: the tools/call queue holds 256 jobs and silently answers "server busy"
// once it is full, so a pipelined batch loses requests. A slow client must get
// backpressure, not dropped calls.
func TestAuditH1PipelinedCallsAreNotDropped(t *testing.T) {
	st := newStore(t)
	s := startAuditServer(t, context.Background(), st, 2, true, false)
	s.send(t, initLine())

	const calls = 300
	s.send(t, encodeTool(1, "create", map[string]any{"name": "task", "content": "x"}))
	s.out.waitArmed(t, 5*time.Second)

	feed := make(chan error, 1)
	go func() {
		var b strings.Builder
		for i := 2; i <= calls; i++ {
			b.WriteString(encodeTool(i, "create", map[string]any{"name": "task", "content": "x"}))
			b.WriteString("\n")
		}
		_, err := io.WriteString(s.pw, b.String())
		feed <- err
	}()

	// Give the read loop time to run ahead of the blocked worker.
	time.Sleep(300 * time.Millisecond)
	s.out.release()

	select {
	case err := <-feed:
		if err != nil {
			t.Fatalf("write requests: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("read loop never accepted all %d pipelined calls", calls)
	}
	_ = s.pw.Close()
	if err := s.wait(t, 10*time.Second); err != nil {
		t.Fatalf("serve: %v", err)
	}

	answered := make(map[int]bool, calls)
	failures := 0
	firstFailure := ""
	for _, envelope := range s.out.envelopes(t) {
		id := numericID(envelope.ID)
		if id == 0 {
			continue
		}
		if envelope.Error != nil {
			failures++
			if firstFailure == "" {
				firstFailure = envelope.Error.Message
			}
			continue
		}
		answered[id] = true
	}
	if failures > 0 {
		t.Fatalf("%d of %d pipelined calls were rejected (first: %q)", failures, calls, firstFailure)
	}
	for i := 1; i <= calls; i++ {
		if !answered[i] {
			t.Fatalf("call %d got no successful response (%d of %d answered)", i, len(answered), calls)
		}
	}

	page, err := st.List(context.Background(), store.ListFilter{Limit: store.MaxLimit})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(page.Tasks) != calls || page.HasNext {
		t.Fatalf("%d of %d pipelined creates were committed", len(page.Tasks), calls)
	}
}

// C1: slice answers with full task contents, so the response is unbounded by
// bytes. The page must stay under the client's 4 MiB scanner limit and carry
// a preview plus the full length, with the body fetched through get.
func TestAuditC1SliceResponseIsBounded(t *testing.T) {
	st := newStore(t)
	body := strings.Repeat("x", store.MaxContentBytes)
	for i := 0; i < 4; i++ {
		if _, err := st.Add(context.Background(), "big", &body); err != nil {
			t.Fatalf("add task %d: %v", i, err)
		}
	}

	envelopes := runServer(t, st, encodeTool(1, "slice", map[string]any{"limit": store.MaxLimit}))
	if len(envelopes) != 1 {
		t.Fatalf("responses = %d", len(envelopes))
	}
	if envelopes[0].Error != nil {
		t.Fatalf("slice failed: %+v", envelopes[0].Error)
	}
	if size := len(envelopes[0].Result); size > 3<<20 {
		t.Errorf("slice result is %d bytes, must stay under 3 MiB", size)
	}

	payload, _ := toolPayload(t, envelopes[0])
	tasks, _ := payload["tasks"].([]any)
	if len(tasks) == 0 {
		t.Fatalf("payload = %v", payload)
	}
	first, _ := tasks[0].(map[string]any)
	preview, ok := first["content_preview"].(string)
	if !ok {
		t.Fatalf("slice returns the full content and has no content_preview: %v keys", mapKeys(first))
	}
	if len(preview) > 4<<10 {
		t.Errorf("content_preview is %d bytes, must be at most 4 KiB", len(preview))
	}
	if first["content_bytes"] != float64(store.MaxContentBytes) {
		t.Errorf("content_bytes = %v, want %d", first["content_bytes"], store.MaxContentBytes)
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// H2: the update schema advertises name and content, but the handler decodes
// only id/status, so a task cannot be renamed through the only public API.
func TestAuditH2UpdateAcceptsNameAndContent(t *testing.T) {
	st := newStore(t)
	runServer(t, st, encodeTool(1, "create", map[string]any{"name": "original", "content": "body"}))

	renamed := runServer(t, st, encodeTool(1, "update", map[string]any{"id": 1, "name": "renamed"}))
	text, isError := auditResultText(t, renamed[0])
	if isError {
		t.Fatalf("update with name was rejected: %s", text)
	}

	current := runServer(t, st, encodeTool(1, "get", map[string]any{"id": 1}))
	payload, _ := toolPayload(t, current[0])
	if payload["name"] != "renamed" {
		t.Fatalf("name = %v, want renamed", payload["name"])
	}
	if payload["content"] != "body" {
		t.Fatalf("content changed to %v", payload["content"])
	}

	edited := runServer(t, st, encodeTool(1, "update", map[string]any{"id": 1, "content": "new body"}))
	if text, isError := auditResultText(t, edited[0]); isError {
		t.Fatalf("update with content was rejected: %s", text)
	}
	current = runServer(t, st, encodeTool(1, "get", map[string]any{"id": 1}))
	if payload, _ := toolPayload(t, current[0]); payload["content"] != "new body" {
		t.Fatalf("content = %v, want new body", payload["content"])
	}

	empty := runServer(t, st, encodeTool(1, "update", map[string]any{"id": 1}))
	if _, isError := auditResultText(t, empty[0]); !isError {
		t.Fatalf("update without fields must fail: %s", empty[0].Result)
	}
}

func auditResultText(t *testing.T, envelope rpcEnvelope) (string, bool) {
	t.Helper()
	if envelope.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", envelope.Error)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, envelope.Result)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("content = %+v", result.Content)
	}
	return result.Content[0].Text, result.IsError
}

// H3: serveErr is written by the worker and read by the read loop without
// synchronisation. The error is evaluated at EOF, before wg.Wait, so a failed
// write is reported as success.
func TestAuditH3ServeReportsLateWriteError(t *testing.T) {
	st := newStore(t)
	out := newAuditWriter(2, true, true)
	// Let the read loop observe EOF before the failed write, so the error can
	// only be caught by a synchronised read after the worker has finished.
	out.failDelay = 250 * time.Millisecond
	s := startAuditServerWith(t, context.Background(), st, out)
	s.send(t, initLine())
	s.send(t, encodeTool(1, "create", map[string]any{"name": "task", "content": "x"}))

	s.out.waitArmed(t, 5*time.Second)
	// EOF first, then the blocked write fails: the return value is already
	// evaluated and only errState after wg.Wait can preserve the error.
	_ = s.pw.Close()
	s.out.release()

	if err := s.wait(t, 5*time.Second); err == nil {
		t.Fatalf("Serve returned nil although every response write failed")
	}
}

// H4: a failed write does not stop the worker, so queued side effects are
// committed with their responses going nowhere. Committed tasks must not
// outnumber delivered responses.
func TestAuditH4NoCommitAfterWriteFailure(t *testing.T) {
	st := newStore(t)
	lines := []string{initLine()}
	for i := 1; i <= 5; i++ {
		lines = append(lines, encodeTool(i, "create", map[string]any{"name": "task", "content": "x"}))
	}
	out := newAuditWriter(2, false, true) // the first tool response fails, then all others
	_ = mcp.NewServer(st, "test").Serve(context.Background(),
		strings.NewReader(strings.Join(lines, "\n")+"\n"), out)

	page, err := st.List(context.Background(), store.ListFilter{Limit: store.MaxLimit})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	delivered := 0
	for _, envelope := range out.envelopes(t) {
		if numericID(envelope.ID) != 0 {
			delivered++
		}
	}
	if len(page.Tasks) > delivered+1 {
		t.Fatalf("%d create calls were committed, but only %d responses reached the client",
			len(page.Tasks), delivered)
	}
}

// H6: writeMu is held across encoder.Encode on a raw os.Stdout, so a client
// that does not read stdout blocks the read loop and the server stops accepting
// input (classic two-way deadlock).
func TestAuditH6BlockedClientDoesNotStopIntake(t *testing.T) {
	st := newStore(t)
	s := startAuditServer(t, context.Background(), st, 2, true, false)
	s.send(t, initLine())
	s.send(t, encodeTool(1, "create", map[string]any{"name": "task", "content": "x"}))
	s.out.waitArmed(t, 5*time.Second)

	const pings = 100
	feed := make(chan error, 1)
	go func() {
		for i := 0; i < pings; i++ {
			if _, err := io.WriteString(s.pw, encodeRequest(100+i, "ping", nil)+"\n"); err != nil {
				feed <- err
				return
			}
		}
		feed <- nil
	}()

	select {
	case err := <-feed:
		if err != nil {
			t.Fatalf("write requests: %v", err)
		}
	case <-time.After(2 * time.Second):
		s.out.release()
		t.Fatalf("read loop stopped consuming stdin while the client did not read stdout")
	}

	s.out.release()
	_ = s.pw.Close()
	if err := s.wait(t, 10*time.Second); err != nil {
		t.Fatalf("serve: %v", err)
	}
	answered := 0
	for _, envelope := range s.out.envelopes(t) {
		if numericID(envelope.ID) >= 100 {
			answered++
		}
	}
	if answered != pings {
		t.Fatalf("%d of %d pings were answered", answered, pings)
	}
}

// H5: SIGTERM cancels the context, but the read loop never selects on it and
// readMessage has no deadline, so Serve stays blocked and in-flight work is
// abandoned.
func TestAuditH5ServeStopsOnContextCancel(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := startAuditServer(t, ctx, st, 0, false, false)
	s.send(t, initLine())

	deadline := time.Now().Add(2 * time.Second)
	for s.out.lineCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := s.wait(t, time.Second); err != nil {
		// A cancelled context is a clean shutdown, not a protocol error.
		t.Logf("serve returned %v", err)
	}
}
