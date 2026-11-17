package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// protocolVersion is the MCP revision this server speaks.
const protocolVersion = "2025-06-18"

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server speaks MCP over a byte stream (stdio), one JSON-RPC message per line.
type Server struct {
	store   *store.Store
	version string
}

// NewServer builds a server backed by st.
func NewServer(st *store.Store, version string) *Server {
	return &Server{store: st, version: version}
}

// Serve reads newline-delimited JSON-RPC messages until EOF or until ctx is
// cancelled. A malformed message is answered and does not stop the stream.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	type readResult struct {
		line []byte
		err  error
	}
	// Reading happens in a goroutine so that a cancelled context (SIGTERM)
	// ends the session without waiting for the next line. The goroutine exits
	// with the process if it is still blocked in ReadString.
	reads := make(chan readResult)
	go func() {
		reader := bufio.NewReader(r)
		for {
			line, err := reader.ReadString('\n')
			select {
			case reads <- readResult{line: []byte(line), err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	encoder := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return nil
		case res := <-reads:
			if strings.TrimSpace(string(res.line)) != "" {
				if resp, ok := s.handle(ctx, res.line); ok {
					if err := encoder.Encode(resp); err != nil {
						return fmt.Errorf("write response: %w", err)
					}
				}
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("read request: %w", res.err)
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return response{JSONRPC: "2.0", Error: &rpcError{
			Code: codeParseError, Message: "parse error: " + err.Error(),
		}}, true
	}
	// A message without an id is a notification: no response, no side effect.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return response{}, false
	}

	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.initialize()
	case "tools/list":
		resp.Result = listToolsResult{Tools: toolDefinitions()}
	case "tools/call":
		result, rpcErr := s.callTool(ctx, req.Params)
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
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
}

func (s *Server) initialize() initializeResult {
	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    capabilities{Tools: &toolsCapability{}},
		ServerInfo:      serverInfo{Name: "taskctl", Version: s.version},
	}
}

// callTool dispatches a tools/call payload.
func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (toolResult, *rpcError) {
	var params callToolParams
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return toolResult{}, &rpcError{Code: codeInvalidRequest, Message: "invalid request: " + err.Error()}
	}
	handler, ok := toolHandlers[params.Name]
	if !ok {
		return toolResult{}, &rpcError{
			Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", params.Name),
		}
	}
	result, err := handler(ctx, s.store, params.Arguments)
	if err != nil {
		if errors.Is(err, errInvalidArguments) {
			return toolResult{}, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		return errorResult(err), nil
	}
	return result, nil
}
