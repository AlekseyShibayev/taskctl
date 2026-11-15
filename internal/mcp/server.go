package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

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
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

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
}

// NewServer builds a server backed by st.
func NewServer(st *store.Store, version string) *Server {
	return &Server{store: st, version: version}
}

// Serve reads newline-delimited JSON-RPC requests until EOF.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	decoder := json.NewDecoder(r)
	encoder := json.NewEncoder(w)

	for {
		var req request
		if err := decoder.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			_ = encoder.Encode(response{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: codeParseError, Message: "parse error: " + err.Error()},
			})
			return fmt.Errorf("decode request: %w", err)
		}

		resp, reply := s.handle(req)
		if !reply {
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
}

func (s *Server) handle(req request) (response, bool) {
	hasID := len(req.ID) > 0 && string(req.ID) != "null"
	resp := response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = s.initialize(req.Params)
	case "notifications/initialized", "notifications/cancelled":
		return response{}, false
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		resp.Result = listToolsResult{Tools: toolDefinitions()}
	case "tools/call":
		result, rpcErr := s.callTool(req.Params)
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
func (s *Server) callTool(raw json.RawMessage) (toolResult, *rpcError) {
	var params callToolParams
	if err := decodeParams(raw, &params); err != nil {
		return toolResult{}, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	handler, ok := toolHandlers[params.Name]
	if !ok {
		return toolResult{}, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", params.Name)}
	}
	payload, err := handler(context.Background(), s.store, params.Arguments)
	if err != nil {
		return errorResult(err), nil
	}
	return jsonResult(payload), nil
}
