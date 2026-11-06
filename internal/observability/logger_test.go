package observability

import (
	"log/slog"
	"testing"

	"github.com/lmittmann/tint"
	"github.com/stretchr/testify/assert"
)

func TestParseLevel_Names_MapToSlogLevels(t *testing.T) {
	tests := map[string]slog.Level{
		LevelDebug: slog.LevelDebug,
		LevelInfo:  slog.LevelInfo,
		LevelWarn:  slog.LevelWarn,
		LevelError: slog.LevelError,
		"ERROR":    slog.LevelError,
		"verbose":  slog.LevelInfo,
	}

	for name, want := range tests {
		assert.Equal(t, want, parseLevel(name), name)
	}
}

func TestNewHandler_Format_SelectsHandler(t *testing.T) {
	assert.IsType(t, &slog.JSONHandler{}, newHandler(LevelInfo, FormatJSON))
	assert.IsType(t, &slog.JSONHandler{}, newHandler(LevelInfo, "unknown"))
	assert.IsType(t, tint.NewTextHandler(nil, nil), newHandler(LevelInfo, FormatConsole))
}

func TestNewLogger_Level_FiltersBelowThreshold(t *testing.T) {
	logger := NewLogger(LevelWarn, FormatJSON)

	assert.False(t, logger.Enabled(t.Context(), slog.LevelInfo))
	assert.True(t, logger.Enabled(t.Context(), slog.LevelWarn))
}
