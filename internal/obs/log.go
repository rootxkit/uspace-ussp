package obs

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns a JSON logger writing to w at level ("debug",
// "info", "warn", "error"; anything else is info) with the process name
// on every line.
func NewLogger(w io.Writer, level, process string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(h).With(Process(process))
}

// ParseLevel maps a configuration value onto a slog level.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Discard is a logger that writes nothing, for tests.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// The structured context of every log line (CLAUDE.md engineering
// rules): one helper per key, so each key is spelled once.

// Process is the process attribute.
func Process(name string) slog.Attr { return slog.String("process", name) }

// RequestID is the request id attribute.
func RequestID(id string) slog.Attr { return slog.String("request_id", id) }

// DroneID is the drone (UAS serial) attribute.
func DroneID(id string) slog.Attr { return slog.String("drone_id", id) }

// FlightID is the flight attribute.
func FlightID(id string) slog.Attr { return slog.String("flight_id", id) }

// IntentID is the operational intent attribute.
func IntentID(id string) slog.Attr { return slog.String("intent_id", id) }

// ClientID is the OAuth client attribute.
func ClientID(id string) slog.Attr { return slog.String("client_id", id) }

// Dependency is the dependency attribute (postgres, timescaledb, nats).
func Dependency(name string) slog.Attr { return slog.String("dep", name) }

// Err is the error attribute.
func Err(err error) slog.Attr { return slog.String("error", err.Error()) }

// Error logs msg at error level with err.
func Error(ctx context.Context, l *slog.Logger, msg string, err error, attrs ...slog.Attr) {
	l.LogAttrs(ctx, slog.LevelError, msg, append([]slog.Attr{Err(err)}, attrs...)...)
}
