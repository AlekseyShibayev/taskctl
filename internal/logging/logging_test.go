package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlekseyShibayev/taskctl/internal/logging"
)

func TestLevelFromEnv(t *testing.T) {
	cases := map[string]slog.Level{
		"":         slog.LevelWarn,
		"debug":    slog.LevelDebug,
		"INFO":     slog.LevelInfo,
		" warning": slog.LevelWarn,
		"error":    slog.LevelError,
		"nonsense": slog.LevelWarn,
	}
	for value, want := range cases {
		t.Setenv("TASKCTL_LOG_LEVEL", value)
		if got := logging.LevelFromEnv(); got != want {
			t.Errorf("TASKCTL_LOG_LEVEL=%q: level = %v, want %v", value, got, want)
		}
	}
}

func TestNewUsesEnvLevel(t *testing.T) {
	t.Setenv("TASKCTL_LOG_LEVEL", "debug")
	var out bytes.Buffer
	logging.New(&out).Debug("hello", "tool", "add")
	if text := out.String(); !strings.Contains(text, "level=DEBUG") || !strings.Contains(text, "hello") {
		t.Errorf("debug log = %q", text)
	}

	t.Setenv("TASKCTL_LOG_LEVEL", "error")
	out.Reset()
	logger := logging.New(&out)
	logger.Info("too quiet")
	if out.Len() != 0 {
		t.Errorf("info must be filtered at error level: %q", out.String())
	}
	logger.Error("loud")
	if !strings.Contains(out.String(), "loud") {
		t.Errorf("error log = %q", out.String())
	}
}
