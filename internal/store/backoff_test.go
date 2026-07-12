package store

import (
	"testing"
	"time"
)

// TestRetryBackoffSchedule pins the scheduled-backoff curve (SPEC-0003 REQ "Bounded Retries via
// max_attempts", scheduled backoff): 30s base doubling per attempt, capped at 15m. retryBackoff is
// the Go-side spec of the schedule; FailTodo's SQL mirrors it (same base/cap constants, same
// power-of-two curve), so this table is the documented contract for both. Pure math — runs in the
// CI gate without a database.
func TestRetryBackoffSchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 15 * time.Minute},  // 16m uncapped → clamps to the 15m cap
		{7, 15 * time.Minute},  // stays at the cap
		{50, 15 * time.Minute}, // no overflow for absurd attempt counts
		{0, 30 * time.Second},  // out-of-range attempts clamp to the base
		{-3, 30 * time.Second},
	}
	for _, c := range cases {
		if got := retryBackoff(c.attempt); got != c.want {
			t.Errorf("retryBackoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

// TestRetryBackoffMonotonic guards the shape of the curve: never decreasing, never above the cap,
// never below the base — so a future constant tweak cannot accidentally schedule an instant or
// unbounded retry.
func TestRetryBackoffMonotonic(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 20; attempt++ {
		d := retryBackoff(attempt)
		if d < retryBackoffBase || d > retryBackoffCap {
			t.Errorf("retryBackoff(%d) = %v, outside [%v, %v]", attempt, d, retryBackoffBase, retryBackoffCap)
		}
		if d < prev {
			t.Errorf("retryBackoff(%d) = %v decreased from %v", attempt, d, prev)
		}
		prev = d
	}
}
