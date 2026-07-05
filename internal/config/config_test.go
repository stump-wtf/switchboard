package config

import (
	"os"
	"testing"
)

func TestFromEnvDefaults(t *testing.T) {
	os.Unsetenv("SWITCHBOARD_ADDR")
	os.Unsetenv("SWITCHBOARD_DATABASE_URL")
	cfg := FromEnv()
	if cfg.Addr != "127.0.0.1:8080" {
		t.Fatalf("default Addr: got %q, want 127.0.0.1:8080", cfg.Addr)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("default DatabaseURL: got %q, want empty", cfg.DatabaseURL)
	}
}

func TestFromEnvOverride(t *testing.T) {
	t.Setenv("SWITCHBOARD_ADDR", "0.0.0.0:9000")
	if got := FromEnv().Addr; got != "0.0.0.0:9000" {
		t.Fatalf("override Addr: got %q, want 0.0.0.0:9000", got)
	}
}
