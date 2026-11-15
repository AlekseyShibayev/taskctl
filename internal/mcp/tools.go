package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type listToolsResult struct {
	Tools []toolDefinition `json:"tools"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolHandler func(ctx context.Context, st *store.Store, args json.RawMessage) (any, error)

var toolHandlers = map[string]toolHandler{
	"add":    addTool,
	"list":   listTool,
	"show":   showTool,
	"update": updateTool,
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "add",
			Description: "Create a task. A new task starts in status create; name is required, content is optional.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string", "description": "Task name, must not be empty."},
					"content": map[string]any{"type": "string", "description": "Task description."},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
		{
			Name: "list",
			Description: "List tasks ordered by id. Optional status filter and pagination. " +
				"Returns {tasks, limit, offset, has_more}.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "enum": statusEnum(), "description": "Only tasks in this status."},
					"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": store.MaxLimit, "default": store.DefaultLimit},
					"offset": map[string]any{"type": "integer", "minimum": 0, "default": 0},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        "show",
			Description: "Show one task by id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "integer", "minimum": 1},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
		{
			Name: "update",
			Description: "Update a task. Only the provided fields change. " +
				"Allowed status moves: create->in_progress, in_progress->create (back for rework), " +
				"in_progress->done.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":      map[string]any{"type": "integer", "minimum": 1},
					"name":    map[string]any{"type": "string", "minLength": 1},
					"content": map[string]any{"type": "string"},
					"status":  map[string]any{"type": "string", "enum": statusEnum()},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
	}
}

func statusEnum() []string {
	values := make([]string, len(store.Statuses))
	for i, status := range store.Statuses {
		values[i] = string(status)
	}
	return values
}

func addTool(ctx context.Context, st *store.Store, raw json.RawMessage) (any, error) {
	var args struct {
		Name    string  `json:"name"`
		Content *string `json:"content"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return st.Add(ctx, args.Name, args.Content)
}

func listTool(ctx context.Context, st *store.Store, raw json.RawMessage) (any, error) {
	var args struct {
		Status *string `json:"status"`
		Limit  *int    `json:"limit"`
		Offset *int    `json:"offset"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}

	filter := store.ListFilter{}
	if args.Status != nil {
		status, err := store.ParseStatus(*args.Status)
		if err != nil {
			return nil, err
		}
		filter.Status = &status
	}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > store.MaxLimit {
			return nil, &store.Error{
				Code:    store.CodeInvalidArgument,
				Message: fmt.Sprintf("limit must be between 1 and %d", store.MaxLimit),
			}
		}
		filter.Limit = *args.Limit
	}
	if args.Offset != nil {
		filter.Offset = *args.Offset
	}
	return st.List(ctx, filter)
}

func showTool(ctx context.Context, st *store.Store, raw json.RawMessage) (any, error) {
	var args struct {
		ID *int64 `json:"id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	var id int64
	if args.ID != nil {
		id = *args.ID
	}
	return st.Get(ctx, id)
}

func updateTool(ctx context.Context, st *store.Store, raw json.RawMessage) (any, error) {
	var args struct {
		ID      *int64  `json:"id"`
		Name    *string `json:"name"`
		Content *string `json:"content"`
		Status  *string `json:"status"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}

	patch := store.Patch{Name: args.Name, Content: args.Content}
	if args.Status != nil {
		status, err := store.ParseStatus(*args.Status)
		if err != nil {
			return nil, err
		}
		patch.Status = &status
	}
	var id int64
	if args.ID != nil {
		id = *args.ID
	}
	return st.Update(ctx, id, patch)
}

// decodeArgs decodes tool arguments strictly, rejecting unknown fields.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return &store.Error{Code: store.CodeInvalidArgument, Message: "invalid arguments: " + err.Error()}
	}
	return nil
}

// decodeParams is lenient: MCP allows _meta and other protocol fields in params.
func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}

func jsonResult(payload any) toolResult {
	data, err := json.Marshal(payload)
	if err != nil {
		return errorResult(err)
	}
	return toolResult{Content: []contentBlock{{Type: "text", Text: string(data)}}}
}

func errorResult(err error) toolResult {
	domain := &store.Error{Code: store.CodeInternal, Message: err.Error()}
	var asDomain *store.Error
	if errors.As(err, &asDomain) {
		domain = asDomain
	}
	data, marshalErr := json.Marshal(domain)
	if marshalErr != nil {
		data = []byte(`{"code":"internal","message":"internal error"}`)
	}
	return toolResult{
		Content: []contentBlock{{Type: "text", Text: string(data)}},
		IsError: true,
	}
}
