package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// SettingInt returns the integer value of a settings row, or def when the key is absent. A row that
// exists but does not parse as an integer returns def alongside an error so callers can log the bad
// value without breaking the feature it tunes (settings are presentation/policy knobs, not truth).
func (s *Store) SettingInt(ctx context.Context, key string, def int) (int, error) {
	var raw string
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("read setting %s: %w", key, err)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("setting %s is not an integer: %w", key, err)
	}
	return v, nil
}
