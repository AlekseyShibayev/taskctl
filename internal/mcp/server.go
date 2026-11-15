package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// protocolVersion is the only MCP revision this server speaks. Clients on
// another revision are answered with this one; the tool set is version-agnostic.
const protocolVersion = "2025-06-18"

// JSON-RPC error codes.
const (
	codeParseError           = -32700
	codeInvalidRequest       = -32600
	codeMethodNotFound       = -32601
	codeInvalidParams        = -32602
	codeServerNotInitialized = -32002
	codeInternalError        = -32603
)

// maxMessageSize bounds one incoming JSON-RPC line, so a single huge line
// cannot exhaust memory.
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

	mu          sync.Mutex
	initialized bool
}

// NewServer builds a server backed by st.
func NewServer(st *store.Store, version string) *Server {
	return &Server{store: st, version: version}
}

// Serve reads newline-delimited JSON-RPC messages until EOF. Malformed
// messages are answered and do not stop the stream.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reader := bufio.NewReaderSize(r, 64<<10)
	encoder := json.NewEncoder(w)

	var (
		writeMu  sync.Mutex
		errOnce  sync.Once
		serveErr error
	)
	write := func(resp response) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
		return nil
	}
	recordErr := func(err error) {
		errOnce.Do(func() { serveErr = err })
	}

	// tools/call runs on a worker so the read loop stays free: a client may
	// pipeline requests and would otherwise deadlock writing while the server
	// blocks writing a response to the same pipe.
	type job struct {
		ctx    context.Context
		cancel context.CancelFunc
		req    request
	}
	jobs := make(chan job, 256)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := range jobs {
			if resp, reply := s.handle(j.ctx, j.req); reply {
				if err := write(resp); err != nil {
					recordErr(err)
				}
			}
			j.cancel()
		}
	}()
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	for {
		line, err := readMessage(reader)
		switch {
		case errors.Is(err, errMessageTooLarge):
			if writeErr := write(parseFailure(codeInvalidRequest,
				fmt.Sprintf("request exceeds maximum size of %d bytes", maxMessageSize))); writeErr != nil {
				return writeErr
			}
			continue
		case errors.Is(err, io.EOF):
			return serveErr
		case err != nil:
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
			if err := write(parseFailure(code, message)); err != nil {
				return err
			}
			continue
		}
		if req.JSONRPC != "2.0" {
			if err := write(requestFailure(req, codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)); err != nil {
				return err
			}
			continue
		}
		if req.Method == "" {
			if err := write(requestFailure(req, codeInvalidRequest, "invalid request: method is required")); err != nil {
				return err
			}
			continue
		}

		if req.Method == "tools/call" {
			if req.ID == nil || string(req.ID) == "null" {
				// Without an id this is a notification: never run a side effect
				// that could not be reported.
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
			select {
			case jobs <- job{ctx: callCtx, cancel: cancel, req: req}:
			default:
				// The queue is full: answer instead of blocking the read loop.
				cancel()
				if err := write(requestFailure(req, codeInternalError, "server busy: too many pending tool calls")); err != nil {
					return err
				}
			}
			continue
		}

		resp, reply := s.handle(ctx, req)
		if !reply {
			continue
		}
		if err := write(resp); err != nil {
			return err
		}
	}
}

// readMessage returns one line without the trailing newline. It reads in
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
		resp.Result = s.initialize()
		s.markInitialized()
	case "notifications/initialized":
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

func (s *Server) initialize() initializeResult {
	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    capabilities{Tools: &toolsCapability{}},
		ServerInfo:      serverInfo{Name: "taskctl", Version: s.version},
		Instructions: "Local task tracker. Use create to make a task, slice and get to read tasks, " +
			"update to edit name, content or status. Statuses: create -> in_progress -> done; " +
			"in_progress -> create sends a task back for rework. Tasks are never deleted.",
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
		return errorResult(err), nil
	}
	return jsonResult(payload), nil
}
