package config

import (
	"os"
	"testing"
)

func TestFromEnvDefaults(t *testing.T) {
	_ = os.Unsetenv("SWITCHBOARD_ADDR")
	_ = os.Unsetenv("SWITCHBOARD_DATABASE_URL")
	_ = os.Unsetenv("SWITCHBOARD_REDIS_URL")
	cfg := FromEnv()
	if cfg.Addr != "127.0.0.1:8080" {
		t.Fatalf("default Addr: got %q, want 127.0.0.1:8080", cfg.Addr)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("default DatabaseURL: got %q, want empty", cfg.DatabaseURL)
	}
	if cfg.RedisURL != "" {
		t.Fatalf("default RedisURL: got %q, want empty (Redis adapters disabled)", cfg.RedisURL)
	}
}

// SPEC-0002: the Redis DSN for the pull-adapter family comes from SWITCHBOARD_REDIS_URL.
func TestFromEnvRedisURL(t *testing.T) {
	t.Setenv("SWITCHBOARD_REDIS_URL", "redis://:secret@127.0.0.1:6379/1")
	if got := FromEnv().RedisURL; got != "redis://:secret@127.0.0.1:6379/1" {
		t.Fatalf("RedisURL: got %q, want the env DSN", got)
	}
}

func TestFromEnvOverride(t *testing.T) {
	t.Setenv("SWITCHBOARD_ADDR", "0.0.0.0:9000")
	if got := FromEnv().Addr; got != "0.0.0.0:9000" {
		t.Fatalf("override Addr: got %q, want 0.0.0.0:9000", got)
	}
}
