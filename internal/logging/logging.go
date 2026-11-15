// Package logging builds the structured stderr logger. MCP owns stdout, so
// every diagnostic must go to stderr.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a text logger writing to w. The level comes from
// TASKCTL_LOG_LEVEL (debug, info, warn, error); the default is warn.
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: LevelFromEnv()}))
}

// LevelFromEnv maps TASKCTL_LOG_LEVEL to a slog level.
func LevelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TASKCTL_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
