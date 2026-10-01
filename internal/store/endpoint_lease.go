package store

// Endpoint Default Lease
//
// The lease a claim or heartbeat on an endpoint gets when the call names no lease_ttl_seconds.
// NULL is the server default (lease.DefaultTTL). The setting is not scope (SPEC-0007): its human
// edits it in place, and so may the endpoint itself when its scope grants set_default_lease. Two
// write paths, one per principal:
//
//   - SetEndpointDefaultLease is keyed by the endpoint id alone, because its one caller, the MCP
//     set_default_lease verb, passes the AUTHENTICATED endpoint's own id: an endpoint can only ever
//     change its own default.
//   - SetEndpointDefaultLeaseForHuman binds ownership in the same UPDATE, for the human API and the
//     web UI. Another human's endpoint is ErrNotFound, exactly like one that does not exist.
//
// Both refuse a revoked endpoint: its credential is dead, so the setting can no longer apply to
// anything. An edit changes claims and heartbeats made after it; a lease already granted keeps its
// expiry, because nothing here touches todos.
//
// Governing: ADR-0043, SPEC-0007 REQ "Endpoint Default Lease", SPEC-0006 REQ "Lease Lifecycle and
// Crash Safety", ADR-0022 (the endpoint's human is the authorization principal).
//
// @joestump-agent 10/01/2026 - Added with migration 0029.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/lease"
)

// EndpointDefaultLease returns the endpoint's default lease in seconds, or nil for the server
// default. The lease verbs call it with the authenticated endpoint's own id on every claim and
// heartbeat that names no lease_ttl_seconds, so an edit applies from the next call on, live
// sessions included. An unknown id is ErrNotFound.
func (s *Store) EndpointDefaultLease(ctx context.Context, endpointID string) (*int, error) {
	var seconds *int
	err := s.pool.QueryRow(ctx,
		`SELECT default_lease_ttl_seconds FROM endpoints WHERE id = $1`, endpointID,
	).Scan(&seconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: endpoint default lease: %w", err)
	}
	return seconds, nil
}

// SetEndpointDefaultLease sets an ACTIVE endpoint's default lease (nil resets it to the server
// default) and returns the stored value. It is keyed by the endpoint id alone; its caller passes the
// authenticated endpoint's own id. An out-of-range value is lease.ErrOutOfRange and changes nothing;
// an unknown or revoked endpoint is ErrNotFound.
func (s *Store) SetEndpointDefaultLease(ctx context.Context, endpointID string, seconds *int) (*int, error) {
	if seconds != nil {
		if err := lease.ValidateDefault(*seconds); err != nil {
			return nil, err
		}
	}
	var stored *int
	err := s.pool.QueryRow(ctx, `
		UPDATE endpoints SET default_lease_ttl_seconds = $2
		WHERE id = $1 AND state = 'active'
		RETURNING default_lease_ttl_seconds`, endpointID, seconds,
	).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: set endpoint default lease: %w", err)
	}
	return stored, nil
}

// SetEndpointDefaultLeaseForHuman sets the default lease of an endpoint the human owns, with the
// ownership predicate in the UPDATE itself, and returns the stored value. An out-of-range value is
// lease.ErrOutOfRange; an endpoint the human does not own, or that does not exist, is ErrNotFound;
// one they own that is revoked is ErrConflict, which the human API answers 409 like every other
// write to a revoked endpoint (SPEC-0035 REQ "Reach on Every Route").
func (s *Store) SetEndpointDefaultLeaseForHuman(ctx context.Context, endpointID, ownerHumanID string, seconds *int) (*int, error) {
	if seconds != nil {
		if err := lease.ValidateDefault(*seconds); err != nil {
			return nil, err
		}
	}
	var stored *int
	err := s.pool.QueryRow(ctx, `
		UPDATE endpoints SET default_lease_ttl_seconds = $3
		WHERE id = $1 AND state = 'active'
		  AND agent_id IN (SELECT id FROM agents WHERE owner_human_id = $2)
		RETURNING default_lease_ttl_seconds`, endpointID, ownerHumanID, seconds,
	).Scan(&stored)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: set endpoint default lease for human: %w", err)
	}
	// Nothing updated: tell "yours, but revoked" apart from "not yours or not there", under the same
	// ownership predicate, so a probe of another human's endpoint learns nothing.
	var state string
	err = s.pool.QueryRow(ctx, `
		SELECT e.state FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE e.id = $1 AND ag.owner_human_id = $2`, endpointID, ownerHumanID,
	).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: set endpoint default lease for human: %w", err)
	}
	return nil, ErrConflict
}
