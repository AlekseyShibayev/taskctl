package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// Protocol versions understood by the server.
const (
	protocol20241105 = "2024-11-05"
	protocol20250326 = "2025-03-26"
	protocol20250618 = "2025-06-18"
)

// JSON-RPC error codes.
const (
	codeParseError           = -32700
	codeInvalidRequest       = -32600
	codeMethodNotFound       = -32601
	codeInvalidParams        = -32602
	codeServerNotInitialized = -32002
	codeInternalError        = -32603
	codeRequestCancelled     = -32800
)

// maxMessageSize bounds one incoming JSON-RPC line. A single huge line must not
// be able to exhaust memory: the rest of an oversized line is discarded and the
// client gets an error instead.
const maxMessageSize = 4 << 20

// defaultCallTimeout bounds one tools/call invocation.
const defaultCallTimeout = 30 * time.Second

var errMessageTooLarge = errors.New("message exceeds maximum size")

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server speaks MCP over a byte stream (stdio).
type Server struct {
	store   *store.Store
	version string
	logger  *slog.Logger

	mu          sync.Mutex
	initialized bool
	inflight    map[string]context.CancelFunc
	callTimeout time.Duration
}

// NewServer builds a server backed by st. A nil logger discards all logs.
func NewServer(st *store.Store, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		store:       st,
		version:     version,
		logger:      logger,
		inflight:    make(map[string]context.CancelFunc),
		callTimeout: defaultCallTimeout,
	}
}

// Serve reads newline-delimited JSON-RPC messages until EOF or until ctx is
// cancelled. Malformed messages are answered and do not stop the stream.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reader := bufio.NewReaderSize(r, 64<<10)
	encoder := json.NewEncoder(w)

	var writeMu sync.Mutex
	var errOnce sync.Once
	var serveErr error
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	recordErr := func(err error) {
		errOnce.Do(func() { serveErr = err })
		stop()
	}
	write := func(resp response) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
		return nil
	}

	// A blocked read (stdin from an MCP client) does not wake up on a signal by
	// itself, and closing the file from another goroutine does not interrupt an
	// in-flight read(2) either. A read deadline does: on cancellation the read
	// returns immediately and the loop treats it as shutdown.
	deadliner, canDeadline := r.(interface{ SetReadDeadline(time.Time) error })
	done := make(chan struct{})
	defer close(done)
	if canDeadline {
		go func() {
			select {
			case <-runCtx.Done():
				_ = deadliner.SetReadDeadline(time.Now())
			case <-done:
			}
		}()
		defer func() { _ = deadliner.SetReadDeadline(time.Time{}) }()
	}

	type callJob struct {
		ctx    context.Context
		req    request
		cancel context.CancelFunc
		key    string
	}

	// The reader runs in its own goroutine so Serve returns on cancellation even
	// when read(2) is blocked (an inherited stdin cannot be interrupted from
	// another goroutine). The goroutine may outlive Serve; it owns no state and
	// the process exits right after a signal.
	type readResult struct {
		line []byte
		err  error
	}
	messages := make(chan readResult)
	go func() {
		for {
			line, err := readMessage(reader)
			select {
			case messages <- readResult{line: line, err: err}:
			case <-runCtx.Done():
				return
			}
			if err != nil && !errors.Is(err, errMessageTooLarge) {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	// Buffered so the read loop is never stuck handing a request to a busy
	// worker: notifications/cancelled must still get through.
	jobs := make(chan callJob, 256)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// One worker preserves the order of tools/call requests, while the
		// read loop stays free to deliver notifications/cancelled.
		for job := range jobs {
			resp, reply := s.handle(job.ctx, job.req)
			if reply {
				if err := write(resp); err != nil {
					recordErr(err)
				}
			}
			if job.key != "" {
				s.clearInflight(job.key)
			}
			job.cancel()
		}
	}()
	defer func() {
		// Let requests that were already read finish before the session context
		// is cancelled: EOF on stdin must not cancel a call in progress.
		close(jobs)
		wg.Wait()
		stop()
	}()

	for {
		var (
			line []byte
			err  error
		)
		select {
		case <-runCtx.Done():
			return serveErr
		case message := <-messages:
			line, err = message.line, message.err
		}
		if errors.Is(err, errMessageTooLarge) {
			s.logger.Warn("rejecting oversized message", "limit_bytes", maxMessageSize)
			if writeErr := write(parseFailure(codeInvalidRequest,
				fmt.Sprintf("request exceeds maximum size of %d bytes", maxMessageSize))); writeErr != nil {
				recordErr(writeErr)
				return serveErr
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return serveErr
		}
		if err != nil {
			if runCtx.Err() != nil {
				return serveErr
			}
			recordErr(fmt.Errorf("read request: %w", err))
			return serveErr
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			// Valid JSON with the wrong field types is an invalid request, not a
			// parse error; only malformed JSON gets -32700.
			code, message := codeParseError, "parse error: "+err.Error()
			if json.Valid(line) {
				code, message = codeInvalidRequest, "invalid request: "+err.Error()
			}
			s.logger.Warn("rejected message", "code", code, "error", err)
			if err := write(parseFailure(code, message)); err != nil {
				recordErr(err)
				return serveErr
			}
			continue
		}
		if req.JSONRPC != "2.0" {
			s.logger.Warn("invalid request", "jsonrpc", req.JSONRPC)
			if err := write(requestFailure(req, codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)); err != nil {
				recordErr(err)
				return serveErr
			}
			continue
		}
		if req.Method == "" {
			if err := write(requestFailure(req, codeInvalidRequest, "invalid request: method is required")); err != nil {
				recordErr(err)
				return serveErr
			}
			continue
		}

		if req.Method == "notifications/cancelled" {
			s.cancelRequest(req.Params)
			continue
		}

		// tools/call can block on I/O, so it is handed to the worker: that is
		// what makes notifications/cancelled able to reach a running call.
		// Other methods are cheap and stay on the read loop.
		if req.Method == "tools/call" {
			key := idKey(req.ID)
			timeout := s.callTimeout
			if timeout <= 0 {
				timeout = defaultCallTimeout
			}
			callCtx, cancel := context.WithTimeout(runCtx, timeout)
			if key != "" {
				s.mu.Lock()
				s.inflight[key] = cancel
				s.mu.Unlock()
			}
			select {
			case jobs <- callJob{ctx: callCtx, req: req, cancel: cancel, key: key}:
			default:
				// The queue is full: answer instead of blocking the read loop,
				// which must stay available for cancellations.
				cancel()
				if key != "" {
					s.clearInflight(key)
				}
				s.logger.Warn("tool call queue full")
				if err := write(requestFailure(req, codeInternalError, "server busy: too many pending tool calls")); err != nil {
					recordErr(err)
					return serveErr
				}
			}
			continue
		}

		resp, reply := s.handle(runCtx, req)
		if !reply {
			continue
		}
		if err := write(resp); err != nil {
			recordErr(err)
			return serveErr
		}
	}
}

// readMessage returns one line (without the trailing newline). It reads in
// bounded chunks, so the memory used by a message is capped.
func readMessage(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxMessageSize {
			if errors.Is(err, bufio.ErrBufferFull) {
				if _, discardErr := discardLine(r); discardErr != nil && !errors.Is(discardErr, io.EOF) {
					return nil, discardErr
				}
			}
			return nil, errMessageTooLarge
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return buf, nil
		default:
			return nil, err
		}
	}
}

// discardLine consumes the remainder of an oversized line.
func discardLine(r *bufio.Reader) (int, error) {
	discarded := 0
	for {
		chunk, err := r.ReadSlice('\n')
		discarded += len(chunk)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return discarded, err
		}
	}
}

func parseFailure(code int, message string) response {
	return response{JSONRPC: "2.0", Error: &rpcError{Code: code, Message: message}}
}

func requestFailure(req request, code int, message string) response {
	return response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: message}}
}

func idKey(id json.RawMessage) string {
	if len(id) == 0 || string(id) == "null" {
		return ""
	}
	return string(id)
}

func (s *Server) cancelRequest(raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		s.logger.Warn("invalid cancellation", "error", err)
		return
	}
	key := idKey(params.RequestID)
	s.mu.Lock()
	cancel, ok := s.inflight[key]
	s.mu.Unlock()
	if !ok {
		s.logger.Debug("cancellation for unknown request", "request_id", key)
		return
	}
	s.logger.Debug("cancelling request", "request_id", key, "reason", params.Reason)
	cancel()
}

func (s *Server) clearInflight(key string) {
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
}

func (s *Server) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *Server) markInitialized() {
	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
}

func (s *Server) handle(ctx context.Context, req request) (response, bool) {
	hasID := len(req.ID) > 0 && string(req.ID) != "null"
	resp := response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = s.initialize(req.Params)
		s.markInitialized()
	case "notifications/initialized", "notifications/cancelled":
		return response{}, false
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		if !s.isInitialized() {
			resp.Error = &rpcError{Code: codeServerNotInitialized, Message: "server not initialized: send initialize first"}
			break
		}
		resp.Result = listToolsResult{Tools: toolDefinitions()}
	case "tools/call":
		if !s.isInitialized() {
			resp.Error = &rpcError{Code: codeServerNotInitialized, Message: "server not initialized: send initialize first"}
			break
		}
		result, rpcErr := s.callTool(ctx, req.Params)
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
	default:
		if !hasID {
			return response{}, false
		}
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}

	if !hasID {
		return response{}, false
	}
	return resp, true
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type capabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions,omitempty"`
}

func (s *Server) initialize(raw json.RawMessage) initializeResult {
	var params initializeParams
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &params)
	}
	return initializeResult{
		ProtocolVersion: negotiateVersion(params.ProtocolVersion),
		Capabilities:    capabilities{Tools: &toolsCapability{}},
		ServerInfo:      serverInfo{Name: "taskctl", Version: s.version},
		Instructions: "Local task tracker. Use add to create a task, list and show to read tasks, " +
			"update to edit name, content or status. Statuses: create -> in_progress -> done; " +
			"in_progress -> create sends a task back for rework. Tasks are never deleted.",
	}
}

func negotiateVersion(requested string) string {
	switch requested {
	case protocol20241105, protocol20250326, protocol20250618:
		return requested
	default:
		return protocol20250618
	}
}

// callTool dispatches a tools/call payload.
func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (toolResult, *rpcError) {
	var params callToolParams
	if err := decodeParams(raw, &params); err != nil {
		return toolResult{}, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	handler, ok := toolHandlers[params.Name]
	if !ok {
		return toolResult{}, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", params.Name)}
	}
	payload, err := handler(ctx, s.store, params.Arguments)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			message := "request cancelled"
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				message = "request timed out"
			}
			s.logger.Warn("tool call interrupted", "tool", params.Name, "reason", message)
			return toolResult{}, &rpcError{Code: codeRequestCancelled, Message: message}
		}
		var domain *store.Error
		if errors.As(err, &domain) {
			s.logger.Warn("tool call failed", "tool", params.Name, "code", domain.Code)
		} else {
			s.logger.Error("tool call failed", "tool", params.Name, "error", err)
		}
		return errorResult(err), nil
	}
	s.logger.Debug("tool call succeeded", "tool", params.Name)
	return jsonResult(payload), nil
}
