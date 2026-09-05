package config

// Log level
//
// One knob, read from the environment like every other setting. It exists because the doorbell's
// delivered/dropped outcome is logged at Debug, and without a way to reach Debug in a deployed
// instance that half of the push path is permanently unobservable — an agent can be attached,
// healthy by every other signal, and silently receiving nothing.
//
// @joestump-agent 09/05/2026 - Added after a deaf agent took hours to diagnose for want of one
// log line.

import (
	"log/slog"
	"os"
	"strings"
)

// LogLevel resolves SWITCHBOARD_LOG_LEVEL to a slog level, defaulting to info. An unrecognised
// value falls back to info rather than failing startup: a typo in a log knob must never be the
// reason a service will not boot.
func LogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SWITCHBOARD_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
