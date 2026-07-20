package store

// A2A PushNotificationConfig storage: a caller-registered webhook switchboard POSTs task updates to
// over authenticated HTTP. This file lands the durable storage only — the CRUD tool/HTTP handlers, the
// SSRF-guarded delivery dispatcher, and the retry/sequence machinery arrive in the follow-up push
// stories. The webhook target URL is validated by the shared internal/push SSRF guard at the layer
// above before CreatePushConfig is ever called (and re-validated immediately before each delivery
// attempt); this store deliberately does no network work. task_id is the todo id (todos.id is text);
// the ON DELETE CASCADE FK means a config never outlives the work it notifies for.
//
// Governing: ADR-0021 (A2A task-delegation transport),
// SPEC-0019 REQ "PushNotificationConfig CRUD",
// SPEC-0019 REQ "Webhook Target Validation (SSRF Guard)".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PushConfig is one registered A2A push-notification webhook for a task. id is server-assigned;
// TaskID is the todo id. Token and Authentication are optional: Token is a caller-supplied value
// echoed back on delivery so the caller can correlate/verify, and Authentication is the descriptor
// switchboard applies (e.g. bearer, API key) when calling the webhook. Authentication is stored as
// raw JSON (jsonb) and carried through untouched; the delivery story interprets it.
type PushConfig struct {
	ID             string
	TaskID         string
	URL            string
	Token          *string // nil when the caller supplied none
	Authentication []byte  // raw JSON descriptor; nil when the caller supplied none
	CreatedAt      time.Time
}

// CreatePushConfig persists a validated push-notification config for a task and returns the stored
// row (with its server-assigned id and created_at). The caller MUST have already run the SSRF-guard
// validator (internal/push) against url — this store does not validate the URL, matching the layering
// where the tool/HTTP handler owns request validation. token == "" persists NULL (no correlation
// token); auth == nil persists NULL (no auth descriptor). A task_id that references no todo violates
// the FK and surfaces as ErrNotFound so the caller can map it to the A2A "task not found" shape rather
// than leaking a raw constraint error. Governing: SPEC-0019 REQ "PushNotificationConfig CRUD".
func (s *Store) CreatePushConfig(ctx context.Context, taskID, url, token string, auth []byte) (PushConfig, error) {
	var c PushConfig
	err := s.pool.QueryRow(ctx, `
		INSERT INTO push_notification_configs (task_id, url, token, authentication)
		VALUES ($1, $2, NULLIF($3, ''), $4)
		RETURNING id::text, task_id, url, token, authentication, created_at`,
		taskID, url, token, nullableJSON(auth),
	).Scan(&c.ID, &c.TaskID, &c.URL, &c.Token, &c.Authentication, &c.CreatedAt)
	if err != nil {
		// 23503 = foreign_key_violation: the referenced todo does not exist.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return PushConfig{}, ErrNotFound
		}
		return PushConfig{}, fmt.Errorf("store: create push config: %w", err)
	}
	return c, nil
}

// GetPushConfig returns a single config by id scoped to its task: the task_id guard is the scoping
// check so a caller cannot read a config for a task outside the id/task pair it presents. An id that
// matches no row (or a mismatched task) returns ErrNotFound with no detail leaked.
// Governing: SPEC-0019 REQ "PushNotificationConfig CRUD" (scoped to the caller's own tasks).
func (s *Store) GetPushConfig(ctx context.Context, id, taskID string) (PushConfig, error) {
	var c PushConfig
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, task_id, url, token, authentication, created_at
		FROM push_notification_configs WHERE id = $1 AND task_id = $2`,
		id, taskID,
	).Scan(&c.ID, &c.TaskID, &c.URL, &c.Token, &c.Authentication, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PushConfig{}, ErrNotFound
	}
	if err != nil {
		return PushConfig{}, fmt.Errorf("store: get push config: %w", err)
	}
	return c, nil
}

// ListPushConfigs returns a task's registered push configs, newest first. This is both the
// ListTaskPushNotificationConfigs read and the delivery-dispatch read (the follow-up delivery story
// re-validates each URL before dialing). Governing: SPEC-0019 REQ "PushNotificationConfig CRUD".
func (s *Store) ListPushConfigs(ctx context.Context, taskID string) ([]PushConfig, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, task_id, url, token, authentication, created_at
		FROM push_notification_configs WHERE task_id = $1 ORDER BY created_at DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list push configs: %w", err)
	}
	defer rows.Close()
	var out []PushConfig
	for rows.Next() {
		var c PushConfig
		if err := rows.Scan(&c.ID, &c.TaskID, &c.URL, &c.Token, &c.Authentication, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list push configs scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeletePushConfig tears down a config scoped to its task, and is idempotent: deleting an id that
// matches no row (already deleted, or never existed) returns nil, not ErrNotFound — matching
// DeleteTaskPushNotificationConfig's required idempotency. The task_id guard is the scoping check.
// Governing: SPEC-0019 REQ "PushNotificationConfig CRUD" (Delete is idempotent).
func (s *Store) DeletePushConfig(ctx context.Context, id, taskID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM push_notification_configs WHERE id = $1 AND task_id = $2`, id, taskID)
	if err != nil {
		return fmt.Errorf("store: delete push config: %w", err)
	}
	return nil
}

// nullableJSON returns b for a non-empty JSON payload and nil otherwise, so an absent auth descriptor
// persists SQL NULL rather than an empty-bytes value pgx would reject as invalid jsonb.
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
