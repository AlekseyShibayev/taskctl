package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AlekseyShibayev/taskctl/internal/store"
)

// errInvalidArguments marks a malformed tools/call payload: it becomes a
// JSON-RPC -32602 error, not a tool result.
var errInvalidArguments = errors.New("invalid arguments")

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

type toolHandler func(ctx context.Context, st *store.Store, args json.RawMessage) (toolResult, error)

var toolHandlers = map[string]toolHandler{
	"create": createTool,
	"get":    getTool,
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "create",
			Description: "Queue a task from text. The task stays queued until get takes it.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Task text.",
					},
				},
				"required":             []string{"text"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "get",
			Description: "Take the next task and return its text. Fails when every task has been taken.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}
}

func createTool(ctx context.Context, st *store.Store, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Text *string `json:"text"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.Text == nil {
		return toolResult{}, errors.New("text is required")
	}
	if err := st.Create(ctx, *args.Text); err != nil {
		return toolResult{}, err
	}
	return emptyResult(), nil
}

func getTool(ctx context.Context, st *store.Store, raw json.RawMessage) (toolResult, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	text, err := st.Get(ctx)
	if err != nil {
		return toolResult{}, err
	}
	return textResult(text), nil
}

// decodeArgs decodes tool arguments strictly, rejecting unknown fields.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", errInvalidArguments, err)
	}
	return nil
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

// emptyResult is a successful tool call with no content, used by create.
func emptyResult() toolResult {
	return toolResult{Content: []contentBlock{}}
}

func errorResult(err error) toolResult {
	return toolResult{
		Content: []contentBlock{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}
