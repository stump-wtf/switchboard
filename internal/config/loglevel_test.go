package config

import (
	"log/slog"
	"testing"
)

// The knob exists so a deployed instance can reach the Debug lines on the doorbell path. A typo in
// it must never stop the service booting, so anything unrecognised is info.
func TestLogLevel(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  Debug  ", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
		{"3", slog.LevelInfo},
	} {
		t.Setenv("SWITCHBOARD_LOG_LEVEL", tc.env)
		if got := LogLevel(); got != tc.want {
			t.Errorf("SWITCHBOARD_LOG_LEVEL=%q -> %v, want %v", tc.env, got, tc.want)
		}
	}
}
