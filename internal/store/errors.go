// Package store owns the SQLite database and the task lifecycle rules.
package store

import "fmt"

// Stable error codes returned to MCP clients.
const (
	CodeInvalidArgument   = "invalid_argument"
	CodeNotFound          = "not_found"
	CodeInvalidTransition = "invalid_transition"
	CodeInternal          = "internal"
)

// Error is a domain error with a machine-readable code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func invalidArgumentf(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func notFoundf(format string, args ...any) *Error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

func invalidTransitionf(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidTransition, Message: fmt.Sprintf(format, args...)}
}
