package observability

import (
	"log/slog"
	"os"
	"strings"

	"github.com/lmittmann/tint"
)

const ComponentLabel = "component"

const (
	FormatJSON    = "json"
	FormatConsole = "console"

	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

func NewLogger(level, format string) *slog.Logger {
	return slog.New(newHandler(level, format))
}

func newHandler(level, format string) slog.Handler {
	slogLevel := parseLevel(level)

	if strings.EqualFold(format, FormatConsole) {
		return tint.NewTextHandler(os.Stdout, &tint.Options{Level: slogLevel})
	}

	return slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel})
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
