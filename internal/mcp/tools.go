package mcp

import (
	"bytes"
	"context"
	"encoding/json"
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
	"create": createTool,
	"get":    getTool,
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "create",
			Description: "Create a task. It stays unclaimed (get_at null) until get takes it.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Task text, must not be empty (at most 2000 characters).",
						"minLength":   1,
						"maxLength":   store.MaxTextChars,
					},
				},
				"required":             []string{"text"},
				"additionalProperties": false,
			},
		},
		{
			Name: "get",
			Description: "Take the oldest task that has not been taken yet (the lowest id with get_at null) " +
				"and mark it taken. Fails when every task has been taken.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}
}

func createTool(ctx context.Context, st *store.Store, raw json.RawMessage) (any, error) {
	var args struct {
		Text *string `json:"text"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Text == nil {
		return nil, requiredArgumentf("text")
	}
	return st.Add(ctx, *args.Text)
}

func getTool(ctx context.Context, st *store.Store, raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return st.Take(ctx)
}

// requiredArgumentf reports a missing mandatory argument separately from a
// malformed one: "text is required" is not "text must not be empty".
func requiredArgumentf(name string) error {
	return fmt.Errorf("%s is required", name)
}

// decodeArgs decodes tool arguments strictly, rejecting unknown fields.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
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
	return toolResult{
		Content: []contentBlock{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}
