package store

// RFC 8628 device-authorization storage for the operator API (issue #308, ADR-0057). A device
// request is a pending consent with NO principal: it names the requesting client and two handles —
// the long device_code (hashed; the CLI polls with it) and the short user_code (hashed; the human
// types it at the consent screen) — plus the clocks. Approval stamps the APPROVING human, which is
// the only way a principal ever appears on the row: the table has no endpoint column, so the
// device flow structurally cannot mint an endpoint credential, only the operator-bound pair the
// code flow issues (ADR-0023). Every raw code exists only in transit; the database holds hashes,
// same as oauth_codes/oauth_tokens. Governing: ADR-0019, ADR-0023, ADR-0057 (proposed).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeviceEntryWindow bounds the user-code lookups one principal may make, and DeviceEntryMaxGuesses
// is how many fit inside it. The short code's keyspace is exhausted in hours at an unlimited rate,
// so this is what makes brute-forcing it a refusal instead of a race. It is counted per ACTOR, not
// per pending request: a wrong code hashes to no row at all, so a per-request counter would never
// see the misses, which are the entire traffic worth counting. The entry surface is session-gated,
// so every lookup has a signed-in principal to charge. Governing: issue #308 AC "user codes ...
// rate-limited on entry".
const (
	DeviceEntryWindow     = time.Minute
	DeviceEntryMaxGuesses = 10
)

// OAuthDeviceCode is one device-authorization request as its consumers see it. The clocks carry
// the whole state machine: ExpiresAt past → expired_token; DeniedAt set → access_denied;
// AuthorizedAt set and UsedAt null → the grant is ready; UsedAt set → already spent. HumanID is
// non-empty only after approval, and it is the granting principal. LastPolledAt is the PREVIOUS
// poll (the lookup stamps the current one in the same statement), which is what the slow_down
// decision reads (the lookup stamps the current arrival in the same statement).
type OAuthDeviceCode struct {
	ID              string
	ClientID        string
	HumanID         string
	ExpiresAt       time.Time
	AuthorizedAt    *time.Time
	DeniedAt        *time.Time
	UsedAt          *time.Time
	LastPolledAt    *time.Time
	IntervalSeconds int
}

// CreateOAuthDeviceCode persists a fresh device request. The caller mints both codes and hands
// over only their hashes; a hash collision on either unique index reports ErrConflict so the
// caller re-mints rather than adopting someone else's request. intervalSeconds is the polling
// interval advertised to the client (and enforced against its polls).
func (s *Store) CreateOAuthDeviceCode(ctx context.Context, deviceHash, userHash, clientID string, expiresAt time.Time, intervalSeconds int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_device_codes (device_code_hash, user_code_hash, client_id, expires_at, interval_seconds)
		VALUES ($1, $2, $3, $4, $5)`,
		deviceHash, userHash, clientID, expiresAt, intervalSeconds)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("store: create oauth device code: %w", err)
	}
	return nil
}

// OAuthDeviceCodeByDeviceHash loads one request for a poll and stamps the arrival. The value it
// returns for LastPolledAt is the PREVIOUS poll, which is what the slow_down decision compares
// against: the read and the stamp are deliberately two statements, because UPDATE ... RETURNING
// (and a data-modifying CTE beside the read) hand back the NEW value, and a handler comparing
// now() against now()+interval would answer slow_down to every honest poller after the first. Two
// polls that overlap perfectly share one previous stamp; the worst case is that both are answered
// normally, which is the clock-skew allowance the interval already carries.
// Unknown rows are ErrNotFound; every state (pending, ready, denied, expired, spent) returns the
// row and lets the clocks decide — the caller must never treat "unknown" as a different kind of
// answer than "gone", so both read as expired_token to the polling client.
func (s *Store) OAuthDeviceCodeByDeviceHash(ctx context.Context, deviceHash string) (OAuthDeviceCode, error) {
	var d OAuthDeviceCode
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, client_id, COALESCE(human_id::text, ''), expires_at,
		       authorized_at, denied_at, used_at, last_polled_at, interval_seconds
		FROM oauth_device_codes
		WHERE device_code_hash = $1`,
		deviceHash,
	).Scan(&d.ID, &d.ClientID, &d.HumanID, &d.ExpiresAt,
		&d.AuthorizedAt, &d.DeniedAt, &d.UsedAt, &d.LastPolledAt, &d.IntervalSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthDeviceCode{}, ErrNotFound
	}
	if err != nil {
		return OAuthDeviceCode{}, fmt.Errorf("store: oauth device code by device hash: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE oauth_device_codes SET last_polled_at = now() WHERE id = NULLIF($1, '')::uuid`, d.ID); err != nil {
		return OAuthDeviceCode{}, fmt.Errorf("store: stamp oauth device poll: %w", err)
	}
	return d, nil
}

// OAuthDeviceCodeByUserCode resolves the request a human typed a user code for, charging the lookup
// against the acting principal's entry window (gateKey: the session human's id, which the entry
// surface always has, since it is session-gated). Past DeviceEntryMaxGuesses inside
// DeviceEntryWindow the answer is ErrConflict whatever the code hashes to — that is the entry rate
// limit, and because it is keyed on the actor it also fires on the misses a per-request counter
// would never see. A correct code on a live request returns the row; a wrong code, an unknown code,
// and a DECIDED or EXPIRED code are uniformly ErrNotFound, so the entry page leaks nothing about
// which pending requests exist.
func (s *Store) OAuthDeviceCodeByUserCode(ctx context.Context, userHash, gateKey string) (OAuthDeviceCode, error) {
	var guesses int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO oauth_device_entry_attempts (gate_key, guess_count, guessed_at)
		VALUES ($1, 1, now())
		ON CONFLICT (gate_key) DO UPDATE SET
			guess_count = CASE WHEN oauth_device_entry_attempts.guessed_at < now() - $2::interval
			                   THEN 1 ELSE oauth_device_entry_attempts.guess_count + 1 END,
			guessed_at = now()
		RETURNING guess_count`,
		gateKey, fmt.Sprintf("%f seconds", DeviceEntryWindow.Seconds()),
	).Scan(&guesses)
	if err != nil {
		return OAuthDeviceCode{}, fmt.Errorf("store: oauth device entry gate: %w", err)
	}
	if guesses > DeviceEntryMaxGuesses {
		return OAuthDeviceCode{}, ErrConflict
	}
	var d OAuthDeviceCode
	err = s.pool.QueryRow(ctx, `
		SELECT id::text, client_id, COALESCE(human_id::text, ''), expires_at,
		       authorized_at, denied_at, used_at, NULL::timestamptz, interval_seconds
		FROM oauth_device_codes
		WHERE user_code_hash = $1
		  AND denied_at IS NULL AND authorized_at IS NULL AND expires_at > now()`,
		userHash,
	).Scan(&d.ID, &d.ClientID, &d.HumanID, &d.ExpiresAt,
		&d.AuthorizedAt, &d.DeniedAt, &d.UsedAt, &d.LastPolledAt, &d.IntervalSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthDeviceCode{}, ErrNotFound
	}
	if err != nil {
		return OAuthDeviceCode{}, fmt.Errorf("store: oauth device code by user hash: %w", err)
	}
	return d, nil
}

// ApproveOAuthDeviceCode binds one pending request to the approving human, atomically on the row:
// only a live, undecided row transitions (the WHERE is the CAS), so a consent can never be
// recorded twice or onto an expired request. used_at stays null — single-use is enforced by
// SpendOAuthDeviceCode at the moment tokens are minted, not at consent. Anything else is
// ErrNotFound.
func (s *Store) ApproveOAuthDeviceCode(ctx context.Context, id, humanID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_device_codes
		SET human_id = NULLIF($2, '')::uuid, authorized_at = now()
		WHERE id = NULLIF($1, '')::uuid AND expires_at > now()
		  AND authorized_at IS NULL AND denied_at IS NULL AND used_at IS NULL`,
		id, humanID)
	if err != nil {
		return fmt.Errorf("store: approve oauth device code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DenyOAuthDeviceCode records the human's refusal. The row stays (it is the consent record) but
// can never authorize anything again; a later poll reads access_denied. Only a live, undecided row
// transitions; anything else is ErrNotFound.
func (s *Store) DenyOAuthDeviceCode(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_device_codes SET denied_at = now()
		WHERE id = NULLIF($1, '')::uuid AND expires_at > now()
		  AND authorized_at IS NULL AND denied_at IS NULL AND used_at IS NULL`,
		id)
	if err != nil {
		return fmt.Errorf("store: deny oauth device code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SpendOAuthDeviceCode marks an approved request spent. The winning token poll spends the row
// with the same CAS shape the code flow's redemption has: only ONE concurrent poll can flip
// used_at from null, every loser reads ErrConflict and answers invalid_grant. Approval deliberately
// leaves used_at null (a grant nobody polled for is not "spent"), so single-use is enforced here,
// at the moment tokens exist.
func (s *Store) SpendOAuthDeviceCode(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_device_codes SET used_at = now()
		WHERE id = NULLIF($1, '')::uuid AND used_at IS NULL AND authorized_at IS NOT NULL`,
		id)
	if err != nil {
		return fmt.Errorf("store: spend oauth device code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}
